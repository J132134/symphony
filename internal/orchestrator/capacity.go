package orchestrator

import (
	"log/slog"
	"strings"
	"time"

	"symphony/internal/agent"
	"symphony/internal/config"
	"symphony/internal/types"
)

func (o *Orchestrator) runningConcurrentCountLocked(cfg *config.SymphonyConfig) int {
	count := 0
	for _, attempt := range o.state.Running {
		if countsTowardConcurrency(cfg, attempt.IssueState) {
			count++
		}
	}
	return count
}

func countsTowardConcurrency(cfg *config.SymphonyConfig, state string) bool {
	return !isPauseState(cfg, state)
}

func (o *Orchestrator) hasRunningUrgentLocked(cfg *config.SymphonyConfig) bool {
	for _, attempt := range o.state.Running {
		if attempt != nil && attempt.Urgent && countsTowardConcurrency(cfg, attempt.IssueState) {
			return true
		}
	}
	return false
}

func (o *Orchestrator) isBlockedByRunningUrgentLocked(cfg *config.SymphonyConfig, issue *types.Issue) bool {
	return !isUrgentIssue(issue) && o.hasRunningUrgentLocked(cfg)
}

func (o *Orchestrator) isBlockedByUrgent(cfg *config.SymphonyConfig, issue *types.Issue) bool {
	if isUrgentIssue(issue) {
		return false
	}

	o.state.mu.Lock()
	localUrgent := o.hasRunningUrgentLocked(cfg)
	o.state.mu.Unlock()
	if localUrgent {
		return true
	}
	return o.globalLimiter != nil && o.globalLimiter.HasUrgent()
}

func (o *Orchestrator) hasGlobalCapacityForState(cfg *config.SymphonyConfig, state string) bool {
	if !countsTowardConcurrency(cfg, state) {
		return true
	}
	return o.hasGlobalCapacity()
}

func (o *Orchestrator) hasGlobalCapacityForIssue(cfg *config.SymphonyConfig, issue *types.Issue) bool {
	if issue == nil || isUrgentIssue(issue) || !shouldAcquireGlobalSlot(cfg, issue) {
		return true
	}
	return o.hasGlobalCapacityForState(cfg, issue.State)
}

func isCtxErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "context canceled") || strings.Contains(s, "context deadline exceeded")
}

func shouldAcquireGlobalSlot(cfg *config.SymphonyConfig, issue *types.Issue) bool {
	return issue != nil && countsTowardConcurrency(cfg, issue.State)
}

func (o *Orchestrator) hasGlobalCapacity() bool {
	return o.globalLimiter == nil || (!o.globalLimiter.HasUrgent() && o.globalLimiter.Available() > 0)
}

func (o *Orchestrator) releaseClaim(issueID string) {
	o.state.mu.Lock()
	delete(o.state.Claimed, issueID)
	o.state.mu.Unlock()
}

func (o *Orchestrator) preemptForUrgent(cfg *config.SymphonyConfig, issue *types.Issue) {
	if !isUrgentIssue(issue) {
		return
	}

	preempted := map[string]struct{}{}

	o.state.mu.Lock()
	for issueID, attempt := range o.state.Running {
		if issueID == issue.ID || attempt == nil || attempt.Urgent || attempt.Preempted || !countsTowardConcurrency(cfg, attempt.IssueState) {
			continue
		}
		preempted[issueID] = struct{}{}
	}
	o.state.mu.Unlock()

	if o.globalLimiter != nil {
		for _, issueID := range o.globalLimiter.PreemptNonUrgent(issue.ID) {
			preempted[issueID] = struct{}{}
		}
	}
	for issueID := range preempted {
		o.preemptIssue(issueID)
	}

	if len(preempted) > 0 {
		slog.Info("orchestrator.urgent_preempted", "project", o.name, "issue", issue.Identifier, "count", len(preempted))
	}
}

