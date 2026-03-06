package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"symphony/internal/config"
	"symphony/internal/tracker"
)

func TestOnWorkerDoneDuringDrainDoesNotScheduleRetry(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)
	attempt := &RunAttempt{IssueID: "issue-1", Identifier: "J-18", Attempt: 1}

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.Claimed[attempt.IssueID] = struct{}{}
	o.state.mu.Unlock()

	o.BeginDrain()
	o.onWorkerDone(context.Background(), config.New(nil), attempt.IssueID, attempt, nil)

	o.state.mu.Lock()
	defer o.state.mu.Unlock()

	if !o.state.Draining {
		t.Fatal("expected orchestrator to stay in draining mode")
	}
	if _, ok := o.state.Running[attempt.IssueID]; ok {
		t.Fatal("running issue should be removed after worker completion")
	}
	if _, ok := o.state.Claimed[attempt.IssueID]; ok {
		t.Fatal("claimed issue should be released during drain")
	}
	if len(o.state.RetryQueue) != 0 {
		t.Fatalf("retry queue should stay empty during drain, got %d entries", len(o.state.RetryQueue))
	}
	if o.state.CompletedCount != 1 {
		t.Fatalf("completed count = %d, want 1", o.state.CompletedCount)
	}
}

func TestOnRetryTimerDuringDrainClearsClaimWithoutRedispatch(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)
	entry := &RetryEntry{
		IssueID:    "issue-1",
		Identifier: "J-18",
		Attempt:    2,
		DueAt:      time.Now(),
	}

	o.state.mu.Lock()
	o.state.Draining = true
	o.state.RetryQueue[entry.IssueID] = entry
	o.state.Claimed[entry.IssueID] = struct{}{}
	o.state.mu.Unlock()

	o.onRetryTimer(context.Background(), config.New(nil), entry.IssueID)

	o.state.mu.Lock()
	defer o.state.mu.Unlock()

	if _, ok := o.state.RetryQueue[entry.IssueID]; ok {
		t.Fatal("retry entry should be removed once timer fires")
	}
	if _, ok := o.state.Claimed[entry.IssueID]; ok {
		t.Fatal("claimed issue should be released when retry is skipped during drain")
	}
	if len(o.state.Running) != 0 {
		t.Fatalf("no issue should be redispatched during drain, got %d running", len(o.state.Running))
	}
}

func TestOnWorkerDoneSuccessPostsCommentAndTransitionsState(t *testing.T) {
	t.Parallel()

	wsPath := initGitWorkspace(t)
	recorder, server := newLinearRecorderServer(t)
	defer server.Close()

	client, err := tracker.NewLinearClient("test-key", server.URL, "proj", []string{"In Progress"})
	if err != nil {
		t.Fatalf("NewLinearClient: %v", err)
	}

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"on_success_state": "Human Review",
		},
		"agent": map[string]any{
			"max_attempts": 2,
			"max_turns":    3,
		},
	})

	o := New("", 0, "alpha", nil)
	o.tracker = client
	attempt := &RunAttempt{
		IssueID:       "issue-1",
		Identifier:    "J-29",
		Attempt:       1,
		WorkspacePath: wsPath,
		StartedAt:     time.Now().Add(-(3*time.Minute + 4*time.Second)),
		Status:        StatusSucceeded,
		Session: LiveSession{
			TurnCount:    2,
			InputTokens:  28000,
			OutputTokens: 14100,
			TotalTokens:  42100,
		},
	}

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.Claimed[attempt.IssueID] = struct{}{}
	o.state.mu.Unlock()

	o.onWorkerDone(context.Background(), cfg, attempt.IssueID, attempt, nil)

	stopRetryTimer(t, o, attempt.IssueID)

	if recorder.commentCount() != 1 {
		t.Fatalf("comment count = %d, want 1", recorder.commentCount())
	}
	body := recorder.lastComment()
	for _, want := range []string{
		"✅ **Symphony agent completed** (attempt 1, turn 2/3)",
		"**Tokens:** 42,100 (in: 28,000 / out: 14,100)",
		"- Modified: foo.txt",
		"- Last commit: `feat: add foo (",
		"- PR: https://github.com/J132134/symphony/pull/new/j-29-test",
		"**Branch:** `j-29-test`",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment body missing %q:\n%s", want, body)
		}
	}
	if got := recorder.lastState(); got != "Human Review" {
		t.Fatalf("state transition = %q, want Human Review", got)
	}

	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	if o.state.CompletedCount != 1 {
		t.Fatalf("completed count = %d, want 1", o.state.CompletedCount)
	}
	if _, ok := o.state.RetryQueue[attempt.IssueID]; !ok {
		t.Fatal("expected success path to schedule follow-up retry")
	}
}

