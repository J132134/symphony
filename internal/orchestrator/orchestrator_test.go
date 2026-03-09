package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"symphony/internal/agent"
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
	o.onWorkerDone(context.Background(), config.New(nil), attempt.IssueID, attempt, nil, nil)

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

func TestGracefulStopDrainWaitsForWorkerWaitGroup(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)
	o.mu.Lock()
	o.cfg = config.New(map[string]any{
		"daemon": map[string]any{
			"drain_timeout_ms": 25,
		},
	})
	o.mu.Unlock()

	attempt := &RunAttempt{IssueID: "issue-1", Identifier: "J-31"}
	handle := &workerHandle{
		cancel:  func() {},
		attempt: attempt,
	}

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.mu.Unlock()

	o.mu.Lock()
	o.workersByIssue[attempt.IssueID] = handle
	o.mu.Unlock()

	o.workers.Add(1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = o.gracefulStop(true)
	}()

	select {
	case <-done:
		t.Fatal("gracefulStop should wait until workers finish")
	case <-time.After(100 * time.Millisecond):
	}

	o.workers.Done()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for gracefulStop to finish")
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
		name     string
		entry    *RetryEntry
		delayMin time.Duration
		delayMax time.Duration
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

func TestRunningConcurrentCountExcludesManualHumanReview(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)
	cfg := config.New(map[string]any{
		"codex": map[string]any{
			"command": "codex app-server",
		},
	})

	o.state.mu.Lock()
	o.state.Running["issue-1"] = &RunAttempt{IssueID: "issue-1", IssueState: "Human Review"}
	o.state.Running["issue-2"] = &RunAttempt{IssueID: "issue-2", IssueState: "In Progress"}
	got := o.runningConcurrentCountLocked(cfg)
	o.state.mu.Unlock()

	if got != 1 {
		t.Fatalf("runningConcurrentCountLocked() = %d, want 1", got)
	}
}

func TestRunningConcurrentCountExcludesConfiguredPauseState(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)
	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"pause_states": []any{"Plan Review"},
		},
	})

	o.state.mu.Lock()
	o.state.Running["issue-1"] = &RunAttempt{IssueID: "issue-1", IssueState: "Plan Review"}
	o.state.Running["issue-2"] = &RunAttempt{IssueID: "issue-2", IssueState: "In Progress"}
	got := o.runningConcurrentCountLocked(cfg)
	o.state.mu.Unlock()

	if got != 1 {
		t.Fatalf("runningConcurrentCountLocked() = %d, want 1", got)
	}
}

func TestCanDispatchIgnoresManualHumanReviewForConcurrencyLimit(t *testing.T) {
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

func TestCanDispatchSkipsManualHumanReviewState(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)
	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"active_states": []any{"Todo", "In Progress", "Human Review"},
		},
		"codex": map[string]any{
			"command": "codex app-server",
		},
	})

	issue := &types.Issue{ID: "issue-1", Identifier: "J-40", State: "Human Review"}
	if o.canDispatch(cfg, issue) {
		t.Fatal("manual human review issue should not dispatch")
	}
}

func TestCanDispatchSkipsConfiguredPauseState(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)
	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"active_states": []any{"Todo", "Plan Review", "In Progress"},
			"pause_states":  []any{"Plan Review"},
		},
		"codex": map[string]any{
			"command": "codex app-server",
		},
	})

	issue := &types.Issue{ID: "issue-1", Identifier: "J-46", State: "Plan Review"}
	if o.canDispatch(cfg, issue) {
		t.Fatal("configured pause-state issue should not dispatch")
	}
}

func TestCanDispatchAllowsUrgentWhenConcurrencyLimitReached(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)

	o.state.mu.Lock()
	o.state.MaxConcurrentAgents = 1
	o.state.Running["issue-1"] = &RunAttempt{
		IssueID:    "issue-1",
		Identifier: "J-10",
		IssueState: "In Progress",
	}
	o.state.mu.Unlock()

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"active_states": []any{"In Progress"},
		},
	})

	issue := &types.Issue{ID: "issue-2", Identifier: "J-39", State: "In Progress", Priority: intPtr(urgentPriority)}
	if !o.canDispatch(cfg, issue) {
		t.Fatal("urgent issue should bypass the project concurrency limit")
	}
}

func TestCanDispatchBlocksNonUrgentWhileUrgentRunning(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)

	o.state.mu.Lock()
	o.state.Running["issue-urgent"] = &RunAttempt{
		IssueID:    "issue-urgent",
		Identifier: "J-39",
		IssueState: "In Progress",
		Urgent:     true,
	}
	o.state.mu.Unlock()

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"active_states": []any{"In Progress"},
		},
	})

	issue := &types.Issue{ID: "issue-2", Identifier: "J-12", State: "In Progress"}
	if o.canDispatch(cfg, issue) {
		t.Fatal("non-urgent issue should stay paused while an urgent issue is running")
	}
}

func TestCanDispatchBlocksTodoWhenBlockerIsNotTerminal(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)
	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"active_states":   []any{"Todo", "In Progress"},
			"terminal_states": []any{"Done"},
		},
	})

	issue := &types.Issue{
		ID:         "issue-33",
		Identifier: "J-33",
		State:      "Todo",
		BlockedBy: []types.BlockerRef{
			{ID: "issue-31", Identifier: "J-31", State: "In Progress"},
		},
	}

	if o.canDispatch(cfg, issue) {
		t.Fatal("todo issue with non-terminal blocker should not dispatch")
	}
}

func TestHasGlobalCapacityForStateIgnoresManualHumanReview(t *testing.T) {
	t.Parallel()

	limiter := NewSessionLimiter(1)
	if !limiter.TryAcquire() {
		t.Fatal("expected limiter warm-up acquire to succeed")
	}

	o := New("", 0, "alpha", limiter)
	cfg := config.New(map[string]any{
		"codex": map[string]any{
			"command": "codex app-server",
		},
	})

	if o.hasGlobalCapacityForState(cfg, "In Progress") {
		t.Fatal("non-review issue should respect the global limiter")
	}
	if !o.hasGlobalCapacityForState(cfg, "Human Review") {
		t.Fatal("human review issue should bypass the global limiter")
	}
}

func TestShouldAcquireGlobalSlotIncludesUrgentIssues(t *testing.T) {
	t.Parallel()

	urgent := &types.Issue{ID: "issue-1", State: "In Progress", Priority: intPtr(urgentPriority)}
	if !shouldAcquireGlobalSlot(nil, urgent) {
		t.Fatal("urgent issue should still count as a running global session")
	}

	normal := &types.Issue{ID: "issue-2", State: "In Progress"}
	if !shouldAcquireGlobalSlot(nil, normal) {
		t.Fatal("non-urgent in-progress issue should still acquire a global slot")
	}
}

