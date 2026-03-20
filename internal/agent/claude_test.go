package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// -- StartSession tests --

func TestClaudeRunnerStartSessionCreatesNewID(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	r := NewClaudeRunner()

	id, err := r.StartSession(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if id == "" {
		t.Fatal("StartSession returned empty session ID")
	}
	if !r.firstTurn {
		t.Fatal("firstTurn should be true for new session")
	}

	// Verify file was written.
	data, err := os.ReadFile(filepath.Join(dir, symphonyStateDir, sessionIDFile))
	if err != nil {
		t.Fatalf("read session_id file: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != id {
		t.Fatalf("session_id file = %q, want %q", got, id)
	}
}

func TestClaudeRunnerStartSessionReusesExistingID(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stateDir := filepath.Join(dir, symphonyStateDir)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wantID := "existing-session-123"
	if err := os.WriteFile(filepath.Join(stateDir, sessionIDFile), []byte(wantID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewClaudeRunner()
	id, err := r.StartSession(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if id != wantID {
		t.Fatalf("StartSession = %q, want %q (reused)", id, wantID)
	}
	if r.firstTurn {
		t.Fatal("firstTurn should be false for resumed session")
	}
}

// -- buildArgs tests --

func TestClaudeRunnerBuildArgsFirstTurn(t *testing.T) {
	t.Parallel()

	r := NewClaudeRunner()
	r.sessionID = "test-session"
	r.firstTurn = true

	args := r.buildArgs(&Config{})

	if args[0] != "claude" || args[1] != "-p" {
		t.Fatalf("args start = %v, want [claude -p ...]", args[:2])
	}
	assertContains(t, args, "--session-id")
	assertContains(t, args, "test-session")
	assertNotContains(t, args, "--resume")
}

func TestClaudeRunnerBuildArgsResume(t *testing.T) {
	t.Parallel()

	r := NewClaudeRunner()
	r.sessionID = "test-session"
	r.firstTurn = false

	args := r.buildArgs(&Config{})

	assertContains(t, args, "--resume")
	assertContains(t, args, "test-session")
	assertNotContains(t, args, "--session-id")
}

func TestClaudeRunnerBuildArgsWithModel(t *testing.T) {
	t.Parallel()

	r := NewClaudeRunner()
	r.sessionID = "s"
	r.firstTurn = true

	args := r.buildArgs(&Config{Model: "claude-sonnet-4-20250514"})

	assertContains(t, args, "--model")
	assertContains(t, args, "claude-sonnet-4-20250514")
}

func TestClaudeRunnerBuildArgsWithAppendSystemPrompt(t *testing.T) {
	t.Parallel()

	r := NewClaudeRunner()
	r.sessionID = "s"
	r.firstTurn = true

	args := r.buildArgs(&Config{AppendSystemPrompt: "Always respond in JSON"})

	assertContains(t, args, "--append-system-prompt")
	assertContains(t, args, "Always respond in JSON")
}

func TestClaudeRunnerBuildArgsOmitsEmptyOptionals(t *testing.T) {
	t.Parallel()

	r := NewClaudeRunner()
	r.sessionID = "s"
	r.firstTurn = true

	args := r.buildArgs(&Config{})

	assertNotContains(t, args, "--model")
	assertNotContains(t, args, "--append-system-prompt")
}

// -- stream-json parsing tests --

func TestHandleStreamEventAssistantText(t *testing.T) {
	t.Parallel()

	r := NewClaudeRunner()
	r.sessionID = "s1"
	var events []Event
	r.setActiveEventSink("s1", "t1", func(e Event) { events = append(events, e) })

	line := mustJSON(t, map[string]any{"type": "assistant", "subtype": "text", "text": "hello"})
	result, done := r.handleStreamEvent("t1", line, nil)
	if done {
		t.Fatal("assistant/text should not terminate")
	}
	if result.Success {
		t.Fatal("result.Success should be false (not done)")
	}
	if len(events) != 1 || events[0].Name != "agent_activity" {
		t.Fatalf("events = %v, want [agent_activity]", eventNames(events))
	}
}

func TestHandleStreamEventAssistantToolUse(t *testing.T) {
	t.Parallel()

	r := NewClaudeRunner()
	r.sessionID = "s1"
	var events []Event
	r.setActiveEventSink("s1", "t1", func(e Event) { events = append(events, e) })

	line := mustJSON(t, map[string]any{"type": "assistant", "subtype": "tool_use", "tool": "Read"})
	_, done := r.handleStreamEvent("t1", line, nil)
	if done {
		t.Fatal("tool_use should not terminate")
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2 (activity + task)", len(events))
	}
	if events[1].Name != "agent_task" {
		t.Fatalf("events[1].Name = %q, want agent_task", events[1].Name)
	}
	if !strings.Contains(events[1].Message, "Read") {
		t.Fatalf("events[1].Message = %q, want tool name", events[1].Message)
	}
}

func TestHandleStreamEventResultSuccess(t *testing.T) {
	t.Parallel()

	r := NewClaudeRunner()
	r.sessionID = "s1"

	var events []Event
	cb := func(e Event) { events = append(events, e) }

	line := mustJSON(t, map[string]any{
		"type":           "result",
		"subtype":        "success",
		"session_id":     "s1",
		"total_cost_usd": 0.05,
		"usage":          map[string]any{"input_tokens": 1000, "output_tokens": 500},
	})

	result, done := r.handleStreamEvent("t1", line, cb)
	if !done {
		t.Fatal("result should terminate")
	}
	if !result.Success {
		t.Fatalf("result.Success = false, want true (error=%q)", result.Error)
	}
	if !result.CompletedNaturally {
		t.Fatal("result.CompletedNaturally = false, want true")
	}

	// Should have token_usage + turn_completed events.
	if len(events) < 2 {
		t.Fatalf("len(events) = %d, want >= 2", len(events))
	}
	if events[0].Name != "token_usage" {
		t.Fatalf("events[0].Name = %q, want token_usage", events[0].Name)
	}
	if events[0].Usage.InputTokens != 1000 || events[0].Usage.OutputTokens != 500 {
		t.Fatalf("usage = %+v, want 1000/500", events[0].Usage)
	}
}

func TestHandleStreamEventResultErrorMaxTurns(t *testing.T) {
	t.Parallel()

	r := NewClaudeRunner()
	r.sessionID = "s1"

	line := mustJSON(t, map[string]any{
		"type":    "result",
		"subtype": "error_max_turns",
	})

	result, done := r.handleStreamEvent("t1", line, func(Event) {})
	if !done {
		t.Fatal("result should terminate")
	}
	if !result.Success {
		t.Fatal("error_max_turns should still be Success=true")
	}
}

func TestHandleStreamEventResultError(t *testing.T) {
	t.Parallel()

	r := NewClaudeRunner()
	r.sessionID = "s1"

	line := mustJSON(t, map[string]any{
		"type":    "result",
		"subtype": "error",
	})

	result, done := r.handleStreamEvent("t1", line, func(Event) {})
	if !done {
		t.Fatal("result should terminate")
	}
	if result.Success {
		t.Fatal("error result should not be Success")
	}
	if result.Error != "error" {
		t.Fatalf("result.Error = %q, want \"error\"", result.Error)
	}
}

func TestHandleStreamEventMalformedJSON(t *testing.T) {
	t.Parallel()

	r := NewClaudeRunner()
	_, done := r.handleStreamEvent("t1", []byte("not json"), nil)
	if done {
		t.Fatal("malformed JSON should not terminate")
	}
}

func TestHandleStreamEventSystemInit(t *testing.T) {
	t.Parallel()

	r := NewClaudeRunner()
	r.sessionID = "s1"
	var events []Event
	r.setActiveEventSink("s1", "t1", func(e Event) { events = append(events, e) })

	line := mustJSON(t, map[string]any{"type": "system", "subtype": "init"})
	_, done := r.handleStreamEvent("t1", line, nil)
	if done {
		t.Fatal("system/init should not terminate")
	}
	// system events are silently skipped.
	if len(events) != 0 {
		t.Fatalf("len(events) = %d, want 0 for system event", len(events))
	}
}

// -- consumeStreamJSON integration test --

func TestConsumeStreamJSONFullSequence(t *testing.T) {
	t.Parallel()

	r := NewClaudeRunner()
	r.sessionID = "s1"

	lines := strings.Join([]string{
		mustJSONStr(t, map[string]any{"type": "system", "subtype": "init"}),
		mustJSONStr(t, map[string]any{"type": "assistant", "subtype": "text", "text": "analyzing"}),
		mustJSONStr(t, map[string]any{"type": "assistant", "subtype": "tool_use", "tool": "Bash"}),
		mustJSONStr(t, map[string]any{
			"type":    "result",
			"subtype": "success",
			"usage":   map[string]any{"input_tokens": 2000, "output_tokens": 800},
		}),
	}, "\n") + "\n"

	var events []Event
	cb := func(e Event) { events = append(events, e) }

	r.setActiveEventSink("s1", "turn-1", cb)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result := r.consumeStreamJSON(ctx, "turn-1", bufio.NewReader(strings.NewReader(lines)), cb)
	if !result.Success {
		t.Fatalf("result.Success = false, error = %q", result.Error)
	}
	if !result.CompletedNaturally {
		t.Fatal("result.CompletedNaturally = false")
	}

	names := eventNames(events)
	if !contains(names, "agent_activity") {
		t.Fatalf("events missing agent_activity: %v", names)
	}
	if !contains(names, "agent_task") {
		t.Fatalf("events missing agent_task: %v", names)
	}
	if !contains(names, "token_usage") {
		t.Fatalf("events missing token_usage: %v", names)
	}
	if !contains(names, "turn_completed") {
		t.Fatalf("events missing turn_completed: %v", names)
	}
}

// -- helpers --

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustJSONStr(t *testing.T, v any) string {
	t.Helper()
	return string(mustJSON(t, v))
}

func eventNames(events []Event) []string {
	names := make([]string, len(events))
	for i, e := range events {
		names[i] = e.Name
	}
	return names
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func assertContains(t *testing.T, args []string, want string) {
	t.Helper()
	for _, a := range args {
		if a == want {
			return
		}
	}
	t.Fatalf("args %v missing %q", args, want)
}

func assertNotContains(t *testing.T, args []string, unwanted string) {
	t.Helper()
	for _, a := range args {
		if a == unwanted {
			t.Fatalf("args %v unexpectedly contains %q", args, unwanted)
		}
	}
}
