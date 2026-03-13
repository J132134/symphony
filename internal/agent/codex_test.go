package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunnerDispatchLineKeepsTerminalNotificationWhenNotifQueueFull(t *testing.T) {
	r := NewRunner()
	fillNotifQueue(r)

	r.dispatchLine(mustNotification(t, methodTurnCompleted, nil))

	if got := len(r.notifCh); got != cap(r.notifCh) {
		t.Fatalf("notifCh len = %d, want %d", got, cap(r.notifCh))
	}
	if got := len(r.priorityNotifCh); got != 1 {
		t.Fatalf("priorityNotifCh len = %d, want 1", got)
	}
}

func TestRunnerDispatchLineKeepsRateLimitNotificationWhenNotifQueueFull(t *testing.T) {
	r := NewRunner()
	fillNotifQueue(r)

	r.dispatchLine(mustNotification(t, methodRateLimits, nil))

	if got := len(r.notifCh); got != cap(r.notifCh) {
		t.Fatalf("notifCh len = %d, want %d", got, cap(r.notifCh))
	}
	if got := len(r.priorityNotifCh); got != 1 {
		t.Fatalf("priorityNotifCh len = %d, want 1", got)
	}
}

func TestRunnerConsumeUntilDoneReadsTerminalNotificationWhenNotifQueueFull(t *testing.T) {
	r := NewRunner()
	fillNotifQueue(r)
	r.dispatchLine(mustNotification(t, methodTurnCompleted, nil))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	result := r.consumeUntilDone(ctx, "thread-1", "turn-1", nil)
	if !result.Success {
		t.Fatalf("Success = false, want true (error=%q)", result.Error)
	}
	if !result.CompletedNaturally {
		t.Fatalf("CompletedNaturally = false, want true")
	}
}

func TestParseRateLimitEventResetAtRFC3339(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC)
	event := parseRateLimitEvent(map[string]any{
		"limits": []any{
			map[string]any{"resetAt": "2026-03-07T00:05:00Z"},
		},
	}, now)

	if event == nil || event.ResetAt == nil {
		t.Fatal("expected resetAt to be parsed")
	}
	want := time.Date(2026, 3, 7, 0, 5, 0, 0, time.UTC)
	if !event.ResetAt.Equal(want) {
		t.Fatalf("resetAt = %v, want %v", event.ResetAt, want)
	}
}

func TestParseRateLimitEventRetryAfterSeconds(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC)
	event := parseRateLimitEvent(map[string]any{
		"retryAfter": 12,
	}, now)

	if event == nil || event.ResetAt == nil {
		t.Fatal("expected retryAfter to be parsed")
	}
	want := now.Add(12 * time.Second)
	if !event.ResetAt.Equal(want) {
		t.Fatalf("resetAt = %v, want %v", event.ResetAt, want)
	}
}

func TestParseRateLimitEventSkipsLowUsedPercent(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 8, 13, 0, 0, 0, time.UTC)
	// primary: 1% used (not throttled), secondary: 0% used (not throttled).
	// Neither window exceeds the threshold, so ResetAt must be nil.
	event := parseRateLimitEvent(map[string]any{
		"primary": map[string]any{
			"used_percent":   1.0,
			"window_minutes": 300,
			"resets_at":      float64(now.Add(5 * time.Hour).Unix()),
		},
		"secondary": map[string]any{
			"used_percent":   0.0,
			"window_minutes": 10080,
			"resets_at":      float64(now.Add(7 * 24 * time.Hour).Unix()),
		},
	}, now)

	if event == nil {
		t.Fatal("expected non-nil event")
	}
	if event.ResetAt != nil {
		t.Fatalf("ResetAt = %v, want nil (no window is throttled)", event.ResetAt)
	}
}