func TestPreemptForUrgentMarksRunningIssuesAndCancels(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)

	var cancelled []string
	o.state.mu.Lock()
	o.state.Running["issue-1"] = &RunAttempt{
		IssueID:    "issue-1",
		Identifier: "J-10",
		IssueState: "In Progress",
	}
	o.state.Running["issue-2"] = &RunAttempt{
		IssueID:    "issue-2",
		Identifier: "J-11",
		IssueState: "In Progress",
		Urgent:     true,
	}
	o.state.mu.Unlock()

	o.mu.Lock()
	o.workersByIssue["issue-1"] = &workerHandle{
		cancel:  func() { cancelled = append(cancelled, "issue-1") },
		attempt: o.state.Running["issue-1"],
	}
	o.workersByIssue["issue-2"] = &workerHandle{
		cancel:  func() { cancelled = append(cancelled, "issue-2") },
		attempt: o.state.Running["issue-2"],
	}
	o.mu.Unlock()

	o.preemptForUrgent(nil, &types.Issue{ID: "issue-99", Identifier: "J-39", State: "In Progress", Priority: intPtr(urgentPriority)})

	o.state.mu.Lock()
	defer o.state.mu.Unlock()

	if !o.state.Running["issue-1"].Preempted {
		t.Fatal("non-urgent issue should be marked preempted")
	}
	if o.state.Running["issue-2"].Preempted {
		t.Fatal("urgent issue should not be preempted by another urgent dispatch")
	}
	if got := len(cancelled); got != 1 || cancelled[0] != "issue-1" {
		t.Fatalf("cancelled = %v, want only issue-1", cancelled)
	}
	if _, ok := o.state.Abandoned["issue-1"]; !ok {
		t.Fatal("preempted issue should be tracked as abandoned for resume")
	}
}

func TestPreemptForUrgentPreemptsAcrossGlobalLimiter(t *testing.T) {
	t.Parallel()

	limiter := NewSessionLimiter(2)
	o := New("", 0, "alpha", limiter)

	var cancelled []string
	if !limiter.TryAcquireIssue("issue-remote", false, func() { cancelled = append(cancelled, "issue-remote") }) {
		t.Fatal("remote issue acquire should succeed")
	}

	o.preemptForUrgent(nil, &types.Issue{ID: "issue-99", Identifier: "J-39", State: "In Progress", Priority: intPtr(urgentPriority)})

	if len(cancelled) != 1 || cancelled[0] != "issue-remote" {
		t.Fatalf("cancelled = %v, want only issue-remote", cancelled)
	}
}

func TestReconcileCancelsManualHumanReviewIssue(t *testing.T) {
	t.Parallel()

	server := newLinearIssueStateServer(t, []*types.Issue{
		{ID: "issue-1", Identifier: "J-40", State: "Human Review"},
	})
	defer server.Close()

	client, err := tracker.NewLinearClient("test-key", server.URL, "proj", []string{"Todo", "In Progress", "Human Review"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient: %v", err)
	}

	o := New("", 0, "alpha", nil)
	o.cfg = config.New(map[string]any{
		"tracker": map[string]any{
			"active_states":   []any{"Todo", "In Progress", "Human Review"},
			"terminal_states": []any{"Done"},
		},
		"codex": map[string]any{
			"command": "codex app-server",
		},
	})
	o.tracker = client

	cancelled := make(chan struct{}, 1)
	o.mu.Lock()
	o.workersByIssue["issue-1"] = &workerHandle{
		cancel: func() {
			select {
			case cancelled <- struct{}{}:
			default:
			}
		},
		attempt: &RunAttempt{IssueID: "issue-1", Identifier: "J-40"},
	}
	o.mu.Unlock()

	o.state.mu.Lock()
	o.state.Running["issue-1"] = &RunAttempt{IssueID: "issue-1", Identifier: "J-40", IssueState: "In Progress"}
	o.state.mu.Unlock()

	o.reconcile(context.Background())

	select {
	case <-cancelled:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("manual human review issue should be cancelled by reconcile")
	}
}

func TestOnRetryTimerReleasesClaimForManualHumanReview(t *testing.T) {
	t.Parallel()

	server := newLinearIssueStateServer(t, []*types.Issue{
		{ID: "issue-1", Identifier: "J-40", State: "Human Review"},
	})
	defer server.Close()

	client, err := tracker.NewLinearClient("test-key", server.URL, "proj", []string{"Todo", "In Progress", "Human Review"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient: %v", err)
	}

	o := New("", 0, "alpha", nil)
	o.tracker = client
	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"active_states": []any{"Todo", "In Progress", "Human Review"},
		},
		"codex": map[string]any{
			"command": "codex app-server",
		},
	})

	entry := &RetryEntry{
		IssueID:    "issue-1",
		Identifier: "J-40",
		Attempt:    2,
		DueAt:      time.Now(),
	}

	o.state.mu.Lock()
	o.state.RetryQueue[entry.IssueID] = entry
	o.state.Claimed[entry.IssueID] = struct{}{}
	o.state.mu.Unlock()

	o.onRetryTimer(context.Background(), cfg, entry.IssueID)

	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	if _, ok := o.state.Claimed[entry.IssueID]; ok {
		t.Fatal("manual human review retry should release claim")
	}
	if len(o.state.Running) != 0 {
		t.Fatalf("manual human review retry should not dispatch, got %d running", len(o.state.Running))
	}
}

func TestShouldAttachPRLinkForConfiguredPauseState(t *testing.T) {
	t.Parallel()

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"pause_states": []any{"Plan Review"},
		},
	})

	summary := workspaceSummary{PRURL: "https://example.com/pr/1"}
	if !shouldAttachPRLink(cfg, "Plan Review", summary, nil) {
		t.Fatal("configured pause state should attach PR link")
	}
	if shouldAttachPRLink(cfg, "In Progress", summary, nil) {
		t.Fatal("non-pause state should not attach PR link")
	}
}

func TestHandleAgentEventRateLimitPausesDispatchUntilReset(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)

	resetAt := time.Now().UTC().Add(2 * time.Minute)
	attempt := &RunAttempt{IssueID: "issue-1", Identifier: "J-20", IssueState: "In Progress"}
	o.handleAgentEvent(attempt.IssueID, attempt, agent.Event{
		Name:      "rate_limit",
		Timestamp: time.Now().UTC(),
		RateLimit: &agent.RateLimitEvent{ResetAt: &resetAt},
	})

	if until, reason, paused := o.admissionPauseState(time.Now().UTC()); !paused {
		t.Fatal("expected orchestrator to enter paused state")
	} else {
		if reason != "rate_limit_reset" {
			t.Fatalf("pause reason = %q, want rate_limit_reset", reason)
		}
		if until.Before(resetAt.Add(-time.Second)) {
			t.Fatalf("pause until = %v, want near %v", until, resetAt)
		}
	}

	// Pause is enforced at tick level (not inside canDispatch); verify via admissionPauseState directly.
	expired := time.Now().UTC().Add(-time.Second)
	o.state.mu.Lock()
	o.state.PausedUntil = &expired
	o.state.mu.Unlock()

	if _, _, paused := o.admissionPauseState(time.Now().UTC()); paused {
		t.Fatal("dispatch should resume once the pause expires")
	}
}

