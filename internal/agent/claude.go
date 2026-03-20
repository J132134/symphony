package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Compile-time interface check.
var _ Runner = (*ClaudeRunner)(nil)

const (
	symphonyStateDir = ".symphony"
	sessionIDFile    = "session_id"
)

// ClaudeRunner manages claude -p process-per-turn execution.
type ClaudeRunner struct {
	sessionID     string
	workspacePath string
	firstTurn     bool

	mu  sync.Mutex
	cmd *exec.Cmd
	pid string

	stderrDebounceWindow time.Duration

	eventMu             sync.RWMutex
	activeCallback      EventCallback
	activeThreadID      string
	activeTurnID        string
	lastStderrEmittedAt time.Time
}

func NewClaudeRunner() *ClaudeRunner {
	return &ClaudeRunner{
		stderrDebounceWindow: defaultStderrDebounce,
	}
}

func (r *ClaudeRunner) PID() string       { return r.pid }
func (r *ClaudeRunner) SessionID() string { return r.sessionID }
func (r *ClaudeRunner) ThreadID() string  { return r.sessionID }

// StartSession sets up the session ID without launching a process.
// If a session_id file exists in the workspace (continuation), it is reused.
func (r *ClaudeRunner) StartSession(_ context.Context, workspacePath string, _ *Config) (string, error) {
	r.workspacePath = workspacePath

	stateDir := filepath.Join(workspacePath, symphonyStateDir)
	sessionIDPath := filepath.Join(stateDir, sessionIDFile)

	if data, err := os.ReadFile(sessionIDPath); err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			r.sessionID = id
			r.firstTurn = false
			slog.Info("agent.claude.session_resumed", "session_id", id)
			return id, nil
		}
	}

	r.sessionID = fmt.Sprintf("%d", time.Now().UnixNano())
	r.firstTurn = true

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}
	if err := os.WriteFile(sessionIDPath, []byte(r.sessionID+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("write session_id: %w", err)
	}

	slog.Info("agent.claude.session_created", "session_id", r.sessionID)
	return r.sessionID, nil
}

// RunTurn spawns a claude -p process for a single turn and streams events.
func (r *ClaudeRunner) RunTurn(
	ctx context.Context,
	_, turnID, prompt, issueIdentifier, issueTitle string,
	cfg *Config,
	cb EventCallback,
) TurnResult {
	emit(cb, Event{
		Name: "turn_started", Timestamp: time.Now().UTC(),
		SessionID: r.sessionID, ThreadID: r.sessionID, TurnID: turnID, PID: r.pid,
	})

	r.setActiveEventSink(r.sessionID, turnID, cb)
	defer r.clearActiveEventSink(r.sessionID, turnID)

	args := r.buildArgs(cfg)
	slog.Info("agent.claude.run_turn", "args", args, "turn_id", turnID,
		"issue", issueIdentifier, "first_turn", r.firstTurn)

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = r.workspacePath
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return TurnResult{Error: fmt.Sprintf("stdin pipe: %v", err)}
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return TurnResult{Error: fmt.Sprintf("stdout pipe: %v", err)}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return TurnResult{Error: fmt.Sprintf("stderr pipe: %v", err)}
	}

	if err := cmd.Start(); err != nil {
		return TurnResult{Error: fmt.Sprintf("start: %v", err)}
	}

	r.mu.Lock()
	r.cmd = cmd
	if cmd.Process != nil {
		r.pid = fmt.Sprintf("%d", cmd.Process.Pid)
	}
	r.mu.Unlock()

	// After this turn we are no longer the first turn.
	r.firstTurn = false

	// Feed prompt via stdin, then close to signal end of input.
	go func() {
		_, _ = stdinPipe.Write([]byte(prompt))
		_ = stdinPipe.Close()
	}()

	go r.readStderr(bufio.NewReader(stderrPipe))

	turnTimeout := time.Duration(cfg.TurnTimeoutMs) * time.Millisecond
	tctx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()

	result := r.consumeStreamJSON(tctx, turnID, bufio.NewReader(stdoutPipe), cb)

	// Wait for the process regardless of result.
	_ = cmd.Wait()

	r.mu.Lock()
	r.cmd = nil
	r.mu.Unlock()

	if !result.Success && result.Error == "" && tctx.Err() != nil {
		emit(cb, Event{Name: "turn_failed", Timestamp: time.Now().UTC(),
			SessionID: r.sessionID, ThreadID: r.sessionID, TurnID: turnID, Message: "turn_timeout"})
		return TurnResult{Error: "turn_timeout"}
	}

	return result
}

func (r *ClaudeRunner) buildArgs(cfg *Config) []string {
	args := []string{"claude", "-p",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
	}

	if r.firstTurn {
		args = append(args, "--session-id", r.sessionID)
	} else {
		args = append(args, "--resume", r.sessionID)
	}

	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", cfg.AppendSystemPrompt)
	}

	return args
}

