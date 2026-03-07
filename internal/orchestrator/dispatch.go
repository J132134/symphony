package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"symphony/internal/agent"
	"symphony/internal/config"
	"symphony/internal/types"
	"symphony/internal/workflow"
	"symphony/internal/workspace"
)

func (o *Orchestrator) canDispatch(cfg *config.SymphonyConfig, issue *types.Issue) bool {
	normState := config.NormalizeState(issue.State)
	activeNorm := cfg.ActiveNorm()
	termNorm := cfg.TermNorm()
	urgent := isUrgentIssue(issue)

	if !activeNorm[normState] || termNorm[normState] {
		return false
	}
	if _, _, paused := o.admissionPauseState(time.Now().UTC()); paused {
		return false
	}
	if normState == humanReviewState {
		return false
	}

	o.state.mu.Lock()
	defer o.state.mu.Unlock()

	if o.state.Draining {
		return false
	}
	if _, ok := o.state.Running[issue.ID]; ok {
		return false
	}
	if _, ok := o.state.Claimed[issue.ID]; ok {
		return false
	}
	if _, ok := o.state.RetryQueue[issue.ID]; ok {
		return false
	}
	if o.isBlockedByRunningUrgentLocked(cfg, issue) {
		return false
	}
	if !urgent && o.runningConcurrentCountLocked(cfg) >= o.state.MaxConcurrentAgents {
		return false
	}

	if !urgent {
		if byState := cfg.MaxConcurrentAgentsByState(); len(byState) > 0 {
			if limit, ok := byState[normState]; ok {
				count := 0
				for _, r := range o.state.Running {
					if config.NormalizeState(r.IssueState) == normState {
						count++
					}
				}
				if count >= limit {
					return false
				}
			}
		}
	}

	if normState == "todo" {
		for _, blocker := range issue.BlockedBy {
			if !termNorm[config.NormalizeState(blocker.State)] {
				slog.Debug("orchestrator.blocked", "issue", issue.Identifier, "blocker", blocker.Identifier)
				return false
			}
		}
	}
	return true
}

func sortCandidates(issues []*types.Issue) {
	sort.Slice(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		pa, pb := 999, 999
		if a.Priority != nil {
			pa = *a.Priority
		}
		if b.Priority != nil {
			pb = *b.Priority
		}
		if pa != pb {
			return pa < pb
		}
		if a.CreatedAt != nil && b.CreatedAt != nil && !a.CreatedAt.Equal(*b.CreatedAt) {
			return a.CreatedAt.Before(*b.CreatedAt)
		}
		return a.Identifier < b.Identifier
	})
}

func (o *Orchestrator) dispatch(ctx context.Context, cfg *config.SymphonyConfig, issue *types.Issue, attemptNum, failureCount int) bool {
	if until, reason, paused := o.admissionPauseState(time.Now().UTC()); paused {
		slog.Info("orchestrator.dispatch_paused", "project", o.name, "issue", issue.Identifier, "resume_at", until.Format(time.RFC3339), "reason", reason)
		return false
	}

	urgent := isUrgentIssue(issue)
	if urgent {
		o.preemptForUrgent(cfg, issue)
	}

	globalSlotHeld := false
	if shouldAcquireGlobalSlot(cfg, issue) && o.globalLimiter != nil {
		if urgent {
			globalSlotHeld = o.globalLimiter.ForceAcquireIssue(issue.ID, true, func() {
				o.preemptIssue(issue.ID)
			})
		} else {
			globalSlotHeld = o.globalLimiter.TryAcquireIssue(issue.ID, false, func() {
				o.preemptIssue(issue.ID)
			})
		}
		if !globalSlotHeld {
			slog.Debug("orchestrator.global_limit_reached", "project", o.name, "issue", issue.Identifier)
			return false
		}
	}

	slog.Info("orchestrator.dispatching", "project", o.name, "issue", issue.Identifier, "attempt", attemptNum)

	wctx, cancel := context.WithCancel(context.Background())
	attempt := &RunAttempt{
		IssueID:        issue.ID,
		Identifier:     issue.Identifier,
		Attempt:        attemptNum,
		FailureCount:   failureCount,
		StartedAt:      time.Now().UTC(),
		IssueState:     issue.State,
		GlobalSlotHeld: globalSlotHeld,
		Urgent:         urgent,
		cancel:         cancel,
	}
	attempt.SetStatus(StatusPreparingWorkspace)
	handle := &workerHandle{
		cancel:  cancel,
		attempt: attempt,
	}

	o.state.mu.Lock()
	delete(o.state.Abandoned, issue.ID)
	o.state.Claimed[issue.ID] = struct{}{}
	o.state.Running[issue.ID] = attempt
	o.state.mu.Unlock()

	o.mu.Lock()
	o.workersByIssue[issue.ID] = handle
	o.mu.Unlock()

	o.workers.Add(1)
	go func() {
		defer o.workers.Done()
		err := o.runAttempt(wctx, cfg, issue, attempt, handle)
		o.onWorkerDone(ctx, cfg, issue.ID, attempt, handle, err)
	}()
	return true
}