func TestHandleAgentEventRateLimitPausesGlobalLimiter(t *testing.T) {
	t.Parallel()

	limiter := NewSessionLimiter(2)
	o := New("", 0, "alpha", limiter)

	resetAt := time.Now().UTC().Add(45 * time.Second)
	o.handleAgentEvent("issue-1", &RunAttempt{Identifier: "J-20"}, agent.Event{
		Name:      "rate_limit",
		Timestamp: time.Now().UTC(),
		RateLimit: &agent.RateLimitEvent{ResetAt: &resetAt},
	})

	if _, ok := limiter.PausedUntil(); !ok {
		t.Fatal("expected shared limiter to be paused")
	}
	if limiter.TryAcquire() {
		t.Fatal("shared limiter should reject new sessions while paused")
	}
}

func TestHandleAgentEventRateLimitWithoutResetDoesNotPause(t *testing.T) {
	t.Parallel()

	// A rate_limit event with ResetAt == nil means no window is actually throttled
	// (e.g. an informational account/rateLimits/updated notification with low
	// used_percent). It must not trigger a dispatch pause or backoff.
	o := New("", 0, "alpha", nil)
	now := time.Now().UTC()

	o.handleAgentEvent("issue-1", &RunAttempt{Identifier: "J-20"}, agent.Event{
		Name:      "rate_limit",
		Timestamp: now,
		RateLimit: &agent.RateLimitEvent{},
	})

	_, _, paused := o.admissionPauseState(now)
	if paused {
		t.Fatal("expected no pause when ResetAt is nil (no window throttled)")
	}
}

func TestOnRetryTimerDuringRateLimitPauseReschedulesAtResume(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)
	entry := &RetryEntry{
		IssueID:    "issue-1",
		Identifier: "J-20",
		Attempt:    2,
		DueAt:      time.Now(),
	}
	pausedUntil := time.Now().UTC().Add(90 * time.Second)

	o.state.mu.Lock()
	o.state.PausedUntil = &pausedUntil
	o.state.PauseReason = "rate_limit_reset"
	o.state.RetryQueue[entry.IssueID] = entry
	o.state.Claimed[entry.IssueID] = struct{}{}
	o.state.mu.Unlock()

	o.onRetryTimer(context.Background(), config.New(nil), entry.IssueID)
	stopRetryTimer(t, o, entry.IssueID)

	o.state.mu.Lock()
	defer o.state.mu.Unlock()

	rescheduled, ok := o.state.RetryQueue[entry.IssueID]
	if !ok {
		t.Fatal("retry entry should be rescheduled while paused")
	}
	if rescheduled.Error != "rate limit pause" {
		t.Fatalf("retry error = %q, want rate limit pause", rescheduled.Error)
	}
	if rescheduled.DueAt.Before(pausedUntil.Add(-time.Second)) {
		t.Fatalf("retry due_at = %v, want near %v", rescheduled.DueAt, pausedUntil)
	}
	if len(o.state.Running) != 0 {
		t.Fatalf("no issue should be redispatched during pause, got %d running", len(o.state.Running))
	}
}

func TestBuildAgentConfigUsesDefaultCommand(t *testing.T) {
	t.Parallel()

	cfg := config.New(map[string]any{
		"codex": map[string]any{
			"command": "codex app-server --model gpt-5",
		},
	})

	gotCfg, err := buildAgentConfig(cfg, t.TempDir())
	if err != nil {
		t.Fatalf("buildAgentConfig() error = %v", err)
	}
	if got := gotCfg.Command; got != "codex app-server --model gpt-5" {
		t.Fatalf("buildAgentConfig().Command = %q", got)
	}
}

func TestBuildAgentConfigIncludesGitWritableDirs(t *testing.T) {
	t.Parallel()

	wsPath := initGitWorkspace(t)
	cfg := config.New(nil)

	gotCfg, err := buildAgentConfig(cfg, wsPath)
	if err != nil {
		t.Fatalf("buildAgentConfig() error = %v", err)
	}
	if len(gotCfg.AdditionalWritableDirs) != 1 {
		t.Fatalf("len(buildAgentConfig().AdditionalWritableDirs) = %d, want 1 (%v)", len(gotCfg.AdditionalWritableDirs), gotCfg.AdditionalWritableDirs)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(wsPath, ".git"))
	if err != nil {
		t.Fatalf("EvalSymlinks(.git): %v", err)
	}
	if got := gotCfg.AdditionalWritableDirs[0]; got != want {
		t.Fatalf("buildAgentConfig().AdditionalWritableDirs[0] = %q, want %q", got, want)
	}
}

