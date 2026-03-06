// Package orchestrator is the main poll-dispatch-reconcile loop for one project.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"symphony/internal/agent"
	"symphony/internal/config"
	"symphony/internal/tracker"
	"symphony/internal/types"
	"symphony/internal/workflow"
	"symphony/internal/workspace"
)

const retryAbandonCommentMarker = "<!-- symphony:retry-abandoned -->"

// Orchestrator drives one project: polls Linear, dispatches agents, reconciles.
type Orchestrator struct {
	workflowPath  string
	name          string
	globalLimiter *SessionLimiter

	mu      sync.Mutex
	cfg     *config.SymphonyConfig
	wf      *workflow.Definition
	tracker *tracker.LinearClient
	wsMgr   *workspace.Manager

	state *State

	// workerCancels maps issue_id → context cancel for the running worker.
	workerCancels map[string]context.CancelFunc

	stopCh chan struct{}
	doneCh chan struct{}
}

// New creates an Orchestrator. Call Run to start it.
func New(workflowPath string, port int, name string, globalLimiter *SessionLimiter) *Orchestrator {
	return &Orchestrator{
		workflowPath:  workflowPath,
		name:          name,
		globalLimiter: globalLimiter,
		state:         NewState(),
		workerCancels: make(map[string]context.CancelFunc),
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
}

// Run starts the orchestrator and blocks until ctx is done or Stop is called.
func (o *Orchestrator) Run(ctx context.Context) error {
	if err := o.reloadWorkflow(); err != nil {
		return fmt.Errorf("load workflow: %w", err)
	}

	o.mu.Lock()
	cfg := o.cfg
	o.mu.Unlock()

	if errs := cfg.Validate(); len(errs) > 0 {
		return fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}

	tr, err := tracker.NewLinearClient(
		cfg.TrackerAPIKey(), cfg.TrackerEndpoint(),
		cfg.TrackerProjectSlug(), cfg.ActiveStates(),
	)
	if err != nil {
		return fmt.Errorf("tracker: %w", err)
	}

	wsMgr, err := workspace.NewManager(cfg.WorkspaceRoot(), cfg.Hooks(), cfg.HooksTimeoutMs())
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}

	o.mu.Lock()
	o.tracker = tr
	o.wsMgr = wsMgr
	o.mu.Unlock()

	o.state.mu.Lock()
	o.state.PollIntervalMs = cfg.PollIntervalMs()
	o.state.PollIntervalIdleMs = cfg.PollIntervalIdleMs()
	o.state.MaxConcurrentAgents = cfg.MaxConcurrentAgents()
	o.state.mu.Unlock()

	o.startupCleanup(ctx)
	go o.watchWorkflow(ctx)

	attrs := []any{
		"project", o.name,
		"poll_ms", cfg.PollIntervalMs(),
		"max_concurrent", cfg.MaxConcurrentAgents(),
	}
	if o.globalLimiter != nil {
		attrs = append(attrs, "max_total_concurrent_sessions", o.globalLimiter.Limit())
	}
	slog.Info("orchestrator.started", attrs...)

	defer close(o.doneCh)
	return o.pollLoop(ctx)
}

// Stop signals shutdown and waits for the loop to exit.
func (o *Orchestrator) Stop() {
	o.requestStop(false)
	o.mu.Lock()
	for _, cancel := range o.workerCancels {
		cancel()
	}
	o.mu.Unlock()
	<-o.doneCh
}

// DrainAndStop stops polling and waits for running turns to finish naturally.
func (o *Orchestrator) DrainAndStop() {
	o.requestStop(true)
	<-o.doneCh
}

func (o *Orchestrator) requestStop(drain bool) {
	if drain {
		o.state.mu.Lock()
		if !o.state.Draining {
			o.state.Draining = true
			o.clearRetryQueueLocked(true)
		}
		o.state.mu.Unlock()
	}
	select {
	case <-o.stopCh:
	default:
		close(o.stopCh)
	}
}

func (o *Orchestrator) GetState() *State { return o.state }

func (o *Orchestrator) BeginDrain() {
	o.state.mu.Lock()
	if o.state.Draining {
		o.state.mu.Unlock()
		return
	}
	o.state.Draining = true
	o.clearRetryQueueLocked(true)
	o.state.mu.Unlock()

	slog.Info("orchestrator.draining", "project", o.name)
}