func (o *Orchestrator) onWorkerDone(ctx context.Context, cfg *config.SymphonyConfig, issueID string, attempt *RunAttempt, handle *workerHandle, err error) {
	defer o.releaseGlobalSlot(attempt)
	defer attempt.ClearDrainDeadline()

	o.state.mu.Lock()
	delete(o.state.Running, issueID)
	o.state.mu.Unlock()

	o.mu.Lock()
	delete(o.workersByIssue, issueID)
	tr := o.tracker
	wsMgr := o.wsMgr
	o.mu.Unlock()

	if attempt != nil && attempt.Preempted && err != nil {
		slog.Info("orchestrator.worker_preempted", "project", o.name, "issue_id", issueID, "identifier", attempt.Identifier)
		if o.isDraining() {
			o.releaseClaim(issueID)
			return
		}
		o.scheduleCapacityRetry(ctx, cfg, issueID, attempt.Identifier, attempt.Attempt, attempt.FailureCount, 1, "preempted by urgent")
		return
	}
	if handle != nil {
		handle.stopDrainTimer()
	}

	if attempt.ShouldCleanupOnExit() && wsMgr != nil && attempt.WorkspacePath != "" {
		cleanupCtx := context.Background()
		cancelCleanup := func() {}
		if deadlineCtx, cancel := attempt.DrainContext(); deadlineCtx != nil {
			cleanupCtx = deadlineCtx
			cancelCleanup = cancel
		}
		ws := &workspace.Workspace{
			Path: attempt.WorkspacePath,
			Key:  workspace.SanitizeIdentifier(attempt.Identifier),
		}
		if cleanupErr := wsMgr.Cleanup(cleanupCtx, ws); cleanupErr != nil {
			slog.Warn("orchestrator.cleanup_on_exit_failed", "issue", attempt.Identifier, "error", cleanupErr)
		}
		cancelCleanup()
	}

	draining := o.isDraining()
	reason := attempt.GetCancelReason()
	cancelled := isWorkerCancelled(err)

	if reason == CancelReasonTerminal || reason == CancelReasonReconcile || reason == CancelReasonShutdown {
		o.releaseClaim(issueID)
		slog.Info("orchestrator.worker_cancelled", "project", o.name, "issue_id", issueID, "reason", reason)
		return
	}

	if err == nil {
		o.state.mu.Lock()
		o.state.CompletedCount++
		o.state.mu.Unlock()
		if draining {
			slog.Info("orchestrator.worker_drained", "project", o.name, "issue_id", issueID)
		} else {
			slog.Info("orchestrator.worker_completed", "project", o.name, "issue_id", issueID)
		}
	} else {
		slog.Error("orchestrator.worker_failed", "project", o.name, "issue_id", issueID, "error", err)
	}

	if err == nil {
		o.maybePostSuccessFeedback(ctx, cfg, tr, issueID, attempt)
	} else if attempt.Attempt >= cfg.MaxAttempts() {
		o.maybePostFinalFailureFeedback(ctx, cfg, tr, issueID, attempt, err)
	}

	if draining {
		o.releaseClaim(issueID)
		if err != nil || cancelled {
			slog.Info("orchestrator.worker_finished_during_drain", "project", o.name, "issue_id", issueID)
		}
		return
	}

	if cancelled && reason == CancelReasonStall {
		o.scheduleFailureRetry(ctx, cfg, issueID, attempt.Identifier, attempt.Attempt+1, attempt.FailureCount+1, "worker cancelled after stall detection")
		return
	}
	if cancelled {
		o.releaseClaim(issueID)
		slog.Info("orchestrator.worker_cancelled", "project", o.name, "issue_id", issueID)
		return
	}

	if err == nil {
		o.releaseClaim(issueID)
	} else {
		if attempt.Attempt >= cfg.MaxAttempts() {
			o.releaseClaim(issueID)
			slog.Info("orchestrator.worker_failed_final", "project", o.name, "issue_id", issueID, "attempt", attempt.Attempt)
			return
		}
		o.scheduleFailureRetry(ctx, cfg, issueID, attempt.Identifier, attempt.Attempt+1, attempt.FailureCount+1, err.Error())
	}
}