func TestWatchWorkflowReloadsOnFileChange(t *testing.T) {
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

func TestWatchWorkflowRejectsInvalidReloadAndKeepsPreviousConfig(t *testing.T) {
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
	writeInvalidWorkflowFile(t, workflowPath)
	time.Sleep(100 * time.Millisecond)

	o.state.mu.Lock()
	if o.state.PollIntervalMs != 1000 {
		t.Fatalf("PollIntervalMs = %d, want 1000", o.state.PollIntervalMs)
	}
	if o.state.PollIntervalIdleMs != 2000 {
		t.Fatalf("PollIntervalIdleMs = %d, want 2000", o.state.PollIntervalIdleMs)
	}
	if o.state.MaxConcurrentAgents != 4 {
		t.Fatalf("MaxConcurrentAgents = %d, want 4", o.state.MaxConcurrentAgents)
	}
	o.state.mu.Unlock()

	o.mu.Lock()
	cfg := o.cfg
	o.mu.Unlock()
	if cfg == nil {
		t.Fatal("expected previous config to be retained")
	}
	if got := cfg.PollIntervalMs(); got != 1000 {
		t.Fatalf("cfg.PollIntervalMs() = %d, want 1000", got)
	}
}

func TestOnWorkerDoneSuccessPostsCommentAndTransitionsState(t *testing.T) {
	t.Parallel()

	recorder, server := newLinearRecorderServer(t)
	defer server.Close()

	client, err := tracker.NewLinearClient("test-key", server.URL, "proj", []string{"In Progress"}, "")
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
		IssueID:    "issue-1",
		Identifier: "J-29",
		Attempt:    1,
		StartedAt:  time.Now().Add(-(3*time.Minute + 4*time.Second)),
		FinishedAt: time.Now(),
		Session: LiveSession{
			TurnCount:    2,
			InputTokens:  28000,
			OutputTokens: 14100,
			TotalTokens:  42100,
		},
		Summary: &workspaceSummary{
			Branch:            "j-29-test",
			IssueBranch:       "j-29-test",
			ModifiedFiles:     []string{"foo.txt"},
			LastCommitHash:    "1234567890abcdef",
			LastCommitSubject: "feat: add foo",
			PRURL:             "https://github.com/example/nonexistent-symphony-test/pull/123",
			PRBranch:          "j-29-test",
		},
	}
	attempt.SetStatus(StatusSucceeded)

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.Claimed[attempt.IssueID] = struct{}{}
	o.state.mu.Unlock()

	o.onWorkerDone(context.Background(), cfg, attempt.IssueID, attempt, nil, nil)

	if recorder.commentCount() != 1 {
		t.Fatalf("comment count = %d, want 1", recorder.commentCount())
	}
	body := recorder.lastComment()
	for _, want := range []string{
		"✅ **Symphony agent completed** (attempt 1, turn 2/3)",
		"**Tokens:** 42,100 (in: 28,000 / out: 14,100)",
		"**Last commit:** `feat: add foo (1234567)`",
		"**PR:** https://github.com/example/nonexistent-symphony-test/pull/123",
		"**Branch:** `j-29-test`",
		"**Changes:**",
		"- Modified: foo.txt",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment body missing %q:\n%s", want, body)
		}
	}
	if got := recorder.lastState(); got != "Human Review" {
		t.Fatalf("state transition = %q, want Human Review", got)
	}
	if recorder.linkCount() != 1 {
		t.Fatalf("link count = %d, want 1", recorder.linkCount())
	}
	if got := recorder.lastLink(); got != "https://github.com/example/nonexistent-symphony-test/pull/123" {
		t.Fatalf("link url = %q, want PR url", got)
	}

	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	if o.state.CompletedCount != 1 {
		t.Fatalf("completed count = %d, want 1", o.state.CompletedCount)
	}
	if _, ok := o.state.RetryQueue[attempt.IssueID]; ok {
		t.Fatal("terminal success should not schedule a continuation retry")
	}
	if _, ok := o.state.Claimed[attempt.IssueID]; ok {
		t.Fatal("terminal success should release the claimed issue")
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
		IssueState: "In Progress",
	}
	attempt.SetStatus(StatusSucceeded)

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.Claimed[attempt.IssueID] = struct{}{}
	o.state.mu.Unlock()

	o.onWorkerDone(context.Background(), cfg, attempt.IssueID, attempt, nil, nil)

	o.state.mu.Lock()
	o.state.mu.Unlock()

	issue := &types.Issue{ID: attempt.IssueID, Identifier: attempt.Identifier, State: "In Progress"}
	if !o.canDispatch(cfg, issue) {
		t.Fatal("successful active issue should be eligible for natural redispatch on the next poll")
	}
}

func TestOnWorkerDoneContinuationPendingSchedulesRetryWithoutSuccessFeedback(t *testing.T) {
	t.Parallel()

	recorder, server := newLinearRecorderServer(t)
	defer server.Close()

	client, err := tracker.NewLinearClient("test-key", server.URL, "proj", []string{"In Progress"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient: %v", err)
	}

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"active_states":    []any{"In Progress"},
			"on_success_state": "Human Review",
		},
	})

	o := New("", 0, "alpha", nil)
	o.tracker = client
	attempt := &RunAttempt{
		IssueID:    "issue-1",
		Identifier: "J-54",
		Attempt:    1,
		StartedAt:  time.Now().Add(-30 * time.Second),
		IssueState: "In Progress",
	}
	attempt.SetNeedsContinuation(true)
	attempt.SetStatus(StatusSucceeded)

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.Claimed[attempt.IssueID] = struct{}{}
	o.state.mu.Unlock()

	before := time.Now()
	o.onWorkerDone(context.Background(), cfg, attempt.IssueID, attempt, nil, nil)

	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	entry, ok := o.state.RetryQueue[attempt.IssueID]
	if !ok {
		t.Fatal("expected continuation retry to be scheduled after success")
	}
	if entry.Kind != RetryKindContinuation {
		t.Fatalf("retry kind = %q, want continuation", entry.Kind)
	}
	if entry.Attempt != 1 {
		t.Fatalf("retry attempt = %d, want 1", entry.Attempt)
	}
	delay := entry.DueAt.Sub(before)
	if delay < 800*time.Millisecond || delay > 1200*time.Millisecond {
		t.Fatalf("continuation retry delay = %v, want about 1s", delay)
	}
	if o.state.CompletedCount != 0 {
		t.Fatalf("completed count = %d, want 0 while continuation is pending", o.state.CompletedCount)
	}
	if entry.timer != nil {
		entry.timer.Stop()
	}
	if recorder.commentCount() != 0 {
		t.Fatalf("comment count = %d, want 0 while continuation is pending", recorder.commentCount())
	}
	if got := recorder.lastState(); got != "" {
		t.Fatalf("unexpected state transition while continuation is pending: %q", got)
	}
}

func TestContinuationRetryReleasesClaimWhenIssueInactive(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Return issue in "Human Review" state — not an active state
		_, _ = w.Write([]byte(`{"data":{"issue":{"id":"issue-1","identifier":"J-54","title":"done","description":"","priority":0,"state":{"name":"Human Review"},"branchName":"","url":"","comments":{"nodes":[]},"labels":{"nodes":[]},"relations":{"nodes":[]},"createdAt":"2026-03-07T00:00:00Z","updatedAt":"2026-03-07T00:00:00Z"}}}`))
	}))
	defer server.Close()

	client, err := tracker.NewLinearClient("test-key", server.URL, "proj", []string{"In Progress"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient: %v", err)
	}

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"active_states": []any{"In Progress"},
		},
	})

	o := New("", 0, "alpha", nil)
	o.tracker = client

	entry := &RetryEntry{
		IssueID:    "issue-1",
		Identifier: "J-54",
		Kind:       RetryKindContinuation,
		Attempt:    1,
		DueAt:      time.Now(),
	}
	o.state.mu.Lock()
	o.state.RetryQueue[entry.IssueID] = entry
	o.state.Claimed[entry.IssueID] = struct{}{}
	o.state.mu.Unlock()

	o.onRetryTimer(context.Background(), cfg, entry.IssueID)

	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	if _, ok := o.state.RetryQueue[entry.IssueID]; ok {
		t.Fatal("retry entry should be removed when issue is inactive")
	}
	if _, ok := o.state.Claimed[entry.IssueID]; ok {
		t.Fatal("claim should be released when issue is inactive")
	}
	if len(o.state.Running) != 0 {
		t.Fatalf("no issue should be dispatched for inactive state, got %d running", len(o.state.Running))
	}
}

