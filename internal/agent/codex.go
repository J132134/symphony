// Package agent implements the Codex/Claude Code subprocess runner over JSON-RPC stdio.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"symphony/internal/types"
)

// DynamicToolSpec describes a dynamic tool exposed to the agent.
type DynamicToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// DynamicToolHandler handles dynamic tool invocations from the agent.
type DynamicToolHandler func(ctx context.Context, toolName string, input map[string]any) (map[string]any, error)

// Config is passed to the runner per session.
type Config struct {
	Command                string
	ApprovalPolicy         string
	MaxTurns               int
	TurnTimeoutMs          int
	ReadTimeoutMs          int
	ThreadStartTimeoutMs   int
	StallTimeoutMs         int
	TurnSandboxPolicy      string
	ThreadSandbox          string
	AdditionalWritableDirs []string
	DynamicTools           []DynamicToolSpec
}

// TokenUsage is an alias for types.TokenUsage.
type TokenUsage = types.TokenUsage

type RateLimitEvent struct {
	ResetAt *time.Time
}

// Event is emitted by the runner to the orchestrator callback.
type Event struct {
	Name      string
	Timestamp time.Time
	SessionID string
	ThreadID  string
	TurnID    string
	PID       string
	Usage     *TokenUsage
	RateLimit *RateLimitEvent
	Message   string
}

// TurnResult is the outcome of a single turn.
type TurnResult struct {
	Success            bool
	Error              string
	CompletedNaturally bool
}

// EventCallback receives events from the runner.
type EventCallback func(Event)

type rpcResult struct {
	result map[string]any
	err    error
}

// Runner manages the Codex/Claude subprocess lifecycle.
type Runner struct {
	cmd       *exec.Cmd
	stdin     *lockedWriter
	sessionID string
	threadID  string
	pid       string

	mu          sync.Mutex
	pending     map[int]chan rpcResult
	reqID       atomic.Int32
	toolHandler DynamicToolHandler
	currentTurn struct {
		callback EventCallback
		threadID string
		turnID   string
	}

	notifCh         chan *Incoming
	priorityNotifCh chan *Incoming

	// cumulative token counts for delta computation
	lastInput  int64
	lastOutput int64
	lastTotal  int64
}

// SetToolHandler registers a handler for dynamic tool invocations.
func (r *Runner) SetToolHandler(h DynamicToolHandler) {
	r.mu.Lock()
	r.toolHandler = h
	r.mu.Unlock()
}

func NewRunner() *Runner {
	return &Runner{
		pending: make(map[int]chan rpcResult),
		notifCh: make(chan *Incoming, 512),
		// Turn terminal notifications must survive general queue saturation.
		priorityNotifCh: make(chan *Incoming, 16),
	}
}

func (r *Runner) PID() string       { return r.pid }
func (r *Runner) SessionID() string { return r.sessionID }
func (r *Runner) ThreadID() string  { return r.threadID }

// StartSession launches the agent process and performs the JSON-RPC handshake.
// Returns the thread ID to use for turns.
func (r *Runner) StartSession(ctx context.Context, workspacePath string, cfg *Config) (string, error) {
	r.sessionID = fmt.Sprintf("%d", time.Now().UnixNano())
	r.lastInput, r.lastOutput, r.lastTotal = 0, 0, 0

	launchCmd := buildLaunchCommand(cfg.Command, cfg.AdditionalWritableDirs)
	slog.Info("agent.launch", "command", launchCmd, "session", r.sessionID)

	cmd := exec.Command("bash", "-lc", launchCmd)
	cmd.Dir = workspacePath
	cmd.Env = append(os.Environ(), "CODEX_APPROVAL_POLICY="+cfg.ApprovalPolicy)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	inPipe, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("stdin pipe: %w", err)
	}
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("stdout pipe: %w", err)
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start: %w", err)
	}

	r.cmd = cmd
	r.stdin = &lockedWriter{w: inPipe}
	if cmd.Process != nil {
		r.pid = fmt.Sprintf("%d", cmd.Process.Pid)
	}

	go r.readStdout(bufio.NewReader(outPipe))
	go r.readStderr(bufio.NewReader(errPipe))

	readTimeout := time.Duration(cfg.ReadTimeoutMs) * time.Millisecond

	// initialize
	capabilities := map[string]any{}
	if len(cfg.DynamicTools) > 0 {
		capabilities["experimentalApi"] = true
	}
	initRes, err := r.sendRequest(ctx, readTimeout, methodInitialize, map[string]any{
		"protocolVersion": "2025-01-01",
		"capabilities":    capabilities,
		"clientInfo":      map[string]any{"name": "symphony", "version": "0.2.0"},
	})
	if err != nil {
		return "", fmt.Errorf("initialize: %w", err)
	}
	slog.Info("agent.initialized", "serverInfo", initRes["serverInfo"])

	// initialized notification
	if err := r.sendNotification(methodInitialized, nil); err != nil {
		return "", fmt.Errorf("initialized: %w", err)
	}

	// thread/start
	threadParams := map[string]any{
		"approvalPolicy": cfg.ApprovalPolicy,
		"cwd":            workspacePath,
	}
	if sb := resolveThreadSandbox(cfg); sb != "" {
		threadParams["sandbox"] = sb
	}
	if len(cfg.DynamicTools) > 0 {
		threadParams["dynamicTools"] = cfg.DynamicTools
	}

	threadStartTimeout := time.Duration(cfg.ThreadStartTimeoutMs) * time.Millisecond
	threadRes, err := r.sendRequest(ctx, threadStartTimeout, methodThreadStart, threadParams)
	if err != nil {
		return "", fmt.Errorf("thread/start: %w", err)
	}

	thread, _ := threadRes["thread"].(map[string]any)
	threadID, _ := thread["id"].(string)
	if threadID == "" {
		return "", fmt.Errorf("thread/start: missing thread.id")
	}
	r.threadID = threadID
	slog.Info("agent.thread_created", "thread_id", threadID)
	return threadID, nil
}

