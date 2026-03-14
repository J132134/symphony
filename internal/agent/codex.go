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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"symphony/internal/types"
)

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
}

// TokenUsage is an alias for types.TokenUsage.
type TokenUsage = types.TokenUsage

type RateLimitEvent struct {
	ResetAt *time.Time
}

type EventDetailKind string

const (
	EventDetailNone          EventDetailKind = ""
	EventDetailCurrentTask   EventDetailKind = "current_task"
	EventDetailServerMessage EventDetailKind = "server_message"
)

// Event is emitted by the runner to the orchestrator callback.
type Event struct {
	Name       string
	Timestamp  time.Time
	SessionID  string
	ThreadID   string
	TurnID     string
	PID        string
	Usage      *TokenUsage
	RateLimit  *RateLimitEvent
	Message    string
	DetailKind EventDetailKind
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

	mu      sync.Mutex
	pending map[int]chan rpcResult
	reqID   atomic.Int32

	eventMu              sync.RWMutex
	activeCallback       EventCallback
	activeThreadID       string
	activeTurnID         string
	lastStderrEmittedAt  time.Time
	stderrDebounceWindow time.Duration
	notifCh              chan *Incoming
	priorityNotifCh chan *Incoming

	// cumulative token counts for delta computation
	lastInput  int64
	lastOutput int64
	lastTotal  int64
}

const defaultStderrDebounce = 500 * time.Millisecond