func TestOnWorkerDoneFinalFailurePostsCommentWithoutRetry(t *testing.T) {
	t.Parallel()

	recorder, server := newLinearRecorderServer(t)
	defer server.Close()

	client, err := tracker.NewLinearClient("test-key", server.URL, "proj", []string{"In Progress"}, "")
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
		Session: LiveSession{
			TurnCount: 1,
		},
	}
	attempt.SetStatus(StatusStalled)

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.Claimed[attempt.IssueID] = struct{}{}
	o.state.mu.Unlock()

	runErr := fmt.Errorf("turn failed: stall_timeout")
	o.onWorkerDone(context.Background(), cfg, attempt.IssueID, attempt, nil, runErr)

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
	if recorder.linkCount() != 0 {
		t.Fatalf("link count = %d, want 0", recorder.linkCount())
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

func TestResolveConfirmedPRPrefersIssueBranch(t *testing.T) {
	t.Parallel()

	summary := workspaceSummary{
		Branch:      "main",
		IssueBranch: "j-36-test",
		RemoteURL:   "https://github.com/example/nonexistent-symphony-test.git",
	}

	url, branch := resolveConfirmedPR(summary, func(owner, repo, branch string) string {
		if owner != "example" || repo != "nonexistent-symphony-test" || branch != "j-36-test" {
			t.Fatalf("unexpected lookup args: %s %s %s", owner, repo, branch)
		}
		return "https://github.com/example/nonexistent-symphony-test/pull/123"
	})

	if url != "https://github.com/example/nonexistent-symphony-test/pull/123" {
		t.Fatalf("resolveConfirmedPR() url = %q", url)
	}
	if branch != "j-36-test" {
		t.Fatalf("resolveConfirmedPR() branch = %q", branch)
	}
}

func TestBuildSuccessCommentOmitsUntrustedFields(t *testing.T) {
	t.Parallel()

	cfg := config.New(map[string]any{
		"agent": map[string]any{
			"max_turns": 20,
		},
	})
	finishedAt := time.Date(2026, 3, 9, 6, 0, 0, 0, time.UTC)
	attempt := &RunAttempt{
		Attempt:    1,
		StartedAt:  finishedAt.Add(-(9*time.Minute + 44*time.Second)),
		FinishedAt: finishedAt,
		Session: LiveSession{
			TurnCount: 20,
		},
	}

	body, err := buildSuccessComment(cfg, attempt, workspaceSummary{
		Branch:            "main",
		IssueBranch:       "j132134/j-54-status",
		LastCommitHash:    "147e34f012345678",
		LastCommitSubject: "fix: skip rate limit pause when no window is throttled",
	})
	if err != nil {
		t.Fatalf("buildSuccessComment() error = %v", err)
	}

	for _, unwanted := range []string{
		"**Tokens:**",
		"**PR:**",
		"**Branch:**",
		"**Changes:**",
		"`main`",
	} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("comment body unexpectedly contained %q:\n%s", unwanted, body)
		}
	}
	if !strings.Contains(body, "**Last commit:** `fix: skip rate limit pause when no window is throttled (147e34f)`") {
		t.Fatalf("comment body missing last commit:\n%s", body)
	}
}

func TestChangedFilesDoesNotFallbackToHeadWhenWorktreeIsClean(t *testing.T) {
	t.Parallel()

	wsPath := initGitWorkspace(t)

	files, err := changedFiles(wsPath)
	if err != nil {
		t.Fatalf("changedFiles() error = %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("changedFiles() = %v, want no files for a clean worktree", files)
	}
}

func TestOnWorkerDoneIntermediateFailureRetriesQuietly(t *testing.T) {
	t.Parallel()

	recorder, server := newLinearRecorderServer(t)
	defer server.Close()

	client, err := tracker.NewLinearClient("test-key", server.URL, "proj", []string{"In Progress"}, "")
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
	}
	attempt.SetStatus(StatusFailed)

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.Claimed[attempt.IssueID] = struct{}{}
	o.state.mu.Unlock()

	o.onWorkerDone(context.Background(), cfg, attempt.IssueID, attempt, nil, fmt.Errorf("boom"))
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

func TestOnWorkerDonePreemptedSchedulesRetryWithoutFeedback(t *testing.T) {
	t.Parallel()

	recorder, server := newLinearRecorderServer(t)
	defer server.Close()

	client, err := tracker.NewLinearClient("test-key", server.URL, "proj", []string{"In Progress"}, "")
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
		Identifier: "J-39",
		Attempt:    1,
		StartedAt:  time.Now().Add(-30 * time.Second),
		Preempted:  true,
	}
	attempt.SetStatus(StatusCanceled)

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.Claimed[attempt.IssueID] = struct{}{}
	o.state.mu.Unlock()

	o.onWorkerDone(context.Background(), cfg, attempt.IssueID, attempt, nil, context.Canceled)
	stopRetryTimer(t, o, attempt.IssueID)

	if recorder.commentCount() != 0 {
		t.Fatalf("comment count = %d, want 0", recorder.commentCount())
	}
	if recorder.lastState() != "" {
		t.Fatalf("unexpected state transition = %q", recorder.lastState())
	}
	if recorder.linkCount() != 0 {
		t.Fatalf("link count = %d, want 0", recorder.linkCount())
	}

	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	entry, ok := o.state.RetryQueue[attempt.IssueID]
	if !ok {
		t.Fatal("preempted issue should be re-queued")
	}
	if entry.Kind != RetryKindCapacity {
		t.Fatalf("retry kind = %q, want %q", entry.Kind, RetryKindCapacity)
	}
	if entry.FailureCount != 0 {
		t.Fatalf("retry failure_count = %d, want 0", entry.FailureCount)
	}
	if entry.DeferCount != 1 {
		t.Fatalf("retry defer_count = %d, want 1", entry.DeferCount)
	}
	if o.state.CompletedCount != 0 {
		t.Fatalf("completed count = %d, want 0", o.state.CompletedCount)
	}
}

func TestOnWorkerDoneCancelledByStallSchedulesRetry(t *testing.T) {
	t.Parallel()

	o := New("", 0, "alpha", nil)
	cfg := config.New(map[string]any{
		"agent": map[string]any{
			"max_attempts": 3,
		},
	})
	attempt := &RunAttempt{
		IssueID:    "issue-1",
		Identifier: "J-31",
		Attempt:    1,
	}
	attempt.SetCancelReason(CancelReasonStall)

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.Claimed[attempt.IssueID] = struct{}{}
	o.state.mu.Unlock()

	o.onWorkerDone(context.Background(), cfg, attempt.IssueID, attempt, nil, workerCancelledError(CancelReasonStall, "cancelled"))
	stopRetryTimer(t, o, attempt.IssueID)

	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	if _, ok := o.state.RetryQueue[attempt.IssueID]; !ok {
		t.Fatal("stall cancellation should schedule a retry")
	}
}