// RunTurn sends turn/start and streams events until the turn completes.
// issueTitle is used only for the turn title field in the protocol.
func (r *Runner) RunTurn(
	ctx context.Context,
	threadID, turnID, prompt, issueIdentifier, issueTitle string,
	cfg *Config,
	cb EventCallback,
) TurnResult {
	if r.cmd == nil || r.cmd.ProcessState != nil {
		return TurnResult{Error: "process_not_running"}
	}

	emit(cb, Event{
		Name: "turn_started", Timestamp: time.Now().UTC(),
		SessionID: r.sessionID, ThreadID: threadID, TurnID: turnID, PID: r.pid,
	})

	// Drain stale notifications from previous turns.
	r.drainNotifications()

	turnParams := map[string]any{
		"threadId":       threadID,
		"input":          []any{map[string]any{"type": "text", "text": prompt}},
		"cwd":            r.cmd.Dir,
		"title":          fmt.Sprintf("%s: %s", issueIdentifier, issueTitle),
		"approvalPolicy": cfg.ApprovalPolicy,
	}
	if policy := buildTurnSandboxPolicy(cfg); policy != nil {
		turnParams["sandboxPolicy"] = policy
	}

	readTimeout := time.Duration(cfg.ReadTimeoutMs) * time.Millisecond
	if _, err := r.sendRequest(ctx, readTimeout, methodTurnStart, turnParams); err != nil {
		return TurnResult{Error: fmt.Sprintf("turn/start: %v", err)}
	}

	turnTimeout := time.Duration(cfg.TurnTimeoutMs) * time.Millisecond
	tctx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()
	r.setCurrentTurnCallback(threadID, turnID, cb)
	defer r.clearCurrentTurnCallback(turnID)

	return r.consumeUntilDone(tctx, threadID, turnID, cb)
}

func (r *Runner) InterruptTurn(ctx context.Context, threadID, turnID string, cfg *Config) error {
	if r.cmd == nil || r.cmd.Process == nil {
		return nil
	}
	if strings.TrimSpace(threadID) == "" || strings.TrimSpace(turnID) == "" {
		return nil
	}
	readTimeout := time.Duration(cfg.ReadTimeoutMs) * time.Millisecond
	_, err := r.sendRequest(ctx, readTimeout, methodTurnInterrupt, map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
	})
	return err
}

