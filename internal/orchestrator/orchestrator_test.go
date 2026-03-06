package orchestrator

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestOnRetryTimerKeepsAttemptAndFailureCountWhenNoSlots(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[{"id":"issue-1","identifier":"J-27","title":"retry","description":"","priority":0,"state":{"name":"In Progress"},"branchName":"","url":"","labels":{"nodes":[]},"relations":{"nodes":[]},"createdAt":"2026-03-06T00:00:00Z","updatedAt":"2026-03-06T00:00:00Z"}]}}}`))
	}))
	defer srv.Close()

	tr, err := tracker.NewLinearClient("token", srv.URL, "proj", []string{"In Progress"})
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	o := New("", 0, "alpha", nil)
	o.tracker = tr

	entry := &RetryEntry{
		IssueID:      "issue-1",
		Identifier:   "J-27",
		Attempt:      3,
		FailureCount: 2,
		DueAt:        time.Now(),
	}

	o.state.mu.Lock()
	o.state.MaxConcurrentAgents = 0
	o.state.RetryQueue[entry.IssueID] = entry
	o.state.Claimed[entry.IssueID] = struct{}{}
	o.state.mu.Unlock()

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"active_states": []any{"In Progress"},
		},
	})

	o.onRetryTimer(context.Background(), cfg, entry.IssueID)

	o.state.mu.Lock()
	defer o.state.mu.Unlock()

	rescheduled, ok := o.state.RetryQueue[entry.IssueID]
	if !ok {
		t.Fatal("retry entry should be rescheduled")
	}
	if rescheduled.timer != nil {
		rescheduled.timer.Stop()
	}
	if rescheduled.Attempt != 3 {
		t.Fatalf("retry attempt = %d, want 3", rescheduled.Attempt)
	}
	if rescheduled.FailureCount != 2 {
		t.Fatalf("retry failure_count = %d, want 2", rescheduled.FailureCount)
	}
}

func TestOnRetryTimerReleasesClaimWhenIssueBecomesInactive(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[{"id":"issue-1","identifier":"J-27","title":"retry","description":"","priority":0,"state":{"name":"Done"},"branchName":"","url":"","labels":{"nodes":[]},"relations":{"nodes":[]},"createdAt":"2026-03-06T00:00:00Z","updatedAt":"2026-03-06T00:00:00Z"}]}}}`))
	}))
	defer srv.Close()

	tr, err := tracker.NewLinearClient("token", srv.URL, "proj", []string{"In Progress", "Done"})
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	o := New("", 0, "alpha", nil)
	o.tracker = tr

	entry := &RetryEntry{
		IssueID:      "issue-1",
		Identifier:   "J-27",
		Attempt:      3,
		FailureCount: 2,
		DueAt:        time.Now(),
	}

	o.state.mu.Lock()
	o.state.MaxConcurrentAgents = 1
	o.state.RetryQueue[entry.IssueID] = entry
	o.state.Claimed[entry.IssueID] = struct{}{}
	o.state.mu.Unlock()

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"active_states":   []any{"In Progress"},
			"terminal_states": []any{"Done"},
		},
	})

	o.onRetryTimer(context.Background(), cfg, entry.IssueID)

	o.state.mu.Lock()
	defer o.state.mu.Unlock()

	if _, ok := o.state.RetryQueue[entry.IssueID]; ok {
		t.Fatal("retry entry should be cleared when issue becomes inactive")
	}
	if _, ok := o.state.Claimed[entry.IssueID]; ok {
		t.Fatal("claimed issue should be released when issue becomes inactive")
	}
	if _, ok := o.state.Abandoned[entry.IssueID]; ok {
		t.Fatal("inactive issue should not be marked abandoned by retry timer")
	}
}

func TestOnWorkerDoneFailureSchedulesIncrementedRetry(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)
	attempt := &RunAttempt{IssueID: "issue-1", Identifier: "J-27", Attempt: 1}

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.Claimed[attempt.IssueID] = struct{}{}
	o.state.mu.Unlock()

	o.onWorkerDone(context.Background(), config.New(nil), attempt.IssueID, attempt, errors.New("boom"))

	o.state.mu.Lock()
	defer o.state.mu.Unlock()

	entry, ok := o.state.RetryQueue[attempt.IssueID]
	if !ok {
		t.Fatal("retry entry should be scheduled")
	}
	if entry.timer != nil {
		entry.timer.Stop()
	}
	if entry.Attempt != 2 {
		t.Fatalf("retry attempt = %d, want 2", entry.Attempt)
	}
	if entry.FailureCount != 1 {
		t.Fatalf("retry failure_count = %d, want 1", entry.FailureCount)
	}
}