func (o *Orchestrator) runAttempt(ctx context.Context, cfg *config.SymphonyConfig, issue *types.Issue, attempt *RunAttempt, handle *workerHandle) error {
	o.mu.Lock()
	wsMgr := o.wsMgr
	wf := o.wf
	o.mu.Unlock()

	runner := agent.NewRunner()
	runnerStarted := false
	defer func() {
		if runnerStarted {
			if handle != nil {
				handle.clearRunner()
			}
			runner.StopSession()
		}
	}()

	attempt.SetStatus(StatusPreparingWorkspace)
	ws, err := wsMgr.Setup(ctx, issue.Identifier)
	if err != nil {
		attempt.SetStatus(StatusFailed)
		attempt.Error = err.Error()
		return fmt.Errorf("setup workspace: %w", err)
	}
	attempt.WorkspacePath = ws.Path

	if err := wsMgr.PrepareForRun(ctx, ws); err != nil {
		attempt.SetStatus(StatusFailed)
		attempt.Error = err.Error()
		return fmt.Errorf("before_run: %w", err)
	}

	attempt.SetStatus(StatusBuildingPrompt)
	initialTurnContext := o.loadTurnContext(issue, wsMgr, ws)
	prompt, err := workflow.Render(wf, workflow.IssueContext{
		ID:          issue.ID,
		Identifier:  issue.Identifier,
		Title:       issue.Title,
		Description: issue.Description,
		Priority:    issue.Priority,
		State:       issue.State,
		Labels:      issue.Labels,
		URL:         issue.URL,
		BranchName:  issue.BranchName,
		TurnContext: initialTurnContext,
	}, attempt.Attempt)
	if err != nil {
		attempt.SetStatus(StatusFailed)
		attempt.Error = err.Error()
		return fmt.Errorf("render prompt: %w", err)
	}

	attempt.SetStatus(StatusLaunchingAgent)
	agentCfg := buildAgentConfig(cfg)

	attempt.SetStatus(StatusInitializingSession)
	threadID, err := runner.StartSession(ctx, ws.Path, agentCfg)
	if err != nil {
		attempt.SetStatus(StatusFailed)
		attempt.Error = err.Error()
		return fmt.Errorf("start session: %w", err)
	}
	runnerStarted = true
	if handle != nil {
		handle.setRunner(runner, agentCfg)
	}
	attempt.SetSessionIdentity(threadID, runner.SessionID(), runner.PID())

	for turnNum := 1; turnNum <= cfg.MaxTurns(); turnNum++ {
		attempt.SetStatus(StatusStreamingTurn)
		attempt.SetTurnCount(turnNum)

		turnPrompt := prompt
		if turnNum > 1 {
			turnPrompt = buildContinuationPrompt(
				issue.Identifier,
				issue.Title,
				turnNum,
				cfg.MaxTurns(),
				o.loadTurnContext(issue, wsMgr, ws),
			)
		}

		turnID := fmt.Sprintf("%d", time.Now().UnixNano())
		attempt.SetActiveTurn(threadID, turnID)
		result := runner.RunTurn(ctx, threadID, turnID, turnPrompt, issue.Identifier, issue.Title, agentCfg,
			func(e agent.Event) { o.handleAgentEvent(issue.ID, attempt, e) })
		attempt.ClearActiveTurn(turnID)

		if ctx.Err() != nil {
			attempt.SetStatus(StatusCanceled)
			attempt.Error = ctx.Err().Error()
			return ctx.Err()
		}

		if !result.Success {
			if !result.CompletedNaturally {
				attempt.SetStatus(StatusFailed)
				attempt.Error = result.Error
				if attempt.GetCancelReason() != CancelReasonNone || strings.Contains(result.Error, "cancelled") {
					return workerCancelledError(attempt.GetCancelReason(), result.Error)
				}
				return fmt.Errorf("turn failed: %s", result.Error)
			}
			slog.Info("orchestrator.turn_failed_naturally", "issue", issue.Identifier, "turn", turnNum)
			break
		}

		if turnNum < cfg.MaxTurns() {
			if active, _ := o.isStillActive(ctx, cfg, issue.ID); !active {
				slog.Info("orchestrator.issue_no_longer_active", "issue", issue.Identifier)
				break
			}
		}
	}

	attempt.SetStatus(StatusFinishing)
	runner.StopSession()
	runnerStarted = false

	if _, err := wsMgr.FinishRun(ctx, ws); err != nil {
		slog.Warn("orchestrator.after_run_failed", "issue", issue.Identifier, "error", err)
	}
	attempt.SetStatus(StatusSucceeded)
	return nil
}

