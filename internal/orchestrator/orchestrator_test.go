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
	"symphony/internal/types"
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

func TestOnRetryTimerWithoutTrackerReleasesClaim(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)
	entry := &RetryEntry{
		IssueID:    "issue-1",
		Identifier: "J-18",
		Kind:       RetryKindFailure,
		Attempt:    2,
		DueAt:      time.Now(),
	}

	o.state.mu.Lock()
	o.state.RetryQueue[entry.IssueID] = entry
	o.state.Claimed[entry.IssueID] = struct{}{}
	o.state.mu.Unlock()

	o.onRetryTimer(context.Background(), config.New(nil), entry.IssueID)

	o.state.mu.Lock()
	defer o.state.mu.Unlock()

	if _, ok := o.state.RetryQueue[entry.IssueID]; ok {
		t.Fatal("retry entry should be removed when tracker is unavailable")
	}
	if _, ok := o.state.Claimed[entry.IssueID]; ok {
		t.Fatal("claimed issue should be released when tracker is unavailable")
	}
}

func TestOnRetryTimerPollFailurePreservesRetrySemantics(t *testing.T) {
	t.Parallel()

	client, server := newLinearFailingClient(t, http.StatusInternalServerError, "boom")
	defer server.Close()

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"project_slug":  "proj",
			"active_states": []any{"In Progress"},
		},
		"agent": map[string]any{
			"max_retry_backoff_ms": 300000,
		},
	})

	tests := []struct {
		name      string
		entry     *RetryEntry
		delayMin  time.Duration
		delayMax  time.Duration
	}{
		{
			name: "failure",
			entry: &RetryEntry{
				IssueID:      "issue-failure",
				Identifier:   "J-21",
				Kind:         RetryKindFailure,
				Attempt:      3,
				FailureCount: 2,
				DueAt:        time.Now(),
			},
			delayMin: 19 * time.Second,
			delayMax: 21 * time.Second,
		},
		{
			name: "capacity",
			entry: &RetryEntry{
				IssueID:      "issue-capacity",
				Identifier:   "J-22",
				Kind:         RetryKindCapacity,
				Attempt:      3,
				FailureCount: 1,
				DeferCount:   4,
				DueAt:        time.Now(),
			},
			delayMin: 4 * time.Second,
			delayMax: 6 * time.Second,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			o := New("", 0, "alpha", nil)
			o.tracker = client

			o.state.mu.Lock()
			o.state.RetryQueue[tc.entry.IssueID] = tc.entry
			o.state.Claimed[tc.entry.IssueID] = struct{}{}
			o.state.mu.Unlock()

			before := time.Now()
			o.onRetryTimer(context.Background(), cfg, tc.entry.IssueID)
			stopRetryTimer(t, o, tc.entry.IssueID)

			o.state.mu.Lock()
			defer o.state.mu.Unlock()

			entry, ok := o.state.RetryQueue[tc.entry.IssueID]
			if !ok {
				t.Fatal("expected retry entry to be rescheduled after poll failure")
			}
			if entry.Kind != tc.entry.Kind {
				t.Fatalf("retry kind = %q, want %q", entry.Kind, tc.entry.Kind)
			}
			if entry.Attempt != tc.entry.Attempt {
				t.Fatalf("retry attempt = %d, want %d", entry.Attempt, tc.entry.Attempt)
			}
			if entry.FailureCount != tc.entry.FailureCount {
				t.Fatalf("retry failure_count = %d, want %d", entry.FailureCount, tc.entry.FailureCount)
			}
			if entry.DeferCount != tc.entry.DeferCount {
				t.Fatalf("retry defer_count = %d, want %d", entry.DeferCount, tc.entry.DeferCount)
			}
			if entry.Error != "poll failed" {
				t.Fatalf("retry error = %q, want %q", entry.Error, "poll failed")
			}
			delay := entry.DueAt.Sub(before)
			if delay < tc.delayMin || delay > tc.delayMax {
				t.Fatalf("retry delay = %v, want between %v and %v", delay, tc.delayMin, tc.delayMax)
			}
		})
	}
}

func TestRunningConcurrentCountExcludesHumanReview(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)

	o.state.mu.Lock()
	o.state.Running["issue-1"] = &RunAttempt{IssueID: "issue-1", IssueState: "Human Review"}
	o.state.Running["issue-2"] = &RunAttempt{IssueID: "issue-2", IssueState: "In Progress"}
	got := o.runningConcurrentCountLocked()
	o.state.mu.Unlock()

	if got != 1 {
		t.Fatalf("runningConcurrentCountLocked() = %d, want 1", got)
	}
}

