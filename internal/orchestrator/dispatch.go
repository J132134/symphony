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
	if cfg.PauseNorm()[normState] {
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
	if !urgent {
		if hasCapacity, _ := o.hasProjectCapacityLocked(cfg, issue); !hasCapacity {
			return false
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

func (o *Orchestrator) dispatch(ctx context.Context, cfg *config.SymphonyConfig, issue *types.Issue, attemptNum, failureCount int, continuation bool) bool {
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
		Continuation:   continuation,
		StartedAt:      time.Now().UTC(),
		IssueState:     issue.State,
		IssuePriority:  issue.Priority,
		IssueBranch:    issue.BranchName,
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
	needsContinuation := attempt.ShouldContinue()

	if reason == CancelReasonTerminal || reason == CancelReasonReconcile || reason == CancelReasonShutdown {
		o.releaseClaim(issueID)
		slog.Info("orchestrator.worker_cancelled", "project", o.name, "issue_id", issueID, "reason", reason)
		return
	}

	if err == nil {
		if needsContinuation {
			slog.Info("orchestrator.worker_continuing", "project", o.name, "issue_id", issueID, "turns", attempt.SessionSnapshot().TurnCount)
		} else {
			o.state.mu.Lock()
			o.state.CompletedCount++
			o.state.mu.Unlock()
		}
		if draining {
			slog.Info("orchestrator.worker_drained", "project", o.name, "issue_id", issueID)
		} else if needsContinuation {
			slog.Info("orchestrator.worker_session_exhausted", "project", o.name, "issue_id", issueID)
		} else {
			slog.Info("orchestrator.worker_completed", "project", o.name, "issue_id", issueID)
		}
	} else {
		slog.Error("orchestrator.worker_failed", "project", o.name, "issue_id", issueID, "error", err)
	}

	if err == nil && !needsContinuation {
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
		if needsContinuation {
			if attempt.Attempt >= cfg.MaxAttempts() {
				slog.Info("orchestrator.worker_continuation_exhausted", "project", o.name, "issue_id", issueID, "attempt", attempt.Attempt)
				o.releaseClaim(issueID)
			} else {
				o.scheduleContinuationRetry(ctx, cfg, issueID, attempt.Identifier, attempt.Attempt+1)
			}
		} else {
			o.releaseClaim(issueID)
		}
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
		ID:           issue.ID,
		Identifier:   issue.Identifier,
		Title:        issue.Title,
		Description:  issue.Description,
		Priority:     issue.Priority,
		State:        issue.State,
		Labels:       issue.Labels,
		URL:          issue.URL,
		BranchName:   issue.BranchName,
		TurnContext:  initialTurnContext,
		Continuation: attempt.Continuation,
	}, attempt.Attempt)
	if err != nil {
		attempt.SetStatus(StatusFailed)
		attempt.Error = err.Error()
		return fmt.Errorf("render prompt: %w", err)
	}

	attempt.SetStatus(StatusLaunchingAgent)
	agentCfg, err := buildAgentConfig(cfg, ws.Path)
	if err != nil {
		attempt.SetStatus(StatusFailed)
		attempt.Error = err.Error()
		return fmt.Errorf("build agent config: %w", err)
	}

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

	initialHead, _ := workspace.GitOutput(ws.Path, "rev-parse", "HEAD")

	needsContinuation := false
	for turnNum := 1; turnNum <= cfg.MaxTurns(); turnNum++ {
		attempt.SetStatus(StatusStreamingTurn)
		attempt.SetTurnCount(turnNum)

		turnPrompt := prompt
		if turnNum > 1 {
			tc := o.loadTurnContext(issue, wsMgr, ws)
			cont, contErr := workflow.RenderContinuation(wf, workflow.IssueContext{
				ID:          issue.ID,
				Identifier:  issue.Identifier,
				Title:       issue.Title,
				Description: issue.Description,
				State:       issue.State,
				Labels:      issue.Labels,
				URL:         issue.URL,
				TurnContext: tc,
			}, turnNum, cfg.MaxTurns())
			if contErr != nil {
				attempt.SetStatus(StatusFailed)
				attempt.Error = contErr.Error()
				return fmt.Errorf("render continuation prompt: %w", contErr)
			}
			turnPrompt = cont
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
			continue
		}

		active, activeErr := o.isStillActive(ctx, cfg, issue.ID)
		if activeErr != nil {
			slog.Warn("orchestrator.issue_activity_check_failed", "issue", issue.Identifier, "error", activeErr)
			active = true
		}
		if active {
			if madeProgress, _ := hasWorkspaceProgress(ws.Path, initialHead); madeProgress {
				needsContinuation = true
			} else {
				slog.Info("orchestrator.no_progress_detected", "issue", issue.Identifier, "initial_head", initialHead)
			}
		}
	}

	attempt.SetNeedsContinuation(needsContinuation)
	attempt.SetStatus(StatusFinishing)
	runner.StopSession()
	runnerStarted = false

	attempt.FinishedAt = time.Now().UTC()
	if summary, err := collectWorkspaceSummary(ws.Path, issue.BranchName); err == nil {
		attempt.Summary = &summary
	}

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

func (o *Orchestrator) handleAgentEvent(issueID string, attempt *RunAttempt, e agent.Event) {
	attempt.UpdateLastEvent(e.Timestamp)
	attempt.SetLastEventDetail(e.Name, e.Message)
	attempt.UpdateSessionRuntime(e.SessionID, e.PID)
	if e.Usage != nil {
		o.state.mu.Lock()
		o.state.Totals.InputTokens += e.Usage.InputTokens
		o.state.Totals.OutputTokens += e.Usage.OutputTokens
		o.state.Totals.TotalTokens += e.Usage.TotalTokens
		o.state.mu.Unlock()
		attempt.AddTokens(e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.TotalTokens)
	}
	if e.Name == "rate_limit" && e.RateLimit != nil && e.RateLimit.ResetAt != nil {
		o.pauseForRateLimit(issueID, attempt, e)
	}
	slog.Debug("orchestrator.agent_event", "issue_id", issueID, "event", e.Name)
}

func buildAgentConfig(cfg *config.SymphonyConfig, workspacePath string) (*agent.Config, error) {
	writableDirs, err := workspace.GitWritablePaths(workspacePath)
	if err != nil {
		return nil, fmt.Errorf("resolve git writable paths: %w", err)
	}

	return &agent.Config{
		Command:                cfg.CodexCommand(),
		ApprovalPolicy:         cfg.ApprovalPolicy(),
		MaxTurns:               cfg.MaxTurns(),
		TurnTimeoutMs:          cfg.TurnTimeoutMs(),
		ReadTimeoutMs:          cfg.ReadTimeoutMs(),
		ThreadStartTimeoutMs:   cfg.ThreadStartTimeoutMs(),
		StallTimeoutMs:         cfg.StallTimeoutMs(),
		TurnSandboxPolicy:      cfg.TurnSandboxPolicy(),
		ThreadSandbox:          cfg.ThreadSandbox(),
		AdditionalWritableDirs: writableDirs,
	}, nil
}


// hasWorkspaceProgress reports whether the workspace advanced since initialHead.
// Progress means either new commits or uncommitted changes exist.
func hasWorkspaceProgress(wsPath, initialHead string) (bool, error) {
	currentHead, err := workspace.GitOutput(wsPath, "rev-parse", "HEAD")
	if err != nil {
		return false, err
	}
	if currentHead != initialHead {
		return true, nil
	}
	status, err := workspace.GitOutput(wsPath, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(status) != "", nil
}

func isUrgentIssue(issue *types.Issue) bool {
	return issue != nil && issue.Priority != nil && *issue.Priority == urgentPriority
}