func TestOnRetryTimerDefersNonUrgentUntilUrgentFinishes(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"issue":{"id":"issue-1","identifier":"J-12","title":"normal","description":"","priority":0,"state":{"name":"In Progress"},"branchName":"","url":"","comments":{"nodes":[]},"labels":{"nodes":[]},"relations":{"nodes":[]},"createdAt":"2026-03-07T00:00:00Z","updatedAt":"2026-03-07T00:00:00Z"}}}`))
	}))
	defer server.Close()

	client, err := tracker.NewLinearClient("test-key", server.URL, "proj", []string{"In Progress"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient: %v", err)
	}

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"active_states": []any{"In Progress"},
		},
	})

	o := New("", 0, "alpha", nil)
	o.tracker = client

	o.state.mu.Lock()
	o.state.Running["issue-urgent"] = &RunAttempt{
		IssueID:    "issue-urgent",
		Identifier: "J-39",
		IssueState: "In Progress",
		Urgent:     true,
	}
	o.state.RetryQueue["issue-1"] = &RetryEntry{
		IssueID:    "issue-1",
		Identifier: "J-12",
		Attempt:    1,
		DueAt:      time.Now(),
	}
	o.state.Claimed["issue-1"] = struct{}{}
	o.state.mu.Unlock()

	o.onRetryTimer(context.Background(), cfg, "issue-1")
	stopRetryTimer(t, o, "issue-1")

	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	entry, ok := o.state.RetryQueue["issue-1"]
	if !ok {
		t.Fatal("non-urgent issue should remain queued while urgent work is running")
	}
	if entry.Kind != RetryKindCapacity {
		t.Fatalf("retry kind = %q, want %q", entry.Kind, RetryKindCapacity)
	}
	if entry.Error != "urgent in progress" {
		t.Fatalf("retry error = %q, want urgent in progress", entry.Error)
	}
	if entry.DeferCount != 1 {
		t.Fatalf("retry defer_count = %d, want 1", entry.DeferCount)
	}
	if len(o.state.Running) != 1 {
		t.Fatalf("running count = %d, want only the urgent issue", len(o.state.Running))
	}
}

func TestOnRetryTimerDefersNonUrgentWhileGlobalUrgentRuns(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"issue":{"id":"issue-1","identifier":"J-12","title":"normal","description":"","priority":0,"state":{"name":"In Progress"},"branchName":"","url":"","comments":{"nodes":[]},"labels":{"nodes":[]},"relations":{"nodes":[]},"createdAt":"2026-03-07T00:00:00Z","updatedAt":"2026-03-07T00:00:00Z"}}}`))
	}))
	defer server.Close()

	client, err := tracker.NewLinearClient("test-key", server.URL, "proj", []string{"In Progress"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient: %v", err)
	}

	cfg := config.New(map[string]any{
		"tracker": map[string]any{
			"active_states": []any{"In Progress"},
		},
	})

	limiter := NewSessionLimiter(2)
	if !limiter.ForceAcquireIssue("issue-urgent", true, nil) {
		t.Fatal("urgent acquire should succeed")
	}

	o := New("", 0, "alpha", limiter)
	o.tracker = client

	o.state.mu.Lock()
	o.state.RetryQueue["issue-1"] = &RetryEntry{
		IssueID:    "issue-1",
		Identifier: "J-12",
		Attempt:    1,
		DueAt:      time.Now(),
	}
	o.state.Claimed["issue-1"] = struct{}{}
	o.state.mu.Unlock()

	o.onRetryTimer(context.Background(), cfg, "issue-1")
	stopRetryTimer(t, o, "issue-1")

	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	entry, ok := o.state.RetryQueue["issue-1"]
	if !ok {
		t.Fatal("non-urgent issue should remain queued while a global urgent issue is running")
	}
	if entry.Kind != RetryKindCapacity {
		t.Fatalf("retry kind = %q, want %q", entry.Kind, RetryKindCapacity)
	}
	if entry.Error != "urgent in progress" {
		t.Fatalf("retry error = %q, want urgent in progress", entry.Error)
	}
	if entry.DeferCount != 1 {
		t.Fatalf("retry defer_count = %d, want 1", entry.DeferCount)
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

	server := newLinearIssueStateServer(t, []*types.Issue{
		{ID: "issue-1", Identifier: "J-21", State: "In Progress"},
	})
	defer server.Close()

	client, err := tracker.NewLinearClient("test-key", server.URL, "proj", []string{"In Progress"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient: %v", err)
	}

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

func TestIsRetryAbandonComment(t *testing.T) {
	t.Parallel()

	if !isRetryAbandonComment("<!-- symphony:retry-abandoned -->\npaused") {
		t.Fatal("expected retry abandon marker to be detected")
	}
	if isRetryAbandonComment("plain comment") {
		t.Fatal("plain comment should not be treated as retry abandon marker")
	}
}

type linearRecorder struct {
	mu            sync.Mutex
	commentOps    []string
	creates       []string
	updates       []string
	issueComments []*types.Comment
	nextCommentID int
	links         []string
	stateNames    []string
}

func (r *linearRecorder) addComment(body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commentOps = append(r.commentOps, body)
	r.creates = append(r.creates, body)
	r.nextCommentID++
	now := time.Now().UTC()
	r.issueComments = append(r.issueComments, &types.Comment{
		ID:        fmt.Sprintf("comment-%d", r.nextCommentID),
		Body:      body,
		CreatedAt: &now,
		UpdatedAt: &now,
	})
}

func (r *linearRecorder) updateComment(commentID, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commentOps = append(r.commentOps, body)
	r.updates = append(r.updates, body)
	now := time.Now().UTC()
	for _, comment := range r.issueComments {
		if comment != nil && comment.ID == commentID {
			comment.Body = body
			comment.UpdatedAt = &now
			return
		}
	}
	r.issueComments = append(r.issueComments, &types.Comment{
		ID:        commentID,
		Body:      body,
		CreatedAt: &now,
		UpdatedAt: &now,
	})
}

func (r *linearRecorder) seedIssueComments(comments ...*types.Comment) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.issueComments = nil
	r.nextCommentID = 0
	for _, comment := range comments {
		if comment == nil {
			continue
		}
		clone := *comment
		if clone.ID == "" {
			r.nextCommentID++
			clone.ID = fmt.Sprintf("seed-comment-%d", r.nextCommentID)
		}
		if clone.CreatedAt == nil {
			now := time.Now().UTC()
			clone.CreatedAt = &now
		}
		if clone.UpdatedAt == nil {
			clone.UpdatedAt = clone.CreatedAt
		}
		r.issueComments = append(r.issueComments, &clone)
	}
	if r.nextCommentID < len(r.issueComments) {
		r.nextCommentID = len(r.issueComments)
	}
}

func (r *linearRecorder) addState(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stateNames = append(r.stateNames, name)
}

func (r *linearRecorder) addLink(url string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.links = append(r.links, url)
}

func (r *linearRecorder) commentCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.commentOps)
}