func TestCanDispatchIgnoresHumanReviewForConcurrencyLimit(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)

	o.state.mu.Lock()
	o.state.MaxConcurrentAgents = 1
	o.state.Running["issue-review"] = &RunAttempt{
		IssueID:    "issue-review",
		Identifier: "J-10",
		IssueState: "Human Review",
	}
	o.state.mu.Unlock()

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"active_states": []any{"Todo", "In Progress", "Human Review"},
		},
	})

	issue := &types.Issue{ID: "issue-1", Identifier: "J-27", State: "In Progress"}
	if !o.canDispatch(cfg, issue) {
		t.Fatal("human review issue should not consume a concurrent slot")
	}
}

func TestHasGlobalCapacityForStateIgnoresHumanReview(t *testing.T) {
	t.Parallel()

	limiter := NewSessionLimiter(1)
	if !limiter.TryAcquire() {
		t.Fatal("expected limiter warm-up acquire to succeed")
	}

	o := New("", 0, "alpha", limiter)

	if o.hasGlobalCapacityForState("In Progress") {
		t.Fatal("non-review issue should respect the global limiter")
	}
	if !o.hasGlobalCapacityForState("Human Review") {
		t.Fatal("human review issue should bypass the global limiter")
	}
}

func TestWatchWorkflowReloadsOnFileChange(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflowFile(t, workflowPath, 1000, 2000)

	o := New(workflowPath, 0, "alpha", nil)
	o.workflowWatchDebounce = 20 * time.Millisecond
	if err := o.reloadWorkflow(); err != nil {
		t.Fatalf("initial reload: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		o.watchWorkflow(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	time.Sleep(50 * time.Millisecond)
	writeWorkflowFile(t, workflowPath, 1500, 2500)

	waitForOrchestrator(t, func() bool {
		o.state.mu.Lock()
		defer o.state.mu.Unlock()
		return o.state.PollIntervalMs == 1500 &&
			o.state.PollIntervalIdleMs == 2500 &&
			o.state.MaxConcurrentAgents == 4
	})
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
	if _, ok := o.state.RetryQueue[attempt.IssueID]; ok {
		t.Fatal("success path should not schedule a follow-up retry")
	}
	if _, ok := o.state.Claimed[attempt.IssueID]; ok {
		t.Fatal("success path should release the claimed issue")
	}
}

func TestOnWorkerDoneSuccessLeavesIssueEligibleForNextPoll(t *testing.T) {
	t.Parallel()

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"active_states": []any{"In Progress"},
		},
	})

	o := New("", 0, "alpha", nil)
	attempt := &RunAttempt{
		IssueID:    "issue-1",
		Identifier: "J-21",
		Attempt:    1,
		StartedAt:  time.Now().Add(-30 * time.Second),
		Status:     StatusSucceeded,
		IssueState: "In Progress",
	}

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.Claimed[attempt.IssueID] = struct{}{}
	o.state.mu.Unlock()

	o.onWorkerDone(context.Background(), cfg, attempt.IssueID, attempt, nil)

	issue := &types.Issue{ID: attempt.IssueID, Identifier: attempt.Identifier, State: "In Progress"}
	if !o.canDispatch(cfg, issue) {
		t.Fatal("successful active issue should be eligible for natural redispatch on the next poll")
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
	entry, ok := o.state.RetryQueue[attempt.IssueID]
	if !ok {
		t.Fatal("expected retry to be scheduled for intermediate failure")
	}
	if entry.Kind != RetryKindFailure {
		t.Fatalf("retry kind = %q, want %q", entry.Kind, RetryKindFailure)
	}
	if entry.Attempt != 2 {
		t.Fatalf("retry attempt = %d, want 2", entry.Attempt)
	}
	if entry.FailureCount != 1 {
		t.Fatalf("retry failure_count = %d, want 1", entry.FailureCount)
	}
	if entry.DeferCount != 0 {
		t.Fatalf("retry defer_count = %d, want 0", entry.DeferCount)
	}
}

func TestScheduleFailureRetryUsesFailureCountForBackoff(t *testing.T) {
	t.Parallel()

	cfg := config.New(map[string]any{
		"agent": map[string]any{
			"max_retry_backoff_ms": 300000,
		},
	})

	o := New("", 0, "alpha", nil)
	before := time.Now()
	o.scheduleFailureRetry(context.Background(), cfg, "issue-1", "J-21", 5, 1, "boom")
	stopRetryTimer(t, o, "issue-1")

	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	entry, ok := o.state.RetryQueue["issue-1"]
	if !ok {
		t.Fatal("expected retry entry to be scheduled")
	}
	if entry.Kind != RetryKindFailure {
		t.Fatalf("retry kind = %q, want %q", entry.Kind, RetryKindFailure)
	}
	if entry.Attempt != 5 {
		t.Fatalf("retry attempt = %d, want 5", entry.Attempt)
	}
	if entry.FailureCount != 1 {
		t.Fatalf("retry failure_count = %d, want 1", entry.FailureCount)
	}
	delay := entry.DueAt.Sub(before)
	if delay < 9*time.Second || delay > 11*time.Second {
		t.Fatalf("failure retry delay = %v, want about 10s", delay)
	}
}

