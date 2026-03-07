package orchestrator

import (
	"context"
	"log/slog"
	"math"
	"time"

	"symphony/internal/config"
)

func (o *Orchestrator) scheduleFailureRetry(ctx context.Context, cfg *config.SymphonyConfig, issueID, identifier string, attemptNum, failureCount int, errMsg string) {
	o.scheduleRetry(ctx, cfg, RetryKindFailure, issueID, identifier, attemptNum, failureCount, 0, errMsg)
}

func (o *Orchestrator) scheduleCapacityRetry(ctx context.Context, cfg *config.SymphonyConfig, issueID, identifier string, attemptNum, failureCount, deferCount int, errMsg string) {
	o.scheduleRetry(ctx, cfg, RetryKindCapacity, issueID, identifier, attemptNum, failureCount, deferCount, errMsg)
}

func (o *Orchestrator) rescheduleRetry(ctx context.Context, cfg *config.SymphonyConfig, entry *RetryEntry, errMsg string) {
	if entry == nil {
		return
	}
	o.scheduleRetry(ctx, cfg, entry.Kind, entry.IssueID, entry.Identifier, entry.Attempt, entry.FailureCount, entry.DeferCount, errMsg)
}

func (o *Orchestrator) scheduleRetry(ctx context.Context, cfg *config.SymphonyConfig, kind RetryKind, issueID, identifier string, attemptNum, failureCount, deferCount int, errMsg string) {
	o.state.mu.Lock()
	if existing, ok := o.state.RetryQueue[issueID]; ok && existing.timer != nil {
		existing.timer.Stop()
	}
	o.state.mu.Unlock()

	var delayMs int
	switch kind {
	case RetryKindFailure:
		maxBackoff := float64(cfg.MaxRetryBackoffMs())
		backoffAttempt := max(failureCount, 1)
		delayMs = int(math.Min(float64(10_000)*math.Pow(2, float64(backoffAttempt-1)), maxBackoff))
	case RetryKindCapacity:
		delayMs = capacityRetryDelayMs
	default:
		delayMs = capacityRetryDelayMs
	}

	entry := &RetryEntry{
		IssueID:      issueID,
		Identifier:   identifier,
		Kind:         kind,
		Attempt:      attemptNum,
		FailureCount: failureCount,
		DeferCount:   deferCount,
		Error:        errMsg,
	}
	o.scheduleRetryAfter(ctx, cfg, entry, time.Duration(delayMs)*time.Millisecond)
}

func (o *Orchestrator) scheduleRetryAfter(ctx context.Context, cfg *config.SymphonyConfig, entry *RetryEntry, delay time.Duration) {
	if entry == nil {
		return
	}

	o.state.mu.Lock()
	if existing, ok := o.state.RetryQueue[entry.IssueID]; ok && existing.timer != nil {
		existing.timer.Stop()
	}
	o.state.mu.Unlock()

	if delay < 0 {
		delay = 0
	}

	entry.DueAt = time.Now().Add(delay)
	entry.timer = time.AfterFunc(delay, func() {
		o.onRetryTimer(ctx, cfg, entry.IssueID)
	})

	o.state.mu.Lock()
	o.state.RetryQueue[entry.IssueID] = entry
	o.state.Claimed[entry.IssueID] = struct{}{}
	o.state.mu.Unlock()

	slog.Info("orchestrator.retry_scheduled", "project", o.name,
		"issue_id", entry.IssueID, "kind", entry.Kind, "attempt", entry.Attempt, "failure_count", entry.FailureCount, "defer_count", entry.DeferCount, "delay_ms", delay.Milliseconds())
}

func (o *Orchestrator) onRetryTimer(ctx context.Context, cfg *config.SymphonyConfig, issueID string) {
	if o.isStopping() {
		o.releaseClaim(issueID)
		return
	}

	o.state.mu.Lock()
	entry, ok := o.state.RetryQueue[issueID]
	if ok {
		delete(o.state.RetryQueue, issueID)
	}
	o.state.mu.Unlock()
	if !ok {
		return
	}
	if o.isDraining() {
		o.releaseClaim(issueID)
		slog.Info("orchestrator.retry_skipped_during_drain", "project", o.name, "issue_id", issueID)
		return
	}
	if until, _, paused := o.admissionPauseState(time.Now().UTC()); paused {
		entry.Error = "rate limit pause"
		o.scheduleRetryAfter(ctx, cfg, entry, time.Until(until))
		return
	}

	o.mu.Lock()
	tr := o.tracker
	o.mu.Unlock()
	if tr == nil {
		o.releaseClaim(issueID)
		return
	}

	issue, err := tr.FetchIssueByID(ctx, issueID)
	if err != nil {
		o.state.RecordTrackerFailure(time.Now().UTC(), err)
		slog.Error("orchestrator.retry_fetch_failed", "issue_id", issueID, "error", err)
		o.rescheduleRetry(ctx, cfg, entry, "poll failed")
		return
	}
	o.state.RecordTrackerSuccess(time.Now().UTC())

	if issue == nil {
		slog.Info("orchestrator.retry_gone", "issue_id", issueID)
		o.releaseClaim(issueID)
		return
	}

	activeNorm := cfg.ActiveNorm()
	if !activeNorm[config.NormalizeState(issue.State)] {
		slog.Info("orchestrator.retry_inactive", "issue_id", issueID, "state", issue.State)
		o.releaseClaim(issueID)
		return
	}
	if config.NormalizeState(issue.State) == humanReviewState {
		slog.Info("orchestrator.retry_paused_for_human_review", "issue_id", issueID, "state", issue.State)
		o.releaseClaim(issueID)
		return
	}

	if o.isBlockedByUrgent(cfg, issue) {
		o.scheduleCapacityRetry(ctx, cfg, issueID, entry.Identifier, entry.Attempt, entry.FailureCount, entry.DeferCount+1, "urgent in progress")
		return
	}

	if !isUrgentIssue(issue) {
		o.state.mu.Lock()
		slots := o.state.MaxConcurrentAgents - o.runningConcurrentCountLocked(cfg)
		o.state.mu.Unlock()

		if slots <= 0 || !o.hasGlobalCapacityForIssue(cfg, issue) {
			o.scheduleCapacityRetry(ctx, cfg, issueID, entry.Identifier, entry.Attempt, entry.FailureCount, entry.DeferCount+1, "no slots")
			return
		}
	}

	o.releaseClaim(issueID)

	if !o.dispatch(ctx, cfg, issue, entry.Attempt, entry.FailureCount) {
		if until, _, paused := o.admissionPauseState(time.Now().UTC()); paused {
			entry.Error = "rate limit pause"
			o.scheduleRetryAfter(ctx, cfg, entry, time.Until(until))
			return
		}
		o.scheduleCapacityRetry(ctx, cfg, issueID, entry.Identifier, entry.Attempt, entry.FailureCount, entry.DeferCount+1, "no global slots")
	}
}

func (o *Orchestrator) clearRetryQueueLocked(releaseClaims bool) {
	for issueID, entry := range o.state.RetryQueue {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		if releaseClaims {
			delete(o.state.Claimed, issueID)
		}
	}
	o.state.RetryQueue = make(map[string]*RetryEntry)
}