func (o *Orchestrator) IsIdle() bool {
	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	return len(o.state.Running) == 0 && len(o.state.RetryQueue) == 0
}

func (o *Orchestrator) TriggerRefresh(ctx context.Context) { go o.tick(ctx) }

// -- poll loop --

func (o *Orchestrator) pollLoop(ctx context.Context) error {
	o.tick(ctx)

	for {
		o.state.mu.Lock()
		active := len(o.state.Running) > 0 || len(o.state.RetryQueue) > 0
		intervalMs := o.state.PollIntervalIdleMs
		if active {
			intervalMs = o.state.PollIntervalMs
		}
		o.state.mu.Unlock()

		select {
		case <-o.stopCh:
			return o.gracefulStop(o.isDraining())
		case <-ctx.Done():
			return o.gracefulStop(false)
		case <-time.After(time.Duration(intervalMs) * time.Millisecond):
		}
		o.tick(ctx)
	}
}

func (o *Orchestrator) gracefulStop(drain bool) error {
	slog.Info("orchestrator.stopping", "project", o.name)

	o.state.mu.Lock()
	o.clearRetryQueueLocked(true)
	o.state.mu.Unlock()

	if drain {
		for {
			o.state.mu.Lock()
			running := len(o.state.Running)
			o.state.mu.Unlock()
			if running == 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	} else {
		o.mu.Lock()
		for _, cancel := range o.workerCancels {
			cancel()
		}
		o.mu.Unlock()
	}

	slog.Info("orchestrator.stopped", "project", o.name)
	return nil
}

// -- tick --

func (o *Orchestrator) tick(ctx context.Context) {
	if o.isStopping() {
		return
	}
	slog.Debug("orchestrator.tick", "project", o.name)
	o.reconcile(ctx)

	if o.isDraining() {
		return
	}

	o.mu.Lock()
	cfg := o.cfg
	tr := o.tracker
	o.mu.Unlock()

	if cfg == nil || tr == nil || len(cfg.Validate()) > 0 {
		return
	}

	candidates, err := tr.FetchCandidateIssues(ctx)
	if err != nil {
		o.state.RecordTrackerFailure(time.Now().UTC(), err)
		slog.Error("orchestrator.fetch_failed", "project", o.name, "error", err)
		return
	}
	o.state.RecordTrackerSuccess(time.Now().UTC())

	sortCandidates(candidates)

	for _, issue := range candidates {
		if o.isStopping() || o.isDraining() {
			return
		}

		o.state.mu.Lock()
		slots := o.state.MaxConcurrentAgents - len(o.state.Running)
		o.state.mu.Unlock()

		if slots <= 0 {
			break
		}
		if !o.hasGlobalCapacity() {
			break
		}
		if o.canDispatch(cfg, issue) {
			if !o.dispatch(ctx, cfg, issue, 1, 0) {
				break
			}
		}
	}
}

// -- candidate selection --

func (o *Orchestrator) canDispatch(cfg *config.SymphonyConfig, issue *types.Issue) bool {
	normState := config.NormalizeState(issue.State)
	activeNorm := normStates(cfg.ActiveStates())
	termNorm := normStates(cfg.TerminalStates())

	if !activeNorm[normState] || termNorm[normState] {
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
	if abandoned, ok := o.state.Abandoned[issue.ID]; ok {
		if config.NormalizeState(abandoned.State) == normState && !issueUpdatedAfter(abandoned.ResumeAfter(issue), issue.UpdatedAt) {
			return false
		}
		delete(o.state.Abandoned, issue.ID)
	}
	if len(o.state.Running) >= o.state.MaxConcurrentAgents {
		return false
	}

	// Per-state concurrency.
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

	// Blocker gate for "todo" state.
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

// -- dispatch --

func (o *Orchestrator) dispatch(ctx context.Context, cfg *config.SymphonyConfig, issue *types.Issue, attemptNum, failureCount int) bool {
	if o.globalLimiter != nil && !o.globalLimiter.TryAcquire() {
		slog.Debug("orchestrator.global_limit_reached", "project", o.name, "issue", issue.Identifier)
		return false
	}

	slog.Info("orchestrator.dispatching", "project", o.name, "issue", issue.Identifier, "attempt", attemptNum)

	wctx, cancel := context.WithCancel(ctx)
	attempt := &RunAttempt{
		IssueID:        issue.ID,
		Identifier:     issue.Identifier,
		Attempt:        attemptNum,
		FailureCount:   failureCount,
		StartedAt:      time.Now().UTC(),
		Status:         StatusPreparingWorkspace,
		IssueState:     issue.State,
		GlobalSlotHeld: o.globalLimiter != nil,
		cancel:         cancel,
	}

	o.state.mu.Lock()
	o.state.Claimed[issue.ID] = struct{}{}
	o.state.Running[issue.ID] = attempt
	o.state.mu.Unlock()

	o.mu.Lock()
	o.workerCancels[issue.ID] = cancel
	o.mu.Unlock()

	go func() {
		err := o.runAttempt(wctx, cfg, issue, attempt)
		o.onWorkerDone(ctx, cfg, issue.ID, attempt, err)
	}()
	return true
}

func (o *Orchestrator) onWorkerDone(ctx context.Context, cfg *config.SymphonyConfig, issueID string, attempt *RunAttempt, err error) {
	defer o.releaseGlobalSlot(attempt)

	o.state.mu.Lock()
	delete(o.state.Running, issueID)
	o.state.mu.Unlock()

	o.mu.Lock()
	delete(o.workerCancels, issueID)
	o.mu.Unlock()

	ctxErr := ctx.Err() != nil || isCtxErr(err)

	if ctxErr {
		o.state.mu.Lock()
		delete(o.state.Claimed, issueID)
		o.state.mu.Unlock()
		slog.Info("orchestrator.worker_cancelled", "project", o.name, "issue_id", issueID)
		return
	}

	draining := o.isDraining()

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

	if draining {
		o.state.mu.Lock()
		delete(o.state.Claimed, issueID)
		o.state.mu.Unlock()
		if err != nil {
			slog.Info("orchestrator.worker_finished_during_drain", "project", o.name, "issue_id", issueID)
		}
		return
	}

	if err == nil {
		o.scheduleRetry(ctx, cfg, issueID, attempt.Identifier, attempt.Attempt, 0, "", false)
	} else {
		failureCount := attempt.FailureCount + 1
		if o.shouldAbandonRetry(cfg, failureCount) {
			o.abandonRetry(ctx, cfg, issueID, attempt, err.Error())
			return
		}
		o.scheduleRetry(ctx, cfg, issueID, attempt.Identifier, attempt.Attempt+1, failureCount, err.Error(), true)
	}
}

// -- agent attempt --

func (o *Orchestrator) runAttempt(ctx context.Context, cfg *config.SymphonyConfig, issue *types.Issue, attempt *RunAttempt) error {
	o.mu.Lock()
	wsMgr := o.wsMgr
	wf := o.wf
	o.mu.Unlock()

	runner := agent.NewRunner()
	runnerStarted := false
	defer func() {
		if runnerStarted {
			runner.StopSession()
		}
	}()

	// 1. Workspace.
	attempt.Status = StatusPreparingWorkspace
	ws, err := wsMgr.Setup(ctx, issue.Identifier)
	if err != nil {
		attempt.Status = StatusFailed
		attempt.Error = err.Error()
		return fmt.Errorf("setup workspace: %w", err)
	}
	attempt.WorkspacePath = ws.Path

	// 2. before_run hook.
	if err := wsMgr.PrepareForRun(ctx, ws); err != nil {
		attempt.Status = StatusFailed
		attempt.Error = err.Error()
		return fmt.Errorf("before_run: %w", err)
	}

	// 3. Render prompt.
	attempt.Status = StatusBuildingPrompt
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
	}, attempt.Attempt)
	if err != nil {
		attempt.Status = StatusFailed
		attempt.Error = err.Error()
		return fmt.Errorf("render prompt: %w", err)
	}

	// 4. Launch agent.
	attempt.Status = StatusLaunchingAgent
	agentCfg := &agent.Config{
		Command:           cfg.CodexCommand(),
		ApprovalPolicy:    cfg.ApprovalPolicy(),
		MaxTurns:          cfg.MaxTurns(),
		TurnTimeoutMs:     cfg.TurnTimeoutMs(),
		ReadTimeoutMs:     cfg.ReadTimeoutMs(),
		StallTimeoutMs:    cfg.StallTimeoutMs(),
		TurnSandboxPolicy: cfg.TurnSandboxPolicy(),
		ThreadSandbox:     cfg.ThreadSandbox(),
	}

	// 5. Handshake.
	attempt.Status = StatusInitializingSession
	threadID, err := runner.StartSession(ctx, ws.Path, agentCfg)
	if err != nil {
		attempt.Status = StatusFailed
		attempt.Error = err.Error()
		return fmt.Errorf("start session: %w", err)
	}
	runnerStarted = true
	attempt.Session.ThreadID = threadID
	attempt.Session.SessionID = runner.SessionID()
	attempt.Session.AgentPID = runner.PID()

	// 6. Turn loop.
	for turnNum := 1; turnNum <= cfg.MaxTurns(); turnNum++ {
		attempt.Status = StatusStreamingTurn
		attempt.Session.TurnCount = turnNum

		turnPrompt := prompt
		if turnNum > 1 {
			turnPrompt = fmt.Sprintf("Continue working on %s: %s. This is turn %d of %d.",
				issue.Identifier, issue.Title, turnNum, cfg.MaxTurns())
		}

		result := runner.RunTurn(ctx, threadID, turnPrompt, issue.Identifier, issue.Title, agentCfg,
			func(e agent.Event) { o.handleAgentEvent(issue.ID, attempt, e) })

		if !result.Success {
			if !result.CompletedNaturally {
				attempt.Status = StatusFailed
				attempt.Error = result.Error
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

	// 7. Finish.
	attempt.Status = StatusFinishing
	runner.StopSession()
	runnerStarted = false

	if err := wsMgr.FinishRun(ctx, ws); err != nil {
		slog.Warn("orchestrator.after_run_failed", "issue", issue.Identifier, "error", err)
	}
	attempt.Status = StatusSucceeded
	return nil
}

func (o *Orchestrator) handleAgentEvent(issueID string, attempt *RunAttempt, e agent.Event) {
	t := e.Timestamp
	attempt.Session.LastEventAt = &t
	if e.SessionID != "" {
		attempt.Session.SessionID = e.SessionID
	}
	if e.PID != "" {
		attempt.Session.AgentPID = e.PID
	}
	if e.Usage != nil {
		o.state.mu.Lock()
		o.state.Totals.InputTokens += e.Usage.InputTokens
		o.state.Totals.OutputTokens += e.Usage.OutputTokens
		o.state.Totals.TotalTokens += e.Usage.TotalTokens
		o.state.mu.Unlock()
		attempt.Session.InputTokens += e.Usage.InputTokens
		attempt.Session.OutputTokens += e.Usage.OutputTokens
		attempt.Session.TotalTokens += e.Usage.TotalTokens
	}
	slog.Debug("orchestrator.agent_event", "issue_id", issueID, "event", e.Name)
}

// -- reconciliation --

func (o *Orchestrator) reconcile(ctx context.Context) {
	o.state.mu.Lock()
	if len(o.state.Running) == 0 {
		o.state.mu.Unlock()
		return
	}

	o.mu.Lock()
	cfg := o.cfg
	tr := o.tracker
	wsMgr := o.wsMgr
	o.mu.Unlock()

	if cfg == nil || tr == nil {
		o.state.mu.Unlock()
		return
	}

	stallMs := cfg.StallTimeoutMs()
	now := time.Now().UTC()

	var stalledIDs []string
	runningIDs := make([]string, 0, len(o.state.Running))
	for id, attempt := range o.state.Running {
		runningIDs = append(runningIDs, id)
		last := attempt.Session.LastEventAt
		if last == nil {
			last = &attempt.StartedAt
		}
		if int(now.Sub(*last).Milliseconds()) > stallMs {
			slog.Warn("orchestrator.stall_detected", "project", o.name,
				"issue", attempt.Identifier, "elapsed_ms", now.Sub(*last).Milliseconds())
			attempt.Status = StatusStalled
			stalledIDs = append(stalledIDs, id)
		}
	}
	o.state.mu.Unlock()

	for _, id := range stalledIDs {
		o.cancelWorker(id)
	}

	if len(runningIDs) == 0 {
		return
	}

	current, err := tr.FetchIssueStatesByIDs(ctx, runningIDs)
	if err != nil {
		o.state.RecordTrackerFailure(time.Now().UTC(), err)
		slog.Error("orchestrator.reconcile_fetch_failed", "error", err)
		return
	}
	o.state.RecordTrackerSuccess(time.Now().UTC())

	issueMap := make(map[string]*types.Issue, len(current))
	for _, iss := range current {
		issueMap[iss.ID] = iss
	}

	termNorm := normStates(cfg.TerminalStates())

	for _, id := range runningIDs {
		o.state.mu.Lock()
		attempt, stillRunning := o.state.Running[id]
		o.state.mu.Unlock()
		if !stillRunning {
			continue
		}

		cur, found := issueMap[id]
		if !found {
			slog.Warn("orchestrator.reconcile_missing", "issue_id", id)
			o.cancelWorker(id)
			continue
		}

		attempt.IssueState = cur.State

		if termNorm[config.NormalizeState(cur.State)] {
			slog.Info("orchestrator.reconcile_terminal", "issue", cur.Identifier, "state", cur.State)
			o.cancelWorker(id)
			if wsMgr != nil && attempt != nil && attempt.WorkspacePath != "" {
				ws := &workspace.Workspace{
					Path: attempt.WorkspacePath,
					Key:  workspace.SanitizeIdentifier(attempt.Identifier),
				}
				if err := wsMgr.Cleanup(ctx, ws); err != nil {
					slog.Warn("orchestrator.reconcile_cleanup_failed", "issue_id", id, "error", err)
				}
			}
		}
	}
}

func (o *Orchestrator) cancelWorker(issueID string) {
	o.mu.Lock()
	cancel, ok := o.workerCancels[issueID]
	o.mu.Unlock()
	if ok {
		cancel()
	}
}

// -- retry --

func (o *Orchestrator) scheduleRetry(ctx context.Context, cfg *config.SymphonyConfig, issueID, identifier string, attemptNum, failureCount int, errMsg string, abnormal bool) {
	o.state.mu.Lock()
	if existing, ok := o.state.RetryQueue[issueID]; ok && existing.timer != nil {
		existing.timer.Stop()
	}
	o.state.mu.Unlock()

	var delayMs int
	if abnormal {
		max := float64(cfg.MaxRetryBackoffMs())
		exp := failureCount
		if exp < 1 {
			exp = 1
		}
		delayMs = int(math.Min(float64(10_000)*math.Pow(2, float64(exp-1)), max))
	} else {
		delayMs = 1_000
	}

	entry := &RetryEntry{
		IssueID:      issueID,
		Identifier:   identifier,
		Attempt:      attemptNum,
		FailureCount: failureCount,
		DueAt:        time.Now().Add(time.Duration(delayMs) * time.Millisecond),
		Error:        errMsg,
	}
	entry.timer = time.AfterFunc(time.Duration(delayMs)*time.Millisecond, func() {
		o.onRetryTimer(ctx, cfg, issueID)
	})

	o.state.mu.Lock()
	o.state.RetryQueue[issueID] = entry
	o.state.Claimed[issueID] = struct{}{}
	o.state.mu.Unlock()

	slog.Info("orchestrator.retry_scheduled", "project", o.name,
		"issue_id", issueID, "attempt", attemptNum, "delay_ms", delayMs)
}

func (o *Orchestrator) onRetryTimer(ctx context.Context, cfg *config.SymphonyConfig, issueID string) {
	if o.isStopping() {
		o.state.mu.Lock()
		delete(o.state.Claimed, issueID)
		o.state.mu.Unlock()
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
		o.state.mu.Lock()
		delete(o.state.Claimed, issueID)
		o.state.mu.Unlock()
		slog.Info("orchestrator.retry_skipped_during_drain", "project", o.name, "issue_id", issueID)
		return
	}

	o.mu.Lock()
	tr := o.tracker
	o.mu.Unlock()
	if tr == nil {
		return
	}

	candidates, err := tr.FetchCandidateIssues(ctx)
	if err != nil {
		o.state.RecordTrackerFailure(time.Now().UTC(), err)
		slog.Error("orchestrator.retry_fetch_failed", "issue_id", issueID, "error", err)
		o.scheduleRetry(ctx, cfg, issueID, entry.Identifier, entry.Attempt, entry.FailureCount, "poll failed", true)
		return
	}
	o.state.RecordTrackerSuccess(time.Now().UTC())

	var issue *types.Issue
	for _, c := range candidates {
		if c.ID == issueID {
			issue = c
			break
		}
	}

	if issue == nil {
		slog.Info("orchestrator.retry_gone", "issue_id", issueID)
		o.state.mu.Lock()
		delete(o.state.Claimed, issueID)
		o.state.mu.Unlock()
		return
	}

	activeNorm := normStates(cfg.ActiveStates())
	if !activeNorm[config.NormalizeState(issue.State)] {
		slog.Info("orchestrator.retry_inactive", "issue_id", issueID, "state", issue.State)
		o.state.mu.Lock()
		delete(o.state.Claimed, issueID)
		o.state.mu.Unlock()
		return
	}

	o.state.mu.Lock()
	slots := o.state.MaxConcurrentAgents - len(o.state.Running)
	o.state.mu.Unlock()

	if slots <= 0 || !o.hasGlobalCapacity() {
		o.scheduleRetry(ctx, cfg, issueID, entry.Identifier, entry.Attempt, entry.FailureCount, "no slots", true)
		return
	}

	o.state.mu.Lock()
	delete(o.state.Claimed, issueID)
	o.state.mu.Unlock()

	if !o.dispatch(ctx, cfg, issue, entry.Attempt, entry.FailureCount) {
		o.scheduleRetry(ctx, cfg, issueID, entry.Identifier, entry.Attempt, entry.FailureCount, "no global slots", true)
	}
}

// -- helpers --

func (o *Orchestrator) isStillActive(ctx context.Context, cfg *config.SymphonyConfig, issueID string) (bool, error) {
	o.mu.Lock()
	tr := o.tracker
	o.mu.Unlock()
	if tr == nil {
		return true, nil
	}
	issues, err := tr.FetchIssueStatesByIDs(ctx, []string{issueID})
	if err != nil {
		return true, err
	}
	if len(issues) == 0 {
		return false, nil
	}
	return normStates(cfg.ActiveStates())[config.NormalizeState(issues[0].State)], nil
}

func (o *Orchestrator) startupCleanup(ctx context.Context) {
	o.mu.Lock()
	cfg := o.cfg
	tr := o.tracker
	wsMgr := o.wsMgr
	o.mu.Unlock()

	if cfg == nil || tr == nil || wsMgr == nil {
		return
	}

	issues, err := tr.FetchIssuesByStates(ctx, cfg.TerminalStates())
	if err != nil {
		slog.Warn("orchestrator.startup_cleanup_failed", "error", err)
		return
	}

	root := cfg.WorkspaceRoot()
	for _, iss := range issues {
		key := workspace.SanitizeIdentifier(iss.Identifier)
		wsPath := filepath.Join(root, key)
		if _, err := os.Stat(wsPath); os.IsNotExist(err) {
			continue
		}
		slog.Info("orchestrator.startup_cleanup", "issue", iss.Identifier)
		ws := &workspace.Workspace{Path: wsPath, Key: key}
		if err := wsMgr.Cleanup(ctx, ws); err != nil {
			slog.Warn("orchestrator.startup_cleanup_error", "issue", iss.Identifier, "error", err)
		}
	}
}

func (o *Orchestrator) watchWorkflow(ctx context.Context) {
	path := o.workflowPath
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	lastMod := info.ModTime()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-o.stopCh:
			return
		case <-ticker.C:
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			if info.ModTime().After(lastMod) {
				lastMod = info.ModTime()
				if err := o.reloadWorkflow(); err != nil {
					slog.Error("orchestrator.workflow_reload_failed", "error", err)
				} else {
					slog.Info("orchestrator.workflow_reloaded", "project", o.name)
				}
			}
		}
	}
}

func (o *Orchestrator) reloadWorkflow() error {
	wf, err := workflow.Load(o.workflowPath)
	if err != nil {
		return err
	}
	cfg := config.New(wf.Config)
	o.mu.Lock()
	o.wf = wf
	o.cfg = cfg
	o.mu.Unlock()
	o.state.mu.Lock()
	o.state.PollIntervalMs = cfg.PollIntervalMs()
	o.state.PollIntervalIdleMs = cfg.PollIntervalIdleMs()
	o.state.MaxConcurrentAgents = cfg.MaxConcurrentAgents()
	o.state.mu.Unlock()
	return nil
}

func normStates(states []string) map[string]bool {
	m := make(map[string]bool, len(states))
	for _, s := range states {
		m[config.NormalizeState(s)] = true
	}
	return m
}

func isCtxErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "context canceled") || strings.Contains(s, "context deadline exceeded")
}