func TestOnWorkerDoneSuccessKeepsAttemptNumber(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)
	attempt := &RunAttempt{IssueID: "issue-1", Identifier: "J-27", Attempt: 1, FailureCount: 3}

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.Claimed[attempt.IssueID] = struct{}{}
	o.state.mu.Unlock()

	o.onWorkerDone(context.Background(), config.New(nil), attempt.IssueID, attempt, nil)

	o.state.mu.Lock()
	defer o.state.mu.Unlock()

	entry, ok := o.state.RetryQueue[attempt.IssueID]
	if !ok {
		t.Fatal("retry entry should be scheduled")
	}
	if entry.timer != nil {
		entry.timer.Stop()
	}
	if entry.Attempt != 1 {
		t.Fatalf("retry attempt = %d, want 1", entry.Attempt)
	}
	if entry.FailureCount != 0 {
		t.Fatalf("retry failure_count = %d, want 0", entry.FailureCount)
	}
}

func TestShouldAbandonRetryOnlyAfterExceedingMaxAttempts(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)

	disabled := config.New(map[string]any{
		"agent": map[string]any{
			"max_retry_attempts": 0,
		},
	})
	if o.shouldAbandonRetry(disabled, 100) {
		t.Fatal("disabled max_retry_attempts should never abandon")
	}

	cfg := config.New(map[string]any{
		"agent": map[string]any{
			"max_retry_attempts": 2,
		},
	})
	if o.shouldAbandonRetry(cfg, 2) {
		t.Fatal("failure count equal to max should not abandon")
	}
	if !o.shouldAbandonRetry(cfg, 3) {
		t.Fatal("failure count above max should abandon")
	}
}

func TestOnWorkerDoneFailureAbandonsAfterMaxRetryAttempts(t *testing.T) {
	t.Parallel()

	var capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		capturedBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"commentCreate":{"success":true}}}`))
	}))
	defer srv.Close()

	tr, err := tracker.NewLinearClient("token", srv.URL, "proj", []string{"Todo", "In Progress"})
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	o := New("", 0, "alpha", nil)
	o.tracker = tr

	attempt := &RunAttempt{
		IssueID:      "issue-1",
		Identifier:   "J-27",
		Attempt:      2,
		FailureCount: 1,
		IssueState:   "In Progress",
	}

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.Claimed[attempt.IssueID] = struct{}{}
	o.state.mu.Unlock()

	cfg := config.New(map[string]any{
		"agent": map[string]any{
			"max_retry_attempts": 1,
		},
	})

	o.onWorkerDone(context.Background(), cfg, attempt.IssueID, attempt, errors.New("fatal setup"))

	o.state.mu.Lock()
	defer o.state.mu.Unlock()

	if _, ok := o.state.RetryQueue[attempt.IssueID]; ok {
		t.Fatal("retry entry should not exist after abandon")
	}
	if _, ok := o.state.Claimed[attempt.IssueID]; ok {
		t.Fatal("claimed issue should be released after abandon")
	}
	entry, ok := o.state.Abandoned[attempt.IssueID]
	if !ok {
		t.Fatal("abandoned entry should be recorded")
	}
	if entry.FailureCount != 2 {
		t.Fatalf("abandoned failure_count = %d, want 2", entry.FailureCount)
	}
	if !strings.Contains(capturedBody, "commentCreate") {
		t.Fatalf("commentCreate mutation was not sent: %s", capturedBody)
	}
	if !strings.Contains(capturedBody, "fatal setup") {
		t.Fatalf("last error missing from comment body: %s", capturedBody)
	}
}

func TestOnWorkerDoneFailureAbandonsEvenWhenCommentFails(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errors":[{"message":"boom"}]}`))
	}))
	defer srv.Close()

	tr, err := tracker.NewLinearClient("token", srv.URL, "proj", []string{"Todo", "In Progress"})
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	o := New("", 0, "alpha", nil)
	o.tracker = tr

	attempt := &RunAttempt{
		IssueID:      "issue-1",
		Identifier:   "J-27",
		Attempt:      2,
		FailureCount: 1,
		IssueState:   "In Progress",
	}

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.Claimed[attempt.IssueID] = struct{}{}
	o.state.mu.Unlock()

	cfg := config.New(map[string]any{
		"agent": map[string]any{
			"max_retry_attempts": 1,
		},
	})

	o.onWorkerDone(context.Background(), cfg, attempt.IssueID, attempt, errors.New("fatal setup"))

	o.state.mu.Lock()
	defer o.state.mu.Unlock()

	if _, ok := o.state.RetryQueue[attempt.IssueID]; ok {
		t.Fatal("retry entry should not exist after abandon")
	}
	if _, ok := o.state.Claimed[attempt.IssueID]; ok {
		t.Fatal("claimed issue should be released after abandon")
	}
	entry, ok := o.state.Abandoned[attempt.IssueID]
	if !ok {
		t.Fatal("abandoned entry should still be recorded when comment create fails")
	}
	if entry.Error != "fatal setup" {
		t.Fatalf("abandoned error = %q, want %q", entry.Error, "fatal setup")
	}
}