func TestOnRetryTimerCapacityWaitKeepsFailureAttempt(t *testing.T) {
	t.Parallel()

	client, server := newLinearCandidateClient(t, []*types.Issue{
		{ID: "issue-1", Identifier: "J-21", State: "In Progress"},
	})
	defer server.Close()

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"project_slug":  "proj",
			"active_states": []any{"In Progress"},
		},
		"agent": map[string]any{
			"max_concurrent_agents": 1,
		},
	})

	o := New("", 0, "alpha", nil)
	o.tracker = client

	o.state.mu.Lock()
	o.state.MaxConcurrentAgents = 1
	o.state.Running["issue-busy"] = &RunAttempt{IssueID: "issue-busy", Identifier: "J-99", IssueState: "In Progress"}
	o.state.RetryQueue["issue-1"] = &RetryEntry{
		IssueID:      "issue-1",
		Identifier:   "J-21",
		Kind:         RetryKindFailure,
		Attempt:      2,
		FailureCount: 1,
		DueAt:        time.Now(),
	}
	o.state.Claimed["issue-1"] = struct{}{}
	o.state.mu.Unlock()

	before := time.Now()
	o.onRetryTimer(context.Background(), cfg, "issue-1")
	stopRetryTimer(t, o, "issue-1")

	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	entry, ok := o.state.RetryQueue["issue-1"]
	if !ok {
		t.Fatal("capacity wait should keep the issue in retry queue")
	}
	if entry.Kind != RetryKindCapacity {
		t.Fatalf("retry kind = %q, want %q", entry.Kind, RetryKindCapacity)
	}
	if entry.Attempt != 2 {
		t.Fatalf("retry attempt = %d, want 2", entry.Attempt)
	}
	if entry.FailureCount != 1 {
		t.Fatalf("retry failure_count = %d, want 1", entry.FailureCount)
	}
	if entry.DeferCount != 1 {
		t.Fatalf("retry defer_count = %d, want 1", entry.DeferCount)
	}
	delay := entry.DueAt.Sub(before)
	if delay < 4*time.Second || delay > 6*time.Second {
		t.Fatalf("capacity retry delay = %v, want about 5s", delay)
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

func newLinearCandidateClient(t *testing.T, issues []*types.Issue) (*tracker.LinearClient, *httptest.Server) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		var req struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		if !strings.Contains(req.Query, "issues(") {
			t.Fatalf("unexpected query: %s", req.Query)
		}

		nodes := make([]map[string]any, 0, len(issues))
		for _, issue := range issues {
			nodes = append(nodes, map[string]any{
				"id":          issue.ID,
				"identifier":  issue.Identifier,
				"title":       issue.Title,
				"description": issue.Description,
				"priority":    nil,
				"state":       map[string]any{"name": issue.State},
				"branchName":  issue.BranchName,
				"url":         issue.URL,
				"comments":    map[string]any{"nodes": []any{}},
				"labels":      map[string]any{"nodes": []any{}},
				"relations":   map[string]any{"nodes": []any{}},
				"createdAt":   time.Now().UTC().Format(time.RFC3339),
				"updatedAt":   time.Now().UTC().Format(time.RFC3339),
			})
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"pageInfo": map[string]any{
						"hasNextPage": false,
						"endCursor":   "",
					},
					"nodes": nodes,
				},
			},
		})
	}))

	client, err := tracker.NewLinearClient("test-key", server.URL, "proj", []string{"In Progress"})
	if err != nil {
		server.Close()
		t.Fatalf("NewLinearClient: %v", err)
	}
	return client, server
}

func newLinearFailingClient(t *testing.T, status int, body string) (*tracker.LinearClient, *httptest.Server) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, body, status)
	}))

	client, err := tracker.NewLinearClient("test-key", server.URL, "proj", []string{"In Progress"})
	if err != nil {
		server.Close()
		t.Fatalf("NewLinearClient: %v", err)
	}
	return client, server
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

func writeWorkflowFile(t *testing.T, path string, intervalMs, idleIntervalMs int) {
	t.Helper()
	content := []byte(
		"---\n" +
			"tracker:\n" +
			"  api_key: test-key\n" +
			"  project_slug: test-project\n" +
			"polling:\n" +
			"  interval_ms: " + itoa(intervalMs) + "\n" +
			"  idle_interval_ms: " + itoa(idleIntervalMs) + "\n" +
			"agent:\n" +
			"  max_concurrent_agents: 4\n" +
			"workspace:\n" +
			"  root: /tmp/symphony\n" +
			"---\n" +
			"# Workflow\n",
	)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
}

func waitForOrchestrator(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func itoa(v int) string {
	return fmt.Sprintf("%d", v)
}