func (o *Orchestrator) preemptIssue(issueID string) {
	now := time.Now().UTC()

	o.state.mu.Lock()
	attempt, ok := o.state.Running[issueID]
	if !ok || attempt == nil || attempt.Urgent || attempt.Preempted {
		o.state.mu.Unlock()
		return
	}
	attempt.Preempted = true
	attempt.SetStatus(StatusCanceled)
	o.state.Abandoned[issueID] = &AbandonedEntry{
		Identifier:  attempt.Identifier,
		State:       attempt.IssueState,
		Error:       "preempted_by_urgent",
		AbandonedAt: now,
	}
	o.state.mu.Unlock()

	o.cancelWorker(issueID, CancelReasonNone)
}

func (o *Orchestrator) pauseForRateLimit(issueID string, attempt *RunAttempt, e agent.Event) {
	now := e.Timestamp.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	o.state.mu.Lock()
	until, reason := o.pauseStateLocked(now, e.RateLimit)
	o.state.mu.Unlock()

	if o.globalLimiter != nil {
		o.globalLimiter.PauseUntil(until)
	}

	attrs := []any{
		"project", o.name,
		"issue_id", issueID,
		"resume_at", until.Format(time.RFC3339),
		"reason", reason,
	}
	if attempt != nil {
		attrs = append(attrs, "issue", attempt.Identifier)
	}
	if e.RateLimit != nil && e.RateLimit.ResetAt != nil {
		attrs = append(attrs, "source_reset_at", e.RateLimit.ResetAt.Format(time.RFC3339))
	}
	slog.Warn("orchestrator.rate_limit_pause", attrs...)
}

func (o *Orchestrator) pauseStateLocked(now time.Time, rateLimit *agent.RateLimitEvent) (time.Time, string) {
	if rateLimit != nil && rateLimit.ResetAt != nil && rateLimit.ResetAt.After(now) {
		until := rateLimit.ResetAt.UTC()
		if o.state.PausedUntil != nil && o.state.PausedUntil.After(until) {
			until = o.state.PausedUntil.UTC()
		}
		o.state.PausedUntil = &until
		o.state.PauseReason = "rate_limit_reset"
		o.state.RateLimitPauseCount = 0
		return until, o.state.PauseReason
	}

	o.state.RateLimitPauseCount++
	shift := o.state.RateLimitPauseCount - 1
	if shift > 6 {
		shift = 6
	}
	backoff := rateLimitPauseInitialBackoff * time.Duration(1<<shift)
	if backoff > rateLimitPauseMaxBackoff {
		backoff = rateLimitPauseMaxBackoff
	}
	until := now.Add(backoff)
	if o.state.PausedUntil != nil && o.state.PausedUntil.After(until) {
		until = o.state.PausedUntil.UTC()
	}
	o.state.PausedUntil = &until
	o.state.PauseReason = "rate_limit_backoff"
	return until, o.state.PauseReason
}

func (o *Orchestrator) admissionPauseState(now time.Time) (time.Time, string, bool) {
	var until time.Time
	var reason string
	var paused bool

	if o.globalLimiter != nil {
		if globalUntil, ok := o.globalLimiter.PausedUntil(); ok {
			until = globalUntil
			reason = "global_rate_limit"
			paused = true
		}
	}

	o.state.mu.Lock()
	defer o.state.mu.Unlock()

	if o.state.PausedUntil != nil {
		if o.state.PausedUntil.After(now) {
			if !paused || o.state.PausedUntil.After(until) {
				until = o.state.PausedUntil.UTC()
				reason = o.state.PauseReason
				paused = true
			}
		} else {
			o.state.PausedUntil = nil
			o.state.PauseReason = ""
			o.state.RateLimitPauseCount = 0
		}
	}

	return until, reason, paused
}

func (o *Orchestrator) isStopping() bool {
	select {
	case <-o.stopCh:
		return true
	default:
		return false
	}
}

func (o *Orchestrator) isDraining() bool {
	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	return o.state.Draining
}

func (o *Orchestrator) releaseGlobalSlot(attempt *RunAttempt) {
	if attempt == nil || !attempt.GlobalSlotHeld || o.globalLimiter == nil {
		return
	}
	o.globalLimiter.ReleaseIssue(attempt.IssueID)
	attempt.GlobalSlotHeld = false
}