func (r *linearRecorder) lastComment() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.commentOps) == 0 {
		return ""
	}
	return r.commentOps[len(r.commentOps)-1]
}

func (r *linearRecorder) createCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.creates)
}

func (r *linearRecorder) updateCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.updates)
}

func (r *linearRecorder) lastState() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.stateNames) == 0 {
		return ""
	}
	return r.stateNames[len(r.stateNames)-1]
}

func (r *linearRecorder) linkCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.links)
}

func (r *linearRecorder) lastLink() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.links) == 0 {
		return ""
	}
	return r.links[len(r.links)-1]
}

func (r *linearRecorder) issueCommentCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.issueComments)
}

func (r *linearRecorder) issueCommentBody(commentID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, comment := range r.issueComments {
		if comment != nil && comment.ID == commentID {
			return comment.Body
		}
	}
	return ""
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
		case strings.Contains(req.Query, "commentUpdate"):
			input, _ := req.Variables["input"].(map[string]any)
			recorder.updateComment(asString(req.Variables["id"]), asString(input["body"]))
			_, _ = w.Write([]byte(`{"data":{"commentUpdate":{"success":true}}}`))
		case strings.Contains(req.Query, "attachmentCreate"):
			input, _ := req.Variables["input"].(map[string]any)
			recorder.addLink(asString(input["url"]))
			_, _ = w.Write([]byte(`{"data":{"attachmentCreate":{"success":true}}}`))
		case strings.Contains(req.Query, "branchName url"):
			recorder.mu.Lock()
			nodes := make([]map[string]any, 0, len(recorder.issueComments))
			for _, comment := range recorder.issueComments {
				if comment == nil {
					continue
				}
				nodes = append(nodes, map[string]any{
					"id":        comment.ID,
					"body":      comment.Body,
					"createdAt": comment.CreatedAt.UTC().Format(time.RFC3339),
					"updatedAt": comment.UpdatedAt.UTC().Format(time.RFC3339),
				})
			}
			recorder.mu.Unlock()
			payload, err := json.Marshal(map[string]any{
				"id":          "issue-1",
				"identifier":  "J-29",
				"title":       "feedback",
				"description": "",
				"priority":    nil,
				"state":       map[string]any{"name": "In Progress"},
				"branchName":  nil,
				"url":         nil,
				"comments":    map[string]any{"nodes": nodes},
				"labels":      map[string]any{"nodes": []any{}},
				"relations":   map[string]any{"nodes": []any{}},
				"createdAt":   time.Now().UTC().Format(time.RFC3339),
				"updatedAt":   time.Now().UTC().Format(time.RFC3339),
			})
			if err != nil {
				t.Fatalf("marshal issue payload: %v", err)
			}
			_, _ = fmt.Fprintf(w, `{"data":{"issue":%s}}`, payload)
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

func TestMaybePostSuccessFeedbackUpdatesExistingCommentWithoutCreatingDuplicate(t *testing.T) {
	t.Parallel()

	recorder, server := newLinearRecorderServer(t)
	defer server.Close()

	existingAt := time.Now().Add(-2 * time.Minute).UTC()
	recorder.seedIssueComments(&types.Comment{
		ID:        "comment-feedback",
		Body:      feedbackCommentMarker + "\n\n✅ **Symphony agent completed** (attempt 1, turn 1/3)",
		CreatedAt: &existingAt,
		UpdatedAt: &existingAt,
	})

	client, err := tracker.NewLinearClient("test-key", server.URL, "proj", []string{"In Progress"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient: %v", err)
	}

	cfg := config.New(map[string]any{
		"agent": map[string]any{
			"max_turns": 3,
		},
	})

	o := New("", 0, "alpha", nil)
	attempt := &RunAttempt{
		IssueID:    "issue-1",
		Identifier: "J-49",
		Attempt:    1,
		StartedAt:  time.Now().Add(-2 * time.Minute),
		Summary: &workspaceSummary{
			ModifiedFiles: []string{"foo.txt"},
			Branch:        "j-49-comment-feedback-upsert",
		},
	}
	attempt.SetTurnCount(1)

	o.maybePostSuccessFeedback(context.Background(), cfg, client, attempt.IssueID, attempt)
	o.maybePostSuccessFeedback(context.Background(), cfg, client, attempt.IssueID, attempt)

	if recorder.createCount() != 0 {
		t.Fatalf("create count = %d, want 0", recorder.createCount())
	}
	if recorder.updateCount() != 2 {
		t.Fatalf("update count = %d, want 2", recorder.updateCount())
	}
	if recorder.issueCommentCount() != 1 {
		t.Fatalf("issue comment count = %d, want 1", recorder.issueCommentCount())
	}
	body := recorder.issueCommentBody("comment-feedback")
	if !strings.Contains(body, feedbackCommentMarker) {
		t.Fatalf("updated comment missing feedback marker:\n%s", body)
	}
	if !strings.Contains(body, "✅ **Symphony agent completed**") {
		t.Fatalf("updated comment missing success summary:\n%s", body)
	}
}

func TestOnWorkerDoneSuccessUpdatesLegacyFeedbackCommentEvenWhenHumanCommentIsLatest(t *testing.T) {
	t.Parallel()

	wsPath := initGitWorkspace(t)
	recorder, server := newLinearRecorderServer(t)
	defer server.Close()

	feedbackAt := time.Now().Add(-4 * time.Minute).UTC()
	humanAt := time.Now().Add(-1 * time.Minute).UTC()
	recorder.seedIssueComments(
		&types.Comment{
			ID:        "comment-feedback",
			Body:      "✅ **Symphony agent completed** (attempt 1, turn 1/3)\n\nold body",
			CreatedAt: &feedbackAt,
			UpdatedAt: &feedbackAt,
		},
		&types.Comment{
			ID:        "comment-human",
			Body:      "사람이 남긴 최신 코멘트",
			CreatedAt: &humanAt,
			UpdatedAt: &humanAt,
		},
	)

	client, err := tracker.NewLinearClient("test-key", server.URL, "proj", []string{"In Progress"}, "")
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
		Identifier:    "J-49",
		Attempt:       1,
		WorkspacePath: wsPath,
		StartedAt:     time.Now().Add(-(3*time.Minute + 4*time.Second)),
		Session: LiveSession{
			TurnCount:    2,
			InputTokens:  28000,
			OutputTokens: 14100,
			TotalTokens:  42100,
		},
	}
	attempt.SetStatus(StatusSucceeded)

	o.state.mu.Lock()
	o.state.Running[attempt.IssueID] = attempt
	o.state.Claimed[attempt.IssueID] = struct{}{}
	o.state.mu.Unlock()

	o.onWorkerDone(context.Background(), cfg, attempt.IssueID, attempt, nil, nil)

	if recorder.createCount() != 0 {
		t.Fatalf("create count = %d, want 0", recorder.createCount())
	}
	if recorder.updateCount() != 1 {
		t.Fatalf("update count = %d, want 1", recorder.updateCount())
	}
	if recorder.issueCommentCount() != 2 {
		t.Fatalf("issue comment count = %d, want 2", recorder.issueCommentCount())
	}
	body := recorder.issueCommentBody("comment-feedback")
	if !strings.Contains(body, feedbackCommentMarker) {
		t.Fatalf("updated legacy comment missing feedback marker:\n%s", body)
	}
	if !strings.Contains(body, "✅ **Symphony agent completed** (attempt 1, turn 2/3)") {
		t.Fatalf("updated comment missing latest success body:\n%s", body)
	}
	if got := recorder.issueCommentBody("comment-human"); got != "사람이 남긴 최신 코멘트" {
		t.Fatalf("human comment was modified: %q", got)
	}
}

func newLinearIssueStateServer(t *testing.T, issues []*types.Issue) *httptest.Server {
	t.Helper()

	byID := make(map[string]*types.Issue, len(issues))
	for _, iss := range issues {
		byID[iss.ID] = iss
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		case strings.Contains(req.Query, "issues(filter: { id: { in: $ids } })"):
			_, _ = fmt.Fprintf(w, `{"data":{"issues":{"nodes":%s}}}`, encodeIssueStateNodes(t, issues))
		case strings.Contains(req.Query, "state: { name: { in: $states } }"):
			_, _ = fmt.Fprintf(w, `{"data":{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":%s}}}`, encodeCandidateIssueNodes(t, issues))
		case strings.Contains(req.Query, "branchName url"):
			id := asString(req.Variables["id"])
			iss, ok := byID[id]
			if !ok {
				_, _ = w.Write([]byte(`{"data":{"issue":null}}`))
				return
			}
			nodes := encodeCandidateIssueNodes(t, []*types.Issue{iss})
			// Strip array brackets to get a single object.
			nodes = strings.TrimSpace(nodes)
			nodes = strings.TrimPrefix(nodes, "[")
			nodes = strings.TrimSuffix(nodes, "]")
			_, _ = fmt.Fprintf(w, `{"data":{"issue":%s}}`, nodes)
		default:
			t.Fatalf("unexpected query: %s", req.Query)
		}
	}))
}