func TestParseRateLimitEventUsesThrottledWindowOnly(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 8, 13, 0, 0, 0, time.UTC)
	primaryReset := now.Add(2 * time.Hour)
	// primary: 100% used (throttled), secondary: 0% used (not throttled).
	// Only the primary resets_at should be returned, not the later secondary one.
	event := parseRateLimitEvent(map[string]any{
		"primary": map[string]any{
			"used_percent":   100.0,
			"window_minutes": 300,
			"resets_at":      float64(primaryReset.Unix()),
		},
		"secondary": map[string]any{
			"used_percent":   0.0,
			"window_minutes": 10080,
			"resets_at":      float64(now.Add(7 * 24 * time.Hour).Unix()),
		},
	}, now)

	if event == nil || event.ResetAt == nil {
		t.Fatal("expected ResetAt to be set for throttled primary window")
	}
	if !event.ResetAt.Equal(primaryReset.UTC()) {
		t.Fatalf("ResetAt = %v, want %v (primary reset, not secondary)", event.ResetAt, primaryReset.UTC())
	}
}

func TestBuildLaunchCommandAddsWritableDirsForCodex(t *testing.T) {
	got := buildLaunchCommand("codex app-server", []string{
		"/tmp/repo/.git",
		"/tmp/repo with space/.git/worktrees/foo",
	})

	for _, want := range []string{
		"codex",
		"app-server",
		"-c 'notify=[]'",
		"--add-dir '/tmp/repo/.git'",
		"--add-dir '/tmp/repo with space/.git/worktrees/foo'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("buildLaunchCommand() = %q, want substring %q", got, want)
		}
	}
	if strings.Index(got, "--add-dir '/tmp/repo/.git'") > strings.Index(got, "app-server") {
		t.Fatalf("buildLaunchCommand() = %q, want --add-dir before app-server", got)
	}
}

func TestBuildLaunchCommandSkipsWritableDirsForNonCodex(t *testing.T) {
	got := buildLaunchCommand("claude", []string{"/tmp/repo/.git"})
	if got != "claude" {
		t.Fatalf("buildLaunchCommand() = %q, want %q", got, "claude")
	}
}

func TestBuildTurnSandboxPolicyUsesWorkspaceWriteWritableRoots(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		TurnSandboxPolicy: "workspace-write",
		AdditionalWritableDirs: []string{
			"/tmp/repo/.git",
			"  /tmp/repo/.git/worktrees/j-62  ",
			"/tmp/repo/.git",
			"",
		},
	}

	got := buildTurnSandboxPolicy(cfg)
	wantRoots := []string{
		"/tmp/repo/.git",
		"/tmp/repo/.git/worktrees/j-62",
	}

	if got["type"] != "workspaceWrite" {
		t.Fatalf("sandboxPolicy.type = %v, want workspaceWrite", got["type"])
	}
	roots, ok := got["writableRoots"].([]string)
	if !ok {
		t.Fatalf("sandboxPolicy.writableRoots type = %T, want []string", got["writableRoots"])
	}
	if len(roots) != len(wantRoots) {
		t.Fatalf("len(writableRoots) = %d, want %d (%v)", len(roots), len(wantRoots), roots)
	}
	for i, want := range wantRoots {
		if roots[i] != want {
			t.Fatalf("writableRoots[%d] = %q, want %q", i, roots[i], want)
		}
	}
}

func TestBuildTurnSandboxPolicyFallsBackToThreadSandboxWorkspaceWrite(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		ThreadSandbox: "workspace-write",
		AdditionalWritableDirs: []string{
			"/tmp/repo/.git",
			"/tmp/repo/.git/worktrees/j-62",
		},
	}

	got := buildTurnSandboxPolicy(cfg)
	if got["type"] != "workspaceWrite" {
		t.Fatalf("sandboxPolicy.type = %v, want workspaceWrite", got["type"])
	}
	roots, ok := got["writableRoots"].([]string)
	if !ok {
		t.Fatalf("sandboxPolicy.writableRoots type = %T, want []string", got["writableRoots"])
	}
	want := []string{
		"/tmp/repo/.git",
		"/tmp/repo/.git/worktrees/j-62",
	}
	if len(roots) != len(want) {
		t.Fatalf("len(writableRoots) = %d, want %d (%v)", len(roots), len(want), roots)
	}
	for i, root := range want {
		if roots[i] != root {
			t.Fatalf("writableRoots[%d] = %q, want %q", i, roots[i], root)
		}
	}
}