func (r *Runner) consumeUntilDone(ctx context.Context, threadID, turnID string, cb EventCallback) TurnResult {
	for {
		msg, err := r.nextNotification(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				emit(cb, Event{Name: "turn_failed", Timestamp: time.Now().UTC(),
					SessionID: r.sessionID, ThreadID: threadID, TurnID: turnID, Message: "turn_timeout"})
				return TurnResult{Error: "turn_timeout"}
			}
			return TurnResult{Error: "channel_closed"}
		}

		if msg.ServerReq != nil {
			r.handleServerRequest(threadID, turnID, msg.ServerReq, cb)
			continue
		}
		if msg.Notif == nil {
			continue
		}

		method := msg.Notif.Method
		params := msg.Notif.Params
		if params == nil {
			params = map[string]any{}
		}

		switch method {
		case methodTurnCompleted:
			emit(cb, Event{Name: "turn_completed", Timestamp: time.Now().UTC(),
				SessionID: r.sessionID, ThreadID: threadID, TurnID: turnID})
			return TurnResult{Success: true, CompletedNaturally: true}

		case methodTurnFailed:
			errMsg, _ := params["error"].(string)
			if errMsg == "" {
				errMsg = "unknown_error"
			}
			emit(cb, Event{Name: "turn_failed", Timestamp: time.Now().UTC(),
				SessionID: r.sessionID, ThreadID: threadID, TurnID: turnID, Message: errMsg})
			return TurnResult{Error: errMsg, CompletedNaturally: true}

		case methodTurnCancelled:
			emit(cb, Event{Name: "turn_cancelled", Timestamp: time.Now().UTC(),
				SessionID: r.sessionID, ThreadID: threadID, TurnID: turnID})
			return TurnResult{Error: "cancelled"}

		case methodTokenUsage:
			r.handleTokenUsage(threadID, turnID, params, cb)

		case methodRateLimits:
			rateLimit := parseRateLimitEvent(params, time.Now().UTC())
			emit(cb, Event{Name: "rate_limit", Timestamp: time.Now().UTC(),
				SessionID: r.sessionID, ThreadID: threadID, TurnID: turnID, RateLimit: rateLimit})

		default:
			emit(cb, Event{
				Name:      "server_notification",
				Timestamp: time.Now().UTC(),
				SessionID: r.sessionID,
				ThreadID:  threadID,
				TurnID:    turnID,
				PID:       r.pid,
				Message:   summarizeServerMessage(method, params),
			})
		}
	}
}

func (r *Runner) handleTokenUsage(threadID, turnID string, params map[string]any, cb EventCallback) {
	newIn := toInt64(params["inputTokens"])
	newOut := toInt64(params["outputTokens"])
	newTotal := toInt64(params["totalTokens"])
	if newTotal == 0 {
		newTotal = newIn + newOut
	}
	usage := &TokenUsage{
		InputTokens:  newIn - r.lastInput,
		OutputTokens: newOut - r.lastOutput,
		TotalTokens:  newTotal - r.lastTotal,
	}
	r.lastInput, r.lastOutput, r.lastTotal = newIn, newOut, newTotal
	emit(cb, Event{
		Name: "token_usage", Timestamp: time.Now().UTC(),
		SessionID: r.sessionID, ThreadID: threadID, TurnID: turnID, Usage: usage,
	})
}

func (r *Runner) handleServerRequest(threadID, turnID string, req *Request, cb EventCallback) {
	switch req.Method {
	case methodCmdApproval, methodFileApproval:
		emit(cb, Event{
			Name:      "approval_request",
			Timestamp: time.Now().UTC(),
			SessionID: r.sessionID,
			ThreadID:  threadID,
			TurnID:    turnID,
			PID:       r.pid,
			Message:   summarizeServerRequest(req.Method, req.Params),
		})
		slog.Debug("agent.auto_approve", "method", req.Method)
		data, _ := FormatResponse(req.ID, map[string]any{"approved": true})
		_ = r.stdin.Write(data)
		emit(cb, Event{Name: "approval_granted", Timestamp: time.Now().UTC(),
			SessionID: r.sessionID, ThreadID: threadID, TurnID: turnID, PID: r.pid, Message: summarizeServerRequest(req.Method, req.Params)})
	case methodToolCall:
		r.mu.Lock()
		handler := r.toolHandler
		r.mu.Unlock()
		toolName, input := extractDynamicToolRequest(req.Params)
		emit(cb, Event{
			Name:      "tool_call",
			Timestamp: time.Now().UTC(),
			SessionID: r.sessionID,
			ThreadID:  threadID,
			TurnID:    turnID,
			PID:       r.pid,
			Message:   summarizeToolCall(toolName, input),
		})
		if handler != nil && toolName != "" {
			go r.handleDynamicTool(req.ID, toolName, input)
			return
		}
		slog.Warn("agent.tool_call_unsupported", "tool", toolName, "params", summarizeParams(req.Params))
		data, _ := FormatErrorResponse(req.ID, -32601, "Tool call not supported")
		_ = r.stdin.Write(data)
	case methodUserInput:
		if response, ok := buildAutoUserInputResponse(req.Params); ok {
			slog.Info("agent.auto_approve_user_input", "params", summarizeParams(req.Params))
			data, _ := FormatResponse(req.ID, response)
			_ = r.stdin.Write(data)
			emit(cb, Event{Name: "approval_granted", Timestamp: time.Now().UTC(),
				SessionID: r.sessionID, Message: req.Method})
			return
		}
		r.mu.Lock()
		handler := r.toolHandler
		r.mu.Unlock()
		toolName, input := extractDynamicToolRequest(req.Params)
		emit(cb, Event{
			Name:      "user_input_request",
			Timestamp: time.Now().UTC(),
			SessionID: r.sessionID,
			ThreadID:  threadID,
			TurnID:    turnID,
			PID:       r.pid,
			Message:   summarizeToolCall(toolName, input),
		})
		if handler != nil && toolName != "" {
			go r.handleDynamicTool(req.ID, toolName, input)
			return
		}
		slog.Warn("agent.user_input_unsupported", "params", summarizeParams(req.Params))
		data, _ := FormatErrorResponse(req.ID, -32601, "User input not supported")
		_ = r.stdin.Write(data)
	default:
		emit(cb, Event{
			Name:      "server_request",
			Timestamp: time.Now().UTC(),
			SessionID: r.sessionID,
			ThreadID:  threadID,
			TurnID:    turnID,
			PID:       r.pid,
			Message:   summarizeServerRequest(req.Method, req.Params),
		})
		slog.Warn("agent.unknown_server_request", "method", req.Method)
		data, _ := FormatErrorResponse(req.ID, -32601, "Unsupported: "+req.Method)
		_ = r.stdin.Write(data)
	}
}