func newLinearFailingClient(t *testing.T, status int, body string) (*tracker.LinearClient, *httptest.Server) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, body, status)
	}))

	client, err := tracker.NewLinearClient("test-key", server.URL, "proj", []string{"In Progress"}, "")
	if err != nil {
		server.Close()
		t.Fatalf("NewLinearClient: %v", err)
	}
	return client, server
}

func encodeIssueStateNodes(t *testing.T, issues []*types.Issue) string {
	t.Helper()

	nodes := make([]map[string]any, 0, len(issues))
	for _, issue := range issues {
		nodes = append(nodes, map[string]any{
			"id":         issue.ID,
			"identifier": issue.Identifier,
			"state": map[string]any{
				"name": issue.State,
			},
		})
	}
	raw, err := json.Marshal(nodes)
	if err != nil {
		t.Fatalf("marshal issue state nodes: %v", err)
	}
	return string(raw)
}

func encodeCandidateIssueNodes(t *testing.T, issues []*types.Issue) string {
	t.Helper()

	nodes := make([]map[string]any, 0, len(issues))
	for _, issue := range issues {
		nodes = append(nodes, map[string]any{
			"id":          issue.ID,
			"identifier":  issue.Identifier,
			"title":       "",
			"description": "",
			"priority":    nil,
			"state": map[string]any{
				"name": issue.State,
			},
			"branchName": nil,
			"url":        nil,
			"comments": map[string]any{
				"nodes": []any{},
			},
			"labels": map[string]any{
				"nodes": []any{},
			},
			"relations": map[string]any{
				"nodes": []any{},
			},
			"createdAt": time.Now().UTC().Format(time.RFC3339),
			"updatedAt": time.Now().UTC().Format(time.RFC3339),
		})
	}
	raw, err := json.Marshal(nodes)
	if err != nil {
		t.Fatalf("marshal candidate issue nodes: %v", err)
	}
	return string(raw)
}

func initGitWorkspace(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Test User")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "checkout", "-b", "j-29-test")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/example/nonexistent-symphony-test.git")

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

func intPtr(v int) *int {
	return &v
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func writeWorkflowFile(t *testing.T, path string, intervalMs, idleIntervalMs int) {
	t.Helper()
	port := randomFreePort(t)
	content := []byte(
		"---\n" +
			"tracker:\n" +
			"  api_key: test-key\n" +
			"  project_slug: test-project\n" +
			"server:\n" +
			"  port: " + itoa(port) + "\n" +
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

func writeInvalidWorkflowFile(t *testing.T, path string) {
	t.Helper()
	port := randomFreePort(t)

	content := `---
tracker:
  api_key: test-key
  project_slug: test-project
server:
  port: ` + itoa(port) + `
polling:
  interval_ms: 1000
  idle_interval_ms: 2000
workspace:
  root: ""
agent:
  max_concurrent_agents: 4
  max_turns: 3
---
# Workflow
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write invalid workflow: %v", err)
	}
}

func randomFreePort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener addr type %T", ln.Addr())
	}
	return addr.Port
}

func itoa(v int) string {
	return fmt.Sprintf("%d", v)
}

func TestParseStatusPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "unstaged modified",
			input: " M README.md",
			want:  []string{"README.md"},
		},
		{
			name:  "staged modified",
			input: "M  README.md",
			want:  []string{"README.md"},
		},
		{
			name:  "untracked",
			input: "?? new.go",
			want:  []string{"new.go"},
		},
		{
			name:  "rename",
			input: "R  old.go -> new.go",
			want:  []string{"new.go"},
		},
		{
			name:  "multiple files",
			input: " M internal/config/config.go\nM  README.md\n?? extra.txt",
			want:  []string{"internal/config/config.go", "README.md", "extra.txt"},
		},
		{
			name:  "empty output",
			input: "",
			want:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseStatusPaths(tc.input)
			if len(got) != len(tc.want) {
				t.Fatalf("parseStatusPaths(%q) = %v, want %v", tc.input, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("parseStatusPaths(%q)[%d] = %q, want %q", tc.input, i, got[i], tc.want[i])
				}
			}
		})
	}
}