// InterruptTurn sends SIGTERM to the running process group.
func (r *ClaudeRunner) InterruptTurn(_ context.Context, _, _ string, _ *Config) error {
	r.mu.Lock()
	cmd := r.cmd
	r.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

// StopSession kills the running process if any.
func (r *ClaudeRunner) StopSession() {
	r.mu.Lock()
	cmd := r.cmd
	r.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}

	pid := cmd.Process.Pid
	slog.Info("agent.claude.stopping", "pid", pid)
	_ = syscall.Kill(-pid, syscall.SIGTERM)

	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		slog.Warn("agent.claude.force_kill", "pid", pid)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-done
	}

	r.mu.Lock()
	r.cmd = nil
	r.mu.Unlock()

	slog.Info("agent.claude.stopped")
}

// -- stream-json parsing --

// streamEvent represents a single event from claude -p --output-format stream-json.
type streamEvent struct {
	Type       string          `json:"type"`
	Subtype    string          `json:"subtype"`
	SessionID  string          `json:"session_id,omitempty"`
	Tool       string          `json:"tool,omitempty"`
	TotalCost  float64         `json:"total_cost_usd,omitempty"`
	Usage      *streamUsage    `json:"usage,omitempty"`
	ModelUsage json.RawMessage `json:"modelUsage,omitempty"`
	NumTurns   int             `json:"num_turns,omitempty"`
	DurationMs int             `json:"duration_ms,omitempty"`
	Result     string          `json:"result,omitempty"`
}

type streamUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

func (r *ClaudeRunner) consumeStreamJSON(ctx context.Context, turnID string, reader *bufio.Reader, cb EventCallback) TurnResult {
	for {
		select {
		case <-ctx.Done():
			return TurnResult{Error: "turn_timeout"}
		default:
		}

		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = trimNewline(line)
			if len(line) > 0 {
				if result, done := r.handleStreamEvent(turnID, line, cb); done {
					return result
				}
			}
		}
		if err != nil {
			// EOF without a result event — process exited.
			return TurnResult{Error: "process_exited_without_result"}
		}
	}
}

func (r *ClaudeRunner) handleStreamEvent(turnID string, line []byte, cb EventCallback) (TurnResult, bool) {
	var ev streamEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		slog.Debug("agent.claude.parse_error", "line", string(line[:min(len(line), 200)]))
		return TurnResult{}, false
	}

	now := time.Now().UTC()

	switch ev.Type {
	case "assistant":
		// Activity event for stall detection.
		r.emitActiveEvent(Event{
			Name:      "agent_activity",
			Timestamp: now,
		})
		if ev.Subtype == "tool_use" && ev.Tool != "" {
			r.emitActiveEvent(Event{
				Name:       "agent_task",
				Timestamp:  now,
				Message:    "running tool: " + ev.Tool,
				DetailKind: EventDetailCurrentTask,
			})
		}

	case "result":
		return r.handleResultEvent(turnID, &ev, cb), true
	}

	return TurnResult{}, false
}

func (r *ClaudeRunner) handleResultEvent(turnID string, ev *streamEvent, cb EventCallback) TurnResult {
	now := time.Now().UTC()

	// Emit token usage if available.
	if ev.Usage != nil {
		emit(cb, Event{
			Name: "token_usage", Timestamp: now,
			SessionID: r.sessionID, ThreadID: r.sessionID, TurnID: turnID,
			Usage: &TokenUsage{
				InputTokens:  ev.Usage.InputTokens,
				OutputTokens: ev.Usage.OutputTokens,
				TotalTokens:  ev.Usage.InputTokens + ev.Usage.OutputTokens,
			},
		})
	}

	switch ev.Subtype {
	case "success", "error_max_turns":
		emit(cb, Event{Name: "turn_completed", Timestamp: now,
			SessionID: r.sessionID, ThreadID: r.sessionID, TurnID: turnID})
		return TurnResult{Success: true, CompletedNaturally: true}
	default:
		errMsg := ev.Subtype
		if errMsg == "" {
			errMsg = "unknown_error"
		}
		emit(cb, Event{Name: "turn_failed", Timestamp: now,
			SessionID: r.sessionID, ThreadID: r.sessionID, TurnID: turnID, Message: errMsg})
		return TurnResult{Error: errMsg, CompletedNaturally: true}
	}
}

// -- stderr reading (reuses same debounce pattern as CodexRunner) --

func (r *ClaudeRunner) readStderr(reader *bufio.Reader) {
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			text := compactInline(stripStderrNoise(strings.TrimRight(string(line), "\r\n")), 160)
			slog.Debug("agent.claude.stderr", "line", text)
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

// -- active event sink (same pattern as CodexRunner) --

func (r *ClaudeRunner) setActiveEventSink(threadID, turnID string, cb EventCallback) {
	r.eventMu.Lock()
	defer r.eventMu.Unlock()
	r.activeCallback = cb
	r.activeThreadID = threadID
	r.activeTurnID = turnID
}

func (r *ClaudeRunner) clearActiveEventSink(threadID, turnID string) {
	r.eventMu.Lock()
	defer r.eventMu.Unlock()
	if r.activeThreadID != threadID || r.activeTurnID != turnID {
		return
	}
	r.activeCallback = nil
	r.activeThreadID = ""
	r.activeTurnID = ""
}

func (r *ClaudeRunner) emitActiveEvent(e Event) {
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