func (r *Runner) handleDynamicTool(reqID any, toolName string, input map[string]any) {
	r.mu.Lock()
	handler := r.toolHandler
	r.mu.Unlock()
	if handler == nil {
		data, _ := FormatErrorResponse(reqID, -32601, "No tool handler registered")
		_ = r.stdin.Write(data)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := handler(ctx, toolName, input)
	if err != nil {
		slog.Warn("agent.dynamic_tool_error", "tool", toolName, "error", err)
		data, _ := FormatErrorResponse(reqID, -32603, err.Error())
		_ = r.stdin.Write(data)
		return
	}

	var text string
	if result != nil {
		b, _ := json.Marshal(result)
		text = string(b)
	}
	data, _ := FormatResponse(reqID, map[string]any{
		"contentItems": []any{
			map[string]any{"type": "text", "text": text},
		},
	})
	_ = r.stdin.Write(data)
	slog.Debug("agent.dynamic_tool_done", "tool", toolName)
}

func extractDynamicToolRequest(params map[string]any) (string, map[string]any) {
	if len(params) == 0 {
		return "", nil
	}

	candidates := []map[string]any{params}
	for _, key := range []string{"toolCall", "tool", "item", "call", "payload"} {
		if nested, ok := params[key].(map[string]any); ok {
			candidates = append(candidates, nested)
		}
	}

	for idx, candidate := range candidates {
		toolName := firstNonEmptyString(candidate["name"], candidate["toolName"], candidate["tool"])
		if toolName == "" {
			continue
		}
		if input := extractDynamicToolInput(candidate); input != nil {
			return toolName, input
		}
		if idx > 0 {
			if input := extractDynamicToolInput(params); input != nil {
				return toolName, input
			}
		}
		return toolName, map[string]any{}
	}

	return "", nil
}

func buildAutoUserInputResponse(params map[string]any) (map[string]any, bool) {
	rawQuestions, ok := params["questions"].([]any)
	if !ok || len(rawQuestions) == 0 {
		return nil, false
	}

	answers := make(map[string]any, len(rawQuestions))
	for _, raw := range rawQuestions {
		question, ok := raw.(map[string]any)
		if !ok {
			return nil, false
		}
		questionID := firstNonEmptyString(question["id"])
		answer, ok := selectAutoUserInputAnswer(question)
		if questionID == "" || !ok {
			return nil, false
		}
		answers[questionID] = answer
	}
	if len(answers) == 0 {
		return nil, false
	}
	return map[string]any{"answers": answers}, true
}

func selectAutoUserInputAnswer(question map[string]any) (string, bool) {
	options, ok := question["options"].([]any)
	if !ok || len(options) == 0 {
		return "", false
	}
	if !looksLikeApprovalQuestion(question, options) {
		return "", false
	}

	labels := make([]string, 0, len(options))
	for _, raw := range options {
		option, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		label := firstNonEmptyString(option["label"])
		if label == "" {
			continue
		}
		labels = append(labels, label)
	}
	if len(labels) == 0 {
		return "", false
	}

	for _, preferred := range []string{"Approve Once", "Approve this session"} {
		for _, label := range labels {
			if strings.EqualFold(label, preferred) {
				return label, true
			}
		}
	}
	for _, label := range labels {
		lower := strings.ToLower(label)
		if strings.Contains(lower, "cancel") || strings.Contains(lower, "deny") || strings.Contains(lower, "reject") {
			continue
		}
		return label, true
	}
	return "", false
}

func looksLikeApprovalQuestion(question map[string]any, options []any) bool {
	text := strings.ToLower(firstNonEmptyString(question["header"], question["question"]))
	if strings.Contains(text, "approve") {
		return true
	}
	for _, raw := range options {
		option, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		label := strings.ToLower(firstNonEmptyString(option["label"], option["description"]))
		if strings.Contains(label, "approve") || strings.Contains(label, "run the tool") {
			return true
		}
	}
	return false
}

func extractDynamicToolInput(params map[string]any) map[string]any {
	if len(params) == 0 {
		return nil
	}
	for _, key := range []string{"input", "arguments"} {
		if input := coerceToolInput(params[key]); input != nil {
			return input
		}
	}
	return nil
}

func coerceToolInput(v any) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		return t
	case string:
		trimmed := strings.TrimSpace(t)
		if trimmed == "" {
			return nil
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(trimmed), &out); err == nil {
			return out
		}
	}
	return nil
}

