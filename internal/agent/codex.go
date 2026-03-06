// Package agent implements the Codex/Claude Code subprocess runner over JSON-RPC stdio.
// Self-contained: no imports from other internal packages (avoids import cycles).
package agent

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Method constants for the Codex app-server JSON-RPC protocol.
// (Duplicated from protocol.go for package clarity.)
const (
	methodInitialize    = "initialize"
	methodInitialized   = "initialized"
	methodThreadStart   = "thread/start"
	methodTurnStart     = "turn/start"
	methodTurnCompleted = "turn/completed"
	methodTurnFailed    = "turn/failed"
	methodTurnCancelled = "turn/cancelled"
	methodTokenUsage    = "thread/tokenUsage/updated"
	methodRateLimits    = "account/rateLimits/updated"
	methodCmdApproval   = "item/commandExecution/requestApproval"
	methodFileApproval  = "item/fileChange/requestApproval"
	methodUserInput     = "item/tool/requestUserInput"
)

// Config is passed to the runner per session.
type Config struct {
	Command          string
	ApprovalPolicy   string
	MaxTurns         int
	TurnTimeoutMs         int
	ReadTimeoutMs         int
	ThreadStartTimeoutMs  int
	StallTimeoutMs        int
	TurnSandboxPolicy string
	ThreadSandbox    string
}

// TokenUsage tracks input/output/total tokens for a turn delta.
type TokenUsage struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
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

	mu      sync.Mutex
	pending map[int]chan rpcResult
	reqID   atomic.Int32

	notifCh chan *Incoming

	// cumulative token counts for delta computation
	lastInput  int64
	lastOutput int64
	lastTotal  int64
}

func NewRunner() *Runner {
	return &Runner{
		pending: make(map[int]chan rpcResult),
		notifCh: make(chan *Incoming, 512),
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

	launchCmd := buildLaunchCommand(cfg.Command)
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
	threadID, prompt, issueIdentifier, issueTitle string,
	cfg *Config,
	cb EventCallback,
) TurnResult {
	if r.cmd == nil || r.cmd.ProcessState != nil {
		return TurnResult{Error: "process_not_running"}
	}

	turnID := fmt.Sprintf("%d", time.Now().UnixNano())
	emit(cb, Event{
		Name: "turn_started", Timestamp: time.Now().UTC(),
		SessionID: r.sessionID, ThreadID: threadID, TurnID: turnID, PID: r.pid,
	})

	// Drain stale notifications from previous turns.
	for len(r.notifCh) > 0 {
		<-r.notifCh
	}

	turnParams := map[string]any{
		"threadId":       threadID,
		"input":          []any{map[string]any{"type": "text", "text": prompt}},
		"cwd":            r.cmd.Dir,
		"title":          fmt.Sprintf("%s: %s", issueIdentifier, issueTitle),
		"approvalPolicy": cfg.ApprovalPolicy,
	}
	if policy := normalizeSandboxPolicy(cfg.TurnSandboxPolicy); policy != "" {
		turnParams["sandboxPolicy"] = map[string]any{"type": policy}
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

func (r *Runner) consumeUntilDone(ctx context.Context, threadID, turnID string, cb EventCallback) TurnResult {
	for {
		select {
		case <-ctx.Done():
			emit(cb, Event{Name: "turn_failed", Timestamp: time.Now().UTC(),
				SessionID: r.sessionID, ThreadID: threadID, TurnID: turnID, Message: "turn_timeout"})
			return TurnResult{Error: "turn_timeout"}

		case msg, ok := <-r.notifCh:
			if !ok {
				return TurnResult{Error: "channel_closed"}
			}

			if msg.ServerReq != nil {
				r.handleServerRequest(msg.ServerReq, cb)
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
				emit(cb, Event{Name: "rate_limit", Timestamp: time.Now().UTC(),
					SessionID: r.sessionID, ThreadID: threadID, TurnID: turnID})

			default:
				slog.Debug("agent.unhandled_notification", "method", method)
			}
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

func (r *Runner) handleServerRequest(req *Request, cb EventCallback) {
	switch req.Method {
	case methodCmdApproval, methodFileApproval:
		slog.Debug("agent.auto_approve", "method", req.Method)
		data, _ := FormatResponse(req.ID, map[string]any{"approved": true})
		_ = r.stdin.Write(data)
		emit(cb, Event{Name: "approval_granted", Timestamp: time.Now().UTC(),
			SessionID: r.sessionID, Message: req.Method})
	case methodUserInput:
		slog.Warn("agent.user_input_unsupported")
		data, _ := FormatErrorResponse(req.ID, -32601, "User input not supported")
		_ = r.stdin.Write(data)
	default:
		slog.Warn("agent.unknown_server_request", "method", req.Method)
		data, _ := FormatErrorResponse(req.ID, -32601, "Unsupported: "+req.Method)
		_ = r.stdin.Write(data)
	}
}

// StopSession gracefully terminates: SIGTERM → 5s → SIGKILL.
func (r *Runner) StopSession() {
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
	case <-time.After(5 * time.Second):
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
			slog.Debug("agent.stderr", "line", strings.TrimRight(string(line), "\r\n"))
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

	select {
	case r.notifCh <- msg:
	default:
		slog.Warn("agent.notif_channel_full")
	}
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

func buildLaunchCommand(command string) string {
	cmd := strings.TrimSpace(command)
	if parts := strings.Fields(cmd); len(parts) > 0 {
		base := parts[0]
		if base == "codex" || strings.HasSuffix(base, "/codex") {
			return cmd + " -c 'notify=[]'"
		}
	}
	return cmd
}

func resolveThreadSandbox(cfg *Config) string {
	if cfg.ThreadSandbox != "" {
		return cfg.ThreadSandbox
	}
	return normalizeSandboxPolicy(cfg.TurnSandboxPolicy)
}

func normalizeSandboxPolicy(p string) string {
	switch p {
	case "read-only", "workspace-write", "external-sandbox":
		return p
	}
	return ""
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
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