func NewRunner() *Runner {
	return &Runner{
		pending:              make(map[int]chan rpcResult),
		notifCh:              make(chan *Incoming, 512),
		stderrDebounceWindow: defaultStderrDebounce,
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
	initRes, err := r.sendRequest(ctx, readTimeout, methodInitialize, map[string]any{
		"protocolVersion": "2025-01-01",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "symphony", "version": "0.5.4"},
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
	r.setActiveEventSink(threadID, turnID, cb)
	defer r.clearActiveEventSink(threadID, turnID)

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
			r.handleServerRequest(msg.ServerReq)
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
			slog.Debug("agent.unhandled_notification", "method", method)
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

func (r *Runner) handleServerRequest(req *Request) {
	taskSummary := summarizeServerRequest(req)
	if taskSummary != "" {
		r.emitActiveEvent(Event{
			Name:       "agent_task",
			Timestamp:  time.Now().UTC(),
			Message:    taskSummary,
			DetailKind: EventDetailCurrentTask,
		})
	}

	switch req.Method {
	case methodCmdApproval, methodFileApproval:
		slog.Info("agent.auto_approve", "method", req.Method, "params", summarizeParams(req.Params))
		data, err := FormatResponse(req.ID, map[string]any{"approved": true})
		if err != nil {
			slog.Warn("agent.format_response_failed", "method", req.Method, "error", err)
			return
		}
		if err := r.stdin.Write(data); err != nil {
			slog.Warn("agent.write_response_failed", "method", req.Method, "error", err)
		}
		r.emitActiveEvent(Event{Name: "approval_granted", Timestamp: time.Now().UTC(), Message: taskSummary})
	case methodToolCall:
		slog.Warn("agent.tool_call_unsupported", "params", summarizeParams(req.Params))
		r.sendErrorResponse(req.ID, -32601, "Tool call not supported", req.Method)
	case methodUserInput:
		if response, ok := buildAutoUserInputResponse(req.Params); ok {
			slog.Info("agent.auto_approve_user_input", "params", summarizeParams(req.Params))
			data, err := FormatResponse(req.ID, response)
			if err != nil {
				slog.Warn("agent.format_response_failed", "method", req.Method, "error", err)
				return
			}
			if err := r.stdin.Write(data); err != nil {
				slog.Warn("agent.write_response_failed", "method", req.Method, "error", err)
			}
			r.emitActiveEvent(Event{Name: "approval_granted", Timestamp: time.Now().UTC(), Message: taskSummary})
			return
		}
		slog.Warn("agent.user_input_unsupported", "params", summarizeParams(req.Params))
		r.sendErrorResponse(req.ID, -32601, "User input not supported", req.Method)
	default:
		slog.Warn("agent.unknown_server_request", "method", req.Method)
		r.sendErrorResponse(req.ID, -32601, "Unsupported: "+req.Method, req.Method)
	}
}

func (r *Runner) sendErrorResponse(id any, code int, message, method string) {
	data, err := FormatErrorResponse(id, code, message)
	if err != nil {
		slog.Warn("agent.format_error_response_failed", "method", method, "error", err)
		return
	}
	if err := r.stdin.Write(data); err != nil {
		slog.Warn("agent.write_response_failed", "method", method, "error", err)
	}
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
		answers[questionID] = map[string]any{"answers": []string{answer}}
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
	return "", false
}

func looksLikeApprovalQuestion(question map[string]any, options []any) bool {
	text := strings.ToLower(firstNonEmptyString(question["header"], question["question"]))
	if containsApprovalWord(text) {
		return true
	}
	for _, raw := range options {
		option, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		label := strings.ToLower(firstNonEmptyString(option["label"], option["description"]))
		if containsApprovalWord(label) || strings.Contains(label, "run the tool") {
			return true
		}
	}
	return false
}

// containsApprovalWord checks for "approve" while excluding negated forms like "disapprove".
func containsApprovalWord(text string) bool {
	idx := strings.Index(text, "approve")
	if idx < 0 {
		return false
	}
	if idx >= 3 && text[idx-3:idx] == "dis" {
		return false
	}
	return true
}

func firstNonEmptyString(values ...any) string {
	for _, value := range values {
		if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
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

func summarizeServerRequest(req *Request) string {
	if req == nil {
		return ""
	}

	switch req.Method {
	case methodCmdApproval:
		return summarizeCommandApproval(req.Params)
	case methodFileApproval:
		return summarizeFileApproval(req.Params)
	case methodUserInput:
		return summarizeUserInput(req.Params)
	case methodToolCall:
		return summarizeToolCall(req.Params)
	default:
		return ""
	}
}

func summarizeCommandApproval(params map[string]any) string {
	command := compactInline(firstNonEmptyString(
		params["command"],
		params["cmd"],
		params["reason"],
	), 120)
	if command == "" {
		command = compactInline(summarizeParams(params), 120)
	}
	if command == "" {
		return "awaiting command approval"
	}
	return "awaiting command approval: " + command
}

func summarizeFileApproval(params map[string]any) string {
	target := compactInline(firstNonEmptyString(
		params["path"],
		params["filePath"],
		params["target"],
	), 120)
	if target == "" {
		target = compactInline(summarizeParams(params), 120)
	}
	if target == "" {
		return "awaiting file change approval"
	}
	return "awaiting file change approval: " + target
}

func summarizeUserInput(params map[string]any) string {
	if rawQuestions, ok := params["questions"].([]any); ok {
		for _, raw := range rawQuestions {
			question, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			prompt := compactInline(firstNonEmptyString(question["header"], question["question"]), 120)
			if prompt != "" {
				return "waiting for tool input: " + prompt
			}
		}
	}

	prompt := compactInline(firstNonEmptyString(params["header"], params["question"], params["prompt"]), 120)
	if prompt == "" {
		prompt = compactInline(summarizeParams(params), 120)
	}
	if prompt == "" {
		return "waiting for tool input"
	}
	return "waiting for tool input: " + prompt
}

func summarizeToolCall(params map[string]any) string {
	toolName := compactInline(firstNonEmptyString(params["toolName"], params["name"]), 80)
	if toolName == "" {
		if tool, ok := params["tool"].(map[string]any); ok {
			toolName = compactInline(firstNonEmptyString(tool["name"]), 80)
		}
	}

	if toolName == "" {
		toolName = "unknown tool"
	}

	args := compactInline(firstNonEmptyString(params["arguments"], params["input"], params["prompt"]), 120)
	if args == "" {
		return "running tool: " + toolName
	}
	return "running tool: " + toolName + " " + args
}

func compactInline(v string, limit int) string {
	v = strings.Join(strings.Fields(strings.TrimSpace(v)), " ")
	if v == "" {
		return ""
	}
	if limit > 0 && len(v) > limit {
		if limit <= 3 {
			return v[:limit]
		}
		return v[:limit-3] + "..."
	}
	return v
}

var ansiEscapeRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// rustLogPrefixRe matches Rust tracing log prefixes like:
// "2026-03-14T17:06:03.311664Z ERROR codex_app_server::bespoke_event_handling: "
var rustLogPrefixRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T[\d:.]+Z\s+(ERROR|WARN|INFO|DEBUG|TRACE)\s+[\w:]+:\s*`)

func stripStderrNoise(s string) string {
	s = ansiEscapeRe.ReplaceAllString(s, "")
	s = rustLogPrefixRe.ReplaceAllString(s, "")
	return s
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
			text := compactInline(stripStderrNoise(strings.TrimRight(string(line), "\r\n")), 160)
			slog.Debug("agent.stderr", "line", text)
			if text != "" {
				now := time.Now().UTC()
				r.eventMu.Lock()
				elapsed := now.Sub(r.lastStderrEmittedAt)
				shouldEmit := elapsed >= r.stderrDebounceWindow
				if shouldEmit {
					r.lastStderrEmittedAt = now
				}
				r.eventMu.Unlock()
				if shouldEmit {
					r.emitActiveEvent(Event{
						Name:       "app_server_message",
						Timestamp:  now,
						Message:    text,
						DetailKind: EventDetailServerMessage,
					})
				}
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
	case "none":
		return ""
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
		policy := map[string]any{"type": "workspaceWrite", "networkAccess": true}
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

func (r *Runner) setActiveEventSink(threadID, turnID string, cb EventCallback) {
	r.eventMu.Lock()
	defer r.eventMu.Unlock()
	r.activeCallback = cb
	r.activeThreadID = threadID
	r.activeTurnID = turnID
}

func (r *Runner) clearActiveEventSink(threadID, turnID string) {
	r.eventMu.Lock()
	defer r.eventMu.Unlock()
	if r.activeThreadID != threadID || r.activeTurnID != turnID {
		return
	}
	r.activeCallback = nil
	r.activeThreadID = ""
	r.activeTurnID = ""
}

func (r *Runner) emitActiveEvent(e Event) {
	r.eventMu.RLock()
	cb := r.activeCallback
	threadID := r.activeThreadID
	turnID := r.activeTurnID
	r.eventMu.RUnlock()
	if cb == nil {
		return
	}
	if e.SessionID == "" {
		e.SessionID = r.sessionID
	}
	if e.ThreadID == "" {
		e.ThreadID = threadID
	}
	if e.TurnID == "" {
		e.TurnID = turnID
	}
	if e.PID == "" {
		e.PID = r.pid
	}
	emit(cb, e)
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