func firstNonEmptyString(values ...any) string {
	for _, value := range values {
		if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func (r *Runner) setCurrentTurnCallback(threadID, turnID string, cb EventCallback) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.currentTurn.callback = cb
	r.currentTurn.threadID = threadID
	r.currentTurn.turnID = turnID
}

func (r *Runner) clearCurrentTurnCallback(turnID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.currentTurn.turnID != turnID {
		return
	}
	r.currentTurn = struct {
		callback EventCallback
		threadID string
		turnID   string
	}{}
}

func (r *Runner) emitCurrentTurnEvent(name, message string) {
	r.mu.Lock()
	cb := r.currentTurn.callback
	threadID := r.currentTurn.threadID
	turnID := r.currentTurn.turnID
	r.mu.Unlock()
	if cb == nil {
		return
	}
	emit(cb, Event{
		Name:      name,
		Timestamp: time.Now().UTC(),
		SessionID: r.sessionID,
		ThreadID:  threadID,
		TurnID:    turnID,
		PID:       r.pid,
		Message:   message,
	})
}

func summarizeParams(params map[string]any) string {
	if len(params) == 0 {
		return ""
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Sprintf("keys=%v", sortedKeys(params))
	}
	const limit = 500
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + "...(truncated)"
}

func summarizeToolCall(toolName string, input map[string]any) string {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		toolName = "unknown tool"
	}
	if detail := summarizeStatusPayload(input); detail != "" {
		return limitStatusText(toolName+" "+detail, 200)
	}
	return toolName
}

func summarizeServerRequest(method string, params map[string]any) string {
	label := humanizeRPCMethod(method)
	if detail := summarizeStatusPayload(params); detail != "" {
		return limitStatusText(label+": "+detail, 200)
	}
	return label
}

func summarizeServerMessage(method string, params map[string]any) string {
	label := humanizeRPCMethod(method)
	if detail := summarizeStatusPayload(params); detail != "" {
		return limitStatusText(label+": "+detail, 200)
	}
	return label
}

func summarizeStatusPayload(params map[string]any) string {
	if len(params) == 0 {
		return ""
	}
	filtered := filterStatusPayload(params, 0)
	raw, err := json.Marshal(filtered)
	if err != nil {
		return summarizeParams(params)
	}
	text := strings.TrimSpace(string(raw))
	text = strings.ReplaceAll(text, "\\n", " ")
	return limitStatusText(text, 160)
}

func filterStatusPayload(v any, depth int) any {
	if depth >= 2 {
		switch value := v.(type) {
		case string:
			return limitStatusText(strings.Join(strings.Fields(value), " "), 80)
		default:
			return "..."
		}
	}

	switch value := v.(type) {
	case map[string]any:
		keys := sortedKeys(value)
		out := make(map[string]any)
		for _, key := range keys {
			if !isInterestingStatusKey(key) {
				continue
			}
			out[key] = filterStatusPayload(value[key], depth+1)
			if len(out) >= 4 {
				return out
			}
		}
		if len(out) > 0 {
			return out
		}
		for _, key := range keys {
			if _, ok := out[key]; ok {
				continue
			}
			out[key] = filterStatusPayload(value[key], depth+1)
			if len(out) >= 4 {
				break
			}
		}
		return out
	case []any:
		limit := min(len(value), 2)
		out := make([]any, 0, limit+1)
		for i := 0; i < limit; i++ {
			out = append(out, filterStatusPayload(value[i], depth+1))
		}
		if len(value) > limit {
			out = append(out, fmt.Sprintf("...+%d more", len(value)-limit))
		}
		return out
	case string:
		return limitStatusText(strings.Join(strings.Fields(value), " "), 80)
	default:
		return value
	}
}

func isInterestingStatusKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "name", "toolname", "input", "arguments", "path", "command", "cmd", "query", "identifier", "title", "text", "reason", "status":
		return true
	default:
		return false
	}
}

func humanizeRPCMethod(method string) string {
	method = strings.Trim(strings.TrimSpace(method), "/")
	if method == "" {
		return "server message"
	}
	method = strings.ReplaceAll(method, "/", " ")
	method = strings.ReplaceAll(method, "_", " ")
	method = strings.ReplaceAll(method, ".", " ")
	parts := strings.Fields(method)
	for i, part := range parts {
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}

func limitStatusText(text string, max int) string {
	text = strings.TrimSpace(text)
	if max <= 0 || len(text) <= max {
		return text
	}
	if max <= 3 {
		return text[:max]
	}
	return text[:max-3] + "..."
}

func sortedKeys(params map[string]any) []string {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// StopSession gracefully terminates: SIGTERM → 10s → SIGKILL.
func (r *Runner) StopSession() {
	r.stopSessionWithTimeout(10 * time.Second)
}

func (r *Runner) stopSessionWithTimeout(timeout time.Duration) {
	if r.cmd == nil || r.cmd.Process == nil {
		return
	}
	pid := r.cmd.Process.Pid
	slog.Info("agent.stopping", "pid", pid)

	_ = syscall.Kill(-pid, syscall.SIGTERM)

	done := make(chan struct{})
	go func() { _ = r.cmd.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(timeout):
		slog.Warn("agent.force_kill", "pid", pid)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-done
	}

	r.mu.Lock()
	for _, ch := range r.pending {
		select {
		case ch <- rpcResult{err: fmt.Errorf("session stopped")}:
		default:
		}
	}
	r.pending = make(map[int]chan rpcResult)
	r.mu.Unlock()

	r.cmd = nil
	slog.Info("agent.stopped")
}

// -- stdout/stderr readers --

func (r *Runner) readStdout(reader *bufio.Reader) {
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = trimNewline(line)
			if len(line) > 0 {
				r.dispatchLine(line)
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *Runner) readStderr(reader *bufio.Reader) {
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			text := strings.TrimSpace(strings.TrimRight(string(line), "\r\n"))
			if text != "" {
				slog.Debug("agent.stderr", "line", text)
				r.emitCurrentTurnEvent("agent_stderr", limitStatusText(text, 200))
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *Runner) dispatchLine(line []byte) {
	msg, err := ParseLine(line)
	if err != nil {
		slog.Debug("agent.parse_error", "line", string(line[:min(len(line), 200)]))
		return
	}

	if msg.Response != nil || msg.ErrResp != nil {
		var rawID any
		if msg.Response != nil {
			rawID = msg.Response.ID
		} else {
			rawID = msg.ErrResp.ID
		}
		reqID := normalizeID(rawID)

		r.mu.Lock()
		ch, ok := r.pending[reqID]
		if ok {
			delete(r.pending, reqID)
		}
		r.mu.Unlock()

		if ok {
			if msg.ErrResp != nil {
				ch <- rpcResult{err: fmt.Errorf("[%d] %s", msg.ErrResp.Error.Code, msg.ErrResp.Error.Message)}
			} else {
				ch <- rpcResult{result: msg.Response.Result}
			}
		}
		return
	}

	if msg.Notif != nil && isPriorityNotification(msg.Notif.Method) {
		r.priorityNotifCh <- msg
		return
	}

	select {
	case r.notifCh <- msg:
	default:
		slog.Warn("agent.notif_channel_full")
	}
}

var errNotificationChannelClosed = errors.New("notification channel closed")

func (r *Runner) nextNotification(ctx context.Context) (*Incoming, error) {
	select {
	case msg, ok := <-r.priorityNotifCh:
		if !ok {
			return nil, errNotificationChannelClosed
		}
		return msg, nil
	default:
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case msg, ok := <-r.priorityNotifCh:
		if !ok {
			return nil, errNotificationChannelClosed
		}
		return msg, nil
	case msg, ok := <-r.notifCh:
		if !ok {
			return nil, errNotificationChannelClosed
		}
		return msg, nil
	}
}

func (r *Runner) drainNotifications() {
	drainNotificationChannel(r.priorityNotifCh)
	drainNotificationChannel(r.notifCh)
}

func drainNotificationChannel(ch chan *Incoming) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func isTerminalNotification(method string) bool {
	switch method {
	case methodTurnCompleted, methodTurnFailed, methodTurnCancelled:
		return true
	default:
		return false
	}
}

func isPriorityNotification(method string) bool {
	return isTerminalNotification(method) || method == methodRateLimits
}

// -- JSON-RPC helpers --

func (r *Runner) sendRequest(ctx context.Context, timeout time.Duration, method string, params map[string]any) (map[string]any, error) {
	id := int(r.reqID.Add(1))
	ch := make(chan rpcResult, 1)

	r.mu.Lock()
	r.pending[id] = ch
	r.mu.Unlock()

	data, err := FormatRequest(id, method, params)
	if err != nil {
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		return nil, err
	}

	if err := r.stdin.Write(data); err != nil {
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		return nil, fmt.Errorf("write stdin: %w", err)
	}
	slog.Debug("agent.request_sent", "method", method, "id", id)

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		return nil, ctx.Err()
	case <-timer.C:
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		return nil, fmt.Errorf("timeout: %s id=%d", method, id)
	case res := <-ch:
		return res.result, res.err
	}
}

func (r *Runner) sendNotification(method string, params map[string]any) error {
	data, err := FormatNotification(method, params)
	if err != nil {
		return err
	}
	return r.stdin.Write(data)
}

// -- helpers --

func buildLaunchCommand(command string, writableDirs []string) string {
	cmd := strings.TrimSpace(command)
	if parts := strings.Fields(cmd); len(parts) > 0 {
		base := parts[0]
		if base == "codex" || strings.HasSuffix(base, "/codex") {
			rest := strings.TrimSpace(strings.TrimPrefix(cmd, base))
			var b strings.Builder
			b.WriteString(base)
			for _, dir := range writableDirs {
				dir = strings.TrimSpace(dir)
				if dir == "" {
					continue
				}
				b.WriteString(" --add-dir ")
				b.WriteString(shellQuote(dir))
			}
			if rest != "" {
				b.WriteString(" ")
				b.WriteString(rest)
			}
			b.WriteString(" -c ")
			b.WriteString(shellQuote("notify=[]"))
			return b.String()
		}
	}
	return cmd
}

func shellQuote(v string) string {
	if v == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(v, "'", `'"'"'`) + "'"
}

func resolveThreadSandbox(cfg *Config) string {
	if cfg.ThreadSandbox != "" {
		return cfg.ThreadSandbox
	}
	return normalizeSandboxPolicy(cfg.TurnSandboxPolicy)
}

func resolveTurnSandbox(cfg *Config) string {
	if cfg == nil {
		return ""
	}
	if policy := normalizeSandboxPolicy(cfg.TurnSandboxPolicy); policy != "" {
		return policy
	}
	return normalizeSandboxPolicy(cfg.ThreadSandbox)
}

func normalizeSandboxPolicy(p string) string {
	switch p {
	case "read-only", "workspace-write", "external-sandbox":
		return p
	}
	return ""
}

func buildTurnSandboxPolicy(cfg *Config) map[string]any {
	if cfg == nil {
		return nil
	}

	switch resolveTurnSandbox(cfg) {
	case "read-only":
		return map[string]any{"type": "readOnly"}
	case "workspace-write":
		policy := map[string]any{"type": "workspaceWrite"}
		if roots := sanitizeWritableRoots(cfg.AdditionalWritableDirs); len(roots) > 0 {
			policy["writableRoots"] = roots
		}
		return policy
	case "external-sandbox":
		return map[string]any{"type": "externalSandbox"}
	default:
		return nil
	}
}

func sanitizeWritableRoots(dirs []string) []string {
	if len(dirs) == 0 {
		return nil
	}

	roots := make([]string, 0, len(dirs))
	seen := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		roots = append(roots, dir)
	}
	return roots
}

func parseRateLimitEvent(params map[string]any, now time.Time) *RateLimitEvent {
	resetAt, ok := findRateLimitResetAt(params, now)
	if !ok {
		return &RateLimitEvent{}
	}
	return &RateLimitEvent{ResetAt: &resetAt}
}

// rateLimitUsedThreshold is the minimum used_percent (0–100 scale) at which a
// rate-limit window is considered throttled. Codex sends account/rateLimits/updated
// as an informational update even when usage is low; only windows at or above this
// threshold should trigger a dispatch pause.
const rateLimitUsedThreshold = 90.0

func findRateLimitResetAt(value any, now time.Time) (time.Time, bool) {
	switch v := value.(type) {
	case map[string]any:
		// If this map advertises a used_percent below the throttle threshold, its
		// resets_at should not contribute to the pause deadline.
		if pct, ok := extractUsedPercent(v); ok && pct < rateLimitUsedThreshold {
			return time.Time{}, false
		}
		var latest time.Time
		var found bool
		for key, raw := range v {
			if normalized, ok := normalizeRateLimitResetKey(key); ok {
				if ts, ok := parseRateLimitResetValue(normalized, raw, now); ok {
					if !found || ts.After(latest) {
						latest = ts
						found = true
					}
				}
			}
			if ts, ok := findRateLimitResetAt(raw, now); ok {
				if !found || ts.After(latest) {
					latest = ts
					found = true
				}
			}
		}
		return latest, found
	case []any:
		var latest time.Time
		var found bool
		for _, item := range v {
			if ts, ok := findRateLimitResetAt(item, now); ok {
				if !found || ts.After(latest) {
					latest = ts
					found = true
				}
			}
		}
		return latest, found
	default:
		return time.Time{}, false
	}
}

// extractUsedPercent returns the used_percent value (0–100 scale) from a limit
// map if the key is present in any casing/separator variant.
func extractUsedPercent(m map[string]any) (float64, bool) {
	for key, val := range m {
		normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(key)))
		if normalized == "usedpercent" {
			if n, ok := asFloat64(val); ok {
				return n, true
			}
		}
	}
	return 0, false
}