func TestOnWorkerDoneFailureDoesNotAbandonWhenMaxRetryAttemptsDisabled(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)
	attempt := &RunAttempt{
		IssueID:      "issue-1",
		Identifier:   "J-27",
		Attempt:      4,
		FailureCount: 7,
		IssueState:   "In Progress",
	}

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.Claimed[attempt.IssueID] = struct{}{}
	o.state.mu.Unlock()

	cfg := config.New(map[string]any{
		"agent": map[string]any{
			"max_retry_attempts": 0,
		},
	})

	o.onWorkerDone(context.Background(), cfg, attempt.IssueID, attempt, errors.New("fatal setup"))

	o.state.mu.Lock()
	defer o.state.mu.Unlock()

	if _, ok := o.state.Abandoned[attempt.IssueID]; ok {
		t.Fatal("abandoned entry should not be recorded when max_retry_attempts is disabled")
	}
	entry, ok := o.state.RetryQueue[attempt.IssueID]
	if !ok {
		t.Fatal("retry entry should be scheduled when max_retry_attempts is disabled")
	}
	if entry.timer != nil {
		entry.timer.Stop()
	}
	if entry.Attempt != 5 {
		t.Fatalf("retry attempt = %d, want 5", entry.Attempt)
	}
	if entry.FailureCount != 8 {
		t.Fatalf("retry failure_count = %d, want 8", entry.FailureCount)
	}
}

func TestCanDispatchReleasesAbandonedIssueAfterStateChange(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)
	o.state.mu.Lock()
	o.state.Abandoned["issue-1"] = &AbandonedEntry{
		Identifier: "J-27",
		State:      "In Progress",
	}
	o.state.mu.Unlock()

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"active_states": []any{"Todo", "In Progress", "Rework"},
		},
	})

	sameState := &types.Issue{ID: "issue-1", Identifier: "J-27", State: "In Progress"}
	if o.canDispatch(cfg, sameState) {
		t.Fatal("abandoned issue should stay blocked while state is unchanged")
	}

	nextState := &types.Issue{ID: "issue-1", Identifier: "J-27", State: "Rework"}
	if !o.canDispatch(cfg, nextState) {
		t.Fatal("state change should release abandoned issue")
	}

	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	if _, ok := o.state.Abandoned["issue-1"]; ok {
		t.Fatal("abandoned entry should be cleared after state change")
	}
}

func TestCanDispatchReleasesAbandonedIssueAfterUpdateAtSameState(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)
	abandonedAt := time.Now().Add(-time.Minute)

	o.state.mu.Lock()
	o.state.Abandoned["issue-1"] = &AbandonedEntry{
		Identifier:  "J-27",
		State:       "In Progress",
		AbandonedAt: abandonedAt,
	}
	o.state.mu.Unlock()

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"active_states": []any{"Todo", "In Progress"},
		},
	})

	staleUpdatedAt := abandonedAt.Add(-time.Second)
	stale := &types.Issue{ID: "issue-1", Identifier: "J-27", State: "In Progress", UpdatedAt: &staleUpdatedAt}
	if o.canDispatch(cfg, stale) {
		t.Fatal("abandoned issue should stay blocked when same-state issue was not updated after abandon")
	}

	freshUpdatedAt := abandonedAt.Add(time.Second)
	fresh := &types.Issue{ID: "issue-1", Identifier: "J-27", State: "In Progress", UpdatedAt: &freshUpdatedAt}
	if !o.canDispatch(cfg, fresh) {
		t.Fatal("same-state issue updated after abandon should be dispatchable")
	}
}

func TestCanDispatchKeepsAbandonedIssueBlockedForOwnAbandonCommentUpdate(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)
	abandonedAt := time.Now().Add(-time.Minute)
	commentUpdatedAt := abandonedAt.Add(2 * time.Second)

	o.state.mu.Lock()
	o.state.Abandoned["issue-1"] = &AbandonedEntry{
		Identifier:  "J-27",
		State:       "In Progress",
		AbandonedAt: abandonedAt,
	}
	o.state.mu.Unlock()

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"active_states": []any{"Todo", "In Progress"},
		},
	})

	issue := &types.Issue{
		ID:         "issue-1",
		Identifier: "J-27",
		State:      "In Progress",
		UpdatedAt:  &commentUpdatedAt,
		LastComment: &types.Comment{
			Body:      buildRetryAbandonComment("J-27", 3, 4, "fatal setup"),
			UpdatedAt: &commentUpdatedAt,
		},
	}
	if o.canDispatch(cfg, issue) {
		t.Fatal("own abandon comment should not release same-state abandoned issue")
	}
}