func (o *Orchestrator) hasGlobalCapacity() bool {
	return o.globalLimiter == nil || o.globalLimiter.Available() > 0
}

func (o *Orchestrator) isStopping() bool {
	select {
	case <-o.stopCh:
		return true
	default:
		return false
	}
}

func (o *Orchestrator) releaseGlobalSlot(attempt *RunAttempt) {
	if attempt == nil || !attempt.GlobalSlotHeld || o.globalLimiter == nil {
		return
	}
	o.globalLimiter.Release()
	attempt.GlobalSlotHeld = false
}

func (o *Orchestrator) shouldAbandonRetry(cfg *config.SymphonyConfig, failureCount int) bool {
	maxAttempts := cfg.MaxRetryAttempts()
	return maxAttempts > 0 && failureCount > maxAttempts
}

func (o *Orchestrator) abandonRetry(ctx context.Context, cfg *config.SymphonyConfig, issueID string, attempt *RunAttempt, errMsg string) {
	if attempt == nil {
		return
	}

	failureCount := attempt.FailureCount + 1

	o.state.mu.Lock()
	delete(o.state.Claimed, issueID)
	delete(o.state.RetryQueue, issueID)
	o.state.Abandoned[issueID] = &AbandonedEntry{
		Identifier:   attempt.Identifier,
		State:        attempt.IssueState,
		FailureCount: failureCount,
		Error:        errMsg,
		AbandonedAt:  time.Now().UTC(),
	}
	o.state.mu.Unlock()

	slog.Warn("orchestrator.retry_abandoned", "project", o.name,
		"issue_id", issueID, "attempt", attempt.Attempt, "failure_count", failureCount)

	o.mu.Lock()
	tr := o.tracker
	o.mu.Unlock()
	if tr == nil {
		return
	}
	if err := tr.CreateIssueComment(ctx, issueID, buildRetryAbandonComment(attempt.Identifier, cfg.MaxRetryAttempts(), failureCount, errMsg)); err != nil {
		slog.Warn("orchestrator.retry_abandon_comment_failed", "issue_id", issueID, "error", err)
	}
}

func buildRetryAbandonComment(identifier string, maxAttempts, failureCount int, errMsg string) string {
	body := fmt.Sprintf(
		"%s\nSymphony가 %s 이슈의 재시도를 중단했습니다.\n\n`agent.max_retry_attempts=%d`를 초과해 연속 실패가 누적되었습니다.\n\n연속 실패 횟수: %d회.",
		retryAbandonCommentMarker,
		identifier, maxAttempts, failureCount,
	)
	if trimmed := strings.TrimSpace(errMsg); trimmed != "" {
		body += fmt.Sprintf("\n\n마지막 오류:\n```text\n%s\n```", truncateForComment(trimmed, 2_000))
	}
	return body
}

func truncateForComment(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

func issueUpdatedAfter(at time.Time, updatedAt *time.Time) bool {
	return updatedAt != nil && updatedAt.After(at)
}

func isRetryAbandonComment(body string) bool {
	return strings.Contains(body, retryAbandonCommentMarker)
}

func (o *Orchestrator) isDraining() bool {
	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	return o.state.Draining
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
