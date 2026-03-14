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

func TestBuildAutoUserInputResponseApprovesAppToolPrompt(t *testing.T) {
	t.Parallel()

	response, ok := buildAutoUserInputResponse(map[string]any{
		"questions": []any{
			map[string]any{
				"id":     "mcp_tool_call_approval_call_123",
				"header": "Approve app tool call?",
				"options": []any{
					map[string]any{"label": "Approve Once"},
					map[string]any{"label": "Approve this session"},
					map[string]any{"label": "Cancel"},
				},
			},
		},
	})

	if !ok {
		t.Fatal("buildAutoUserInputResponse() = false, want true")
	}
	answers, _ := response["answers"].(map[string]any)
	if got, _ := answers["mcp_tool_call_approval_call_123"].(string); got != "Approve Once" {
		t.Fatalf("answers[id] = %q, want Approve Once", got)
	}
}

func TestBuildAutoUserInputResponseSkipsNonApprovalPrompt(t *testing.T) {
	t.Parallel()

	_, ok := buildAutoUserInputResponse(map[string]any{
		"questions": []any{
			map[string]any{
				"id":     "plain_question",
				"header": "Need more details",
				"options": []any{
					map[string]any{"label": "Foo"},
					map[string]any{"label": "Bar"},
				},
			},
		},
	})

	if ok {
		t.Fatal("buildAutoUserInputResponse() = true, want false")
	}
}

func TestContainsApprovalWord(t *testing.T) {
	t.Parallel()

	tests := []struct {
		text string
		want bool
	}{
		{"approve this action", true},
		{"Approve Once", true},
		{"do you approve?", true},
		{"disapprove this", false},
		{"need more details", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := containsApprovalWord(strings.ToLower(tc.text)); got != tc.want {
			t.Errorf("containsApprovalWord(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestLooksLikeApprovalQuestionRejectsDisapprove(t *testing.T) {
	t.Parallel()

	got := looksLikeApprovalQuestion(
		map[string]any{"header": "Do you disapprove of this change?"},
		[]any{
			map[string]any{"label": "Yes"},
			map[string]any{"label": "No"},
		},
	)
	if got {
		t.Fatal("looksLikeApprovalQuestion() = true for 'disapprove', want false")
	}
}

func TestSelectAutoUserInputAnswerRejectsUnknownOptions(t *testing.T) {
	t.Parallel()

	// Options that contain "approve" in the header but no known "Approve Once"/"Approve this session"
	_, ok := selectAutoUserInputAnswer(map[string]any{
		"header": "Approve something?",
		"options": []any{
			map[string]any{"label": "Do it"},
			map[string]any{"label": "Skip"},
		},
	})
	if ok {
		t.Fatal("selectAutoUserInputAnswer() = true for unknown options, want false")
	}
}

func TestNormalizeSandboxPolicyNone(t *testing.T) {
	t.Parallel()

	if got := normalizeSandboxPolicy("none"); got != "" {
		t.Fatalf("normalizeSandboxPolicy(\"none\") = %q, want empty", got)
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