func TestCanDispatchReleasesAbandonedIssueAfterUpdatePastOwnAbandonComment(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)
	abandonedAt := time.Now().Add(-time.Minute)
	commentUpdatedAt := abandonedAt.Add(2 * time.Second)
	issueUpdatedAt := commentUpdatedAt.Add(time.Second)

	o.state.mu.Lock()
	o.state.Abandoned["issue-1"] = &AbandonedEntry{
		Identifier:  "J-27",
		State:       "In Progress",
		AbandonedAt: abandonedAt,
	}
	o.state.mu.Unlock()

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"active_states": []any{"Todo", "In Progress"},
		},
	})

	issue := &types.Issue{
		ID:         "issue-1",
		Identifier: "J-27",
		State:      "In Progress",
		UpdatedAt:  &issueUpdatedAt,
		LastComment: &types.Comment{
			Body:      buildRetryAbandonComment("J-27", 3, 4, "fatal setup"),
			UpdatedAt: &commentUpdatedAt,
		},
	}
	if !o.canDispatch(cfg, issue) {
		t.Fatal("same-state issue updated after abandon comment should be dispatchable")
	}
}

func TestCanDispatchKeepsAbandonedIssueBlockedWhenUpdatedAtMissing(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)
	abandonedAt := time.Now().Add(-time.Minute)

	o.state.mu.Lock()
	o.state.Abandoned["issue-1"] = &AbandonedEntry{
		Identifier:  "J-27",
		State:       "In Progress",
		AbandonedAt: abandonedAt,
	}
	o.state.mu.Unlock()

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"active_states": []any{"Todo", "In Progress"},
		},
	})

	issue := &types.Issue{ID: "issue-1", Identifier: "J-27", State: "In Progress"}
	if o.canDispatch(cfg, issue) {
		t.Fatal("abandoned issue should stay blocked when updatedAt is missing")
	}

	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	if _, ok := o.state.Abandoned["issue-1"]; !ok {
		t.Fatal("abandoned issue should remain recorded when updatedAt is missing")
	}
}

func TestReconcileUpdatesRunningIssueState(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"issues":{"nodes":[{"id":"issue-1","identifier":"J-27","state":{"name":"Rework"}}]}}}`))
	}))
	defer srv.Close()

	tr, err := tracker.NewLinearClient("token", srv.URL, "proj", []string{"Todo", "In Progress", "Rework"})
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	o := New("", 0, "alpha", nil)
	o.tracker = tr
	o.cfg = config.New(map[string]any{
		"tracker": map[string]any{
			"terminal_states": []any{"Done"},
		},
		"codex": map[string]any{
			"stall_timeout_ms": 60_000,
		},
	})

	attempt := &RunAttempt{
		IssueID:    "issue-1",
		Identifier: "J-27",
		IssueState: "Todo",
		StartedAt:  time.Now(),
	}

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.mu.Unlock()

	o.reconcile(context.Background())

	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	if got := o.state.Running[attempt.IssueID].IssueState; got != "Rework" {
		t.Fatalf("IssueState = %q, want %q", got, "Rework")
	}
}

func TestBuildRetryAbandonCommentIncludesCountsAndError(t *testing.T) {
	t.Parallel()

	body := buildRetryAbandonComment("J-27", 3, 4, "fatal setup")

	if !strings.Contains(body, "J-27") {
		t.Fatalf("comment body missing identifier: %s", body)
	}
	if !strings.Contains(body, retryAbandonCommentMarker) {
		t.Fatalf("comment body missing abandon marker: %s", body)
	}
	if !strings.Contains(body, "agent.max_retry_attempts=3") {
		t.Fatalf("comment body missing max attempts: %s", body)
	}
	if !strings.Contains(body, "연속 실패 횟수: 4회") {
		t.Fatalf("comment body missing failure count: %s", body)
	}
	if !strings.Contains(body, "fatal setup") {
		t.Fatalf("comment body missing last error: %s", body)
	}
}

func TestBuildRetryAbandonCommentTruncatesLongError(t *testing.T) {
	t.Parallel()

	errMsg := strings.Repeat("x", 2100)
	body := buildRetryAbandonComment("J-27", 3, 4, errMsg)

	if strings.Contains(body, strings.Repeat("x", 2001)) {
		t.Fatalf("comment body should truncate long error: len=%d", len(body))
	}
	if !strings.Contains(body, strings.Repeat("x", 2000)) {
		t.Fatalf("comment body should include truncated 2000-char error payload")
	}
}