func normalizeRateLimitResetKey(key string) (string, bool) {
	normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(key)))
	switch normalized {
	case "reset", "resetat", "resetsat", "resettime", "resettimestamp":
		return "reset", true
	case "retryafter", "retryafterseconds":
		return "retry_after", true
	case "retryafterms", "retryaftermilliseconds":
		return "retry_after_ms", true
	default:
		return "", false
	}
}

func parseRateLimitResetValue(kind string, raw any, now time.Time) (time.Time, bool) {
	if s, ok := raw.(string); ok {
		s = strings.TrimSpace(s)
		if s == "" {
			return time.Time{}, false
		}
		switch kind {
		case "reset":
			for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
				if ts, err := time.Parse(layout, s); err == nil {
					return ts.UTC(), true
				}
			}
		case "retry_after":
			if d, err := time.ParseDuration(s); err == nil {
				return now.Add(d), true
			}
		}
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return parseRateLimitResetNumber(kind, n, now)
		}
		return time.Time{}, false
	}

	if n, ok := asFloat64(raw); ok {
		return parseRateLimitResetNumber(kind, n, now)
	}

	return time.Time{}, false
}

func parseRateLimitResetNumber(kind string, value float64, now time.Time) (time.Time, bool) {
	if value <= 0 {
		return time.Time{}, false
	}

	switch kind {
	case "retry_after":
		return now.Add(time.Duration(math.Round(value * float64(time.Second)))), true
	case "retry_after_ms":
		return now.Add(time.Duration(math.Round(value * float64(time.Millisecond)))), true
	default:
		if value >= 1e12 {
			return time.UnixMilli(int64(math.Round(value))).UTC(), true
		}
		if value >= 1e9 {
			return time.Unix(int64(math.Round(value)), 0).UTC(), true
		}
		return time.Time{}, false
	}
}

func normalizeID(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		var n int
		fmt.Sscanf(t, "%d", &n)
		return n
	}
	return 0
}

func toInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	}
	return 0
}

func asFloat64(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case int32:
		return float64(v), true
	case int16:
		return float64(v), true
	case int8:
		return float64(v), true
	case uint:
		return float64(v), true
	case uint64:
		return float64(v), true
	case uint32:
		return float64(v), true
	case uint16:
		return float64(v), true
	case uint8:
		return float64(v), true
	default:
		return 0, false
	}
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func emit(cb EventCallback, e Event) {
	if cb != nil {
		cb(e)
	}
}

type lockedWriter struct {
	mu sync.Mutex
	w  interface{ Write([]byte) (int, error) }
}

func (lw *lockedWriter) Write(b []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	_, err := lw.w.Write(b)
	return err
}