func (o *Orchestrator) loadTurnContext(issue *types.Issue, wsMgr *workspace.Manager, ws *workspace.Workspace) string {
	if wsMgr == nil || ws == nil {
		return ""
	}
	turnContext, err := wsMgr.GetTurnContext(ws)
	if err != nil {
		slog.Warn("orchestrator.turn_context_unavailable", "issue", issue.Identifier, "error", err)
		return ""
	}
	return turnContext
}

func buildContinuationPrompt(identifier, title string, turnNum, maxTurns int, turnContext string) string {
	if strings.TrimSpace(turnContext) == "" {
		return fmt.Sprintf("Continue working on %s: %s. This is turn %d of %d.",
			identifier, title, turnNum, maxTurns)
	}

	return fmt.Sprintf(
		"Continue working on %s: %s.\n\nProgress so far:\n%s\n\nThis is turn %d of %d. Continue where you left off without repeating completed work.",
		identifier,
		title,
		turnContext,
		turnNum,
		maxTurns,
	)
}

func (o *Orchestrator) handleAgentEvent(issueID string, attempt *RunAttempt, e agent.Event) {
	attempt.UpdateLastEvent(e.Timestamp)
	attempt.UpdateSessionRuntime(e.SessionID, e.PID)
	if e.Usage != nil {
		o.state.mu.Lock()
		o.state.Totals.InputTokens += e.Usage.InputTokens
		o.state.Totals.OutputTokens += e.Usage.OutputTokens
		o.state.Totals.TotalTokens += e.Usage.TotalTokens
		o.state.mu.Unlock()
		attempt.AddTokens(e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.TotalTokens)
	}
	if e.Name == "rate_limit" {
		o.pauseForRateLimit(issueID, attempt, e)
	}
	slog.Debug("orchestrator.agent_event", "issue_id", issueID, "event", e.Name)
}

func buildAgentConfig(cfg *config.SymphonyConfig) *agent.Config {
	return &agent.Config{
		Command:              cfg.CodexCommand(),
		ApprovalPolicy:       cfg.ApprovalPolicy(),
		MaxTurns:             cfg.MaxTurns(),
		TurnTimeoutMs:        cfg.TurnTimeoutMs(),
		ReadTimeoutMs:        cfg.ReadTimeoutMs(),
		ThreadStartTimeoutMs: cfg.ThreadStartTimeoutMs(),
		StallTimeoutMs:       cfg.StallTimeoutMs(),
		TurnSandboxPolicy:    cfg.TurnSandboxPolicy(),
		ThreadSandbox:        cfg.ThreadSandbox(),
	}
}

func isUrgentIssue(issue *types.Issue) bool {
	return issue != nil && issue.Priority != nil && *issue.Priority == urgentPriority
}