func TestOnWorkerDoneFinalFailurePostsCommentWithoutRetry(t *testing.T) {
	t.Parallel()

	recorder, server := newLinearRecorderServer(t)
	defer server.Close()

	client, err := tracker.NewLinearClient("test-key", server.URL, "proj", []string{"In Progress"})
	if err != nil {
		t.Fatalf("NewLinearClient: %v", err)
	}

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"on_failure_state": "Rework",
		},
		"agent": map[string]any{
			"max_attempts": 2,
		},
	})

	o := New("", 0, "alpha", nil)
	o.tracker = client
	attempt := &RunAttempt{
		IssueID:    "issue-1",
		Identifier: "J-29",
		Attempt:    2,
		StartedAt:  time.Now().Add(-5 * time.Minute),
		Status:     StatusStalled,
		Session: LiveSession{
			TurnCount: 1,
		},
	}

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.Claimed[attempt.IssueID] = struct{}{}
	o.state.mu.Unlock()

	runErr := fmt.Errorf("turn failed: stall_timeout")
	o.onWorkerDone(context.Background(), cfg, attempt.IssueID, attempt, runErr)

	if recorder.commentCount() != 1 {
		t.Fatalf("comment count = %d, want 1", recorder.commentCount())
	}
	body := recorder.lastComment()
	for _, want := range []string{
		"❌ **Symphony agent failed** (attempt 2/2)",
		"**Error:** turn failed: stall_timeout",
		"**Duration:** 5m 0s (stalled)",
		"**Last status:** stalled (turn 1)",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment body missing %q:\n%s", want, body)
		}
	}
	if got := recorder.lastState(); got != "Rework" {
		t.Fatalf("state transition = %q, want Rework", got)
	}

	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	if _, ok := o.state.RetryQueue[attempt.IssueID]; ok {
		t.Fatal("final failure should not schedule a retry")
	}
	if _, ok := o.state.Claimed[attempt.IssueID]; ok {
		t.Fatal("final failure should release the claimed issue")
	}
}

func TestOnWorkerDoneIntermediateFailureRetriesQuietly(t *testing.T) {
	t.Parallel()

	recorder, server := newLinearRecorderServer(t)
	defer server.Close()

	client, err := tracker.NewLinearClient("test-key", server.URL, "proj", []string{"In Progress"})
	if err != nil {
		t.Fatalf("NewLinearClient: %v", err)
	}

	cfg := config.New(map[string]any{
		"agent": map[string]any{
			"max_attempts": 3,
		},
	})

	o := New("", 0, "alpha", nil)
	o.tracker = client
	attempt := &RunAttempt{
		IssueID:    "issue-1",
		Identifier: "J-29",
		Attempt:    1,
		StartedAt:  time.Now().Add(-30 * time.Second),
		Status:     StatusFailed,
	}

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.Claimed[attempt.IssueID] = struct{}{}
	o.state.mu.Unlock()

	o.onWorkerDone(context.Background(), cfg, attempt.IssueID, attempt, fmt.Errorf("boom"))
	stopRetryTimer(t, o, attempt.IssueID)

	if recorder.commentCount() != 0 {
		t.Fatalf("comment count = %d, want 0", recorder.commentCount())
	}
	if recorder.lastState() != "" {
		t.Fatalf("unexpected state transition = %q", recorder.lastState())
	}

	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	if _, ok := o.state.RetryQueue[attempt.IssueID]; !ok {
		t.Fatal("expected retry to be scheduled for intermediate failure")
	}
}

type linearRecorder struct {
	mu         sync.Mutex
	comments   []string
	stateNames []string
}

func (r *linearRecorder) addComment(body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.comments = append(r.comments, body)
}

func (r *linearRecorder) addState(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stateNames = append(r.stateNames, name)
}

func (r *linearRecorder) commentCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.comments)
}

func (r *linearRecorder) lastComment() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.comments) == 0 {
		return ""
	}
	return r.comments[len(r.comments)-1]
}

func (r *linearRecorder) lastState() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.stateNames) == 0 {
		return ""
	}
	return r.stateNames[len(r.stateNames)-1]
}

func newLinearRecorderServer(t *testing.T) (*linearRecorder, *httptest.Server) {
	t.Helper()

	recorder := &linearRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.Query, "commentCreate"):
			input, _ := req.Variables["input"].(map[string]any)
			recorder.addComment(asString(input["body"]))
			_, _ = w.Write([]byte(`{"data":{"commentCreate":{"success":true}}}`))
		case strings.Contains(req.Query, "issue(id: $id)"):
			_, _ = w.Write([]byte(`{"data":{"issue":{"team":{"states":{"nodes":[{"id":"state-human-review","name":"Human Review"},{"id":"state-rework","name":"Rework"}]}}}}}`))
		case strings.Contains(req.Query, "issueUpdate"):
			switch asString(req.Variables["stateId"]) {
			case "state-human-review":
				recorder.addState("Human Review")
			case "state-rework":
				recorder.addState("Rework")
			default:
				recorder.addState(asString(req.Variables["stateId"]))
			}
			_, _ = w.Write([]byte(`{"data":{"issueUpdate":{"success":true}}}`))
		default:
			t.Fatalf("unexpected query: %s", req.Query)
		}
	}))

	return recorder, server
}

func initGitWorkspace(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Test User")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "checkout", "-b", "j-29-test")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/J132134/symphony.git")

	path := filepath.Join(dir, "foo.txt")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write foo.txt: %v", err)
	}
	runGit(t, dir, "add", "foo.txt")
	runGit(t, dir, "commit", "-m", "feat: add foo")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func stopRetryTimer(t *testing.T, o *Orchestrator, issueID string) {
	t.Helper()
	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	entry, ok := o.state.RetryQueue[issueID]
	if !ok || entry.timer == nil {
		return
	}
	entry.timer.Stop()
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