func TestBuildTurnSandboxPolicyMapsSandboxTypesToProtocolValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{name: "read-only", input: "read-only", expect: "readOnly"},
		{name: "external-sandbox", input: "external-sandbox", expect: "externalSandbox"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildTurnSandboxPolicy(&Config{TurnSandboxPolicy: tt.input})
			if got["type"] != tt.expect {
				t.Fatalf("sandboxPolicy.type = %v, want %s", got["type"], tt.expect)
			}
			if _, ok := got["writableRoots"]; ok {
				t.Fatalf("sandboxPolicy.writableRoots present for %s, want omitted", tt.input)
			}
		})
	}
}

func TestExtractDynamicToolRequestReadsTopLevelToolCall(t *testing.T) {
	t.Parallel()

	toolName, input := extractDynamicToolRequest(map[string]any{
		"name": "linear_graphql",
		"input": map[string]any{
			"query": "query { viewer { id } }",
		},
	})

	if toolName != "linear_graphql" {
		t.Fatalf("toolName = %q, want linear_graphql", toolName)
	}
	if got, _ := input["query"].(string); got != "query { viewer { id } }" {
		t.Fatalf("input.query = %q, want GraphQL query", got)
	}
}

func TestExtractDynamicToolRequestReadsTopLevelToolField(t *testing.T) {
	t.Parallel()

	toolName, input := extractDynamicToolRequest(map[string]any{
		"tool": "linear_graphql",
		"arguments": map[string]any{
			"query": "query { issue(id: \"J-75\") { id } }",
		},
	})

	if toolName != "linear_graphql" {
		t.Fatalf("toolName = %q, want linear_graphql", toolName)
	}
	if got, _ := input["query"].(string); got != "query { issue(id: \"J-75\") { id } }" {
		t.Fatalf("input.query = %q, want query", got)
	}
}

func TestExtractDynamicToolRequestReadsNestedToolCallArgumentsString(t *testing.T) {
	t.Parallel()

	toolName, input := extractDynamicToolRequest(map[string]any{
		"toolCall": map[string]any{
			"toolName":  "linear_graphql",
			"arguments": "{\"query\":\"mutation { issueUpdate { success } }\",\"variables\":{\"id\":\"issue-1\"}}",
		},
	})

	if toolName != "linear_graphql" {
		t.Fatalf("toolName = %q, want linear_graphql", toolName)
	}
	if got, _ := input["query"].(string); got != "mutation { issueUpdate { success } }" {
		t.Fatalf("input.query = %q, want mutation", got)
	}
	vars, _ := input["variables"].(map[string]any)
	if got, _ := vars["id"].(string); got != "issue-1" {
		t.Fatalf("input.variables.id = %q, want issue-1", got)
	}
}

func TestExtractDynamicToolRequestReadsNestedItemWithTopLevelInput(t *testing.T) {
	t.Parallel()

	toolName, input := extractDynamicToolRequest(map[string]any{
		"item": map[string]any{
			"name": "linear_graphql",
		},
		"input": map[string]any{
			"query": "query { issue(id: \"1\") { id } }",
		},
	})

	if toolName != "linear_graphql" {
		t.Fatalf("toolName = %q, want linear_graphql", toolName)
	}
	if got, _ := input["query"].(string); got != "query { issue(id: \"1\") { id } }" {
		t.Fatalf("input.query = %q, want query", got)
	}
}

func fillNotifQueue(r *Runner) {
	for i := 0; i < cap(r.notifCh); i++ {
		r.notifCh <- &Incoming{Notif: &Notification{Method: methodRateLimits}}
	}
}

func mustNotification(t *testing.T, method string, params map[string]any) []byte {
	t.Helper()

	msg, err := FormatNotification(method, params)
	if err != nil {
		t.Fatalf("FormatNotification(%q): %v", method, err)
	}
	return msg
}
