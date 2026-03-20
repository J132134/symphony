// Package orchestrator is the main poll-dispatch-reconcile loop for one project.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"symphony/internal/agent"
	"symphony/internal/config"
	"symphony/internal/filewatch"
	"symphony/internal/tracker"
	"symphony/internal/workflow"
	"symphony/internal/workspace"
)

const urgentPriority = 1
const capacityRetryDelayMs = 5_000

const (
	rateLimitPauseInitialBackoff = 30 * time.Second
	rateLimitPauseMaxBackoff     = 5 * time.Minute
)

// Orchestrator drives one project: polls Linear, dispatches agents, reconciles.
type Orchestrator struct {
	workflowBasePath string
	workflowPath     string
	name             string
	globalLimiter    *SessionLimiter

	mu      sync.Mutex
	cfg     *config.SymphonyConfig
	wf      *workflow.Definition
	tracker *tracker.LinearClient
	wsMgr   *workspace.Manager

	state *State

	workers sync.WaitGroup

	webhookMode bool
	refreshCh   chan struct{}

	// workersByIssue maps issue_id → running worker handle.
	workersByIssue map[string]*workerHandle

	workflowWatchDebounce time.Duration
	after                 func(time.Duration) <-chan time.Time
	tickFn                func(context.Context)
	stopCh                chan struct{}
	doneCh                chan struct{}
}

type workerHandle struct {
	mu          sync.Mutex
	cancel      context.CancelFunc
	attempt     *RunAttempt
	runner      agent.Runner
	agentCfg    *agent.Config
	drainTimer  *time.Timer
	draining    bool
	interrupted bool
}

// New creates an Orchestrator. Call Run to start it.
func New(workflowPath string, port int, name string, globalLimiter *SessionLimiter) *Orchestrator {
	return NewWithBase("", workflowPath, port, name, globalLimiter)
}

// NewWithBase creates an Orchestrator with an optional shared workflow base.
func NewWithBase(workflowBasePath, workflowPath string, port int, name string, globalLimiter *SessionLimiter) *Orchestrator {
	return &Orchestrator{
		workflowBasePath:      workflowBasePath,
		workflowPath:          workflowPath,
		name:                  name,
		globalLimiter:         globalLimiter,
		state:                 NewState(),
		refreshCh:             make(chan struct{}, 1),
		workersByIssue:        make(map[string]*workerHandle),
		workflowWatchDebounce: filewatch.DefaultDebounce,
		after:                 time.After,
		stopCh:                make(chan struct{}),
		doneCh:                make(chan struct{}),
	}
}

func (h *workerHandle) setRunner(runner agent.Runner, cfg *agent.Config) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runner = runner
	h.agentCfg = cfg
}

func (h *workerHandle) clearRunner() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runner = nil
	h.agentCfg = nil
}

func (h *workerHandle) requestDrain(timeout time.Duration) {
	h.mu.Lock()
	if h.draining {
		h.mu.Unlock()
		return
	}
	h.draining = true
	deadline := time.Now().Add(timeout)
	h.attempt.SetDrainDeadline(deadline)
	if timeout > 0 {
		h.drainTimer = time.AfterFunc(timeout, func() {
			h.attempt.SetCancelReason(CancelReasonDrain)
			h.cancel()
		})
	}
	runner := h.runner
	agentCfg := h.agentCfg
	threadID, turnID := h.attempt.ActiveTurn()
	h.mu.Unlock()

	if runner == nil || agentCfg == nil {
		return
	}
	if strings.TrimSpace(threadID) == "" || strings.TrimSpace(turnID) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(agentCfg.ReadTimeoutMs)*time.Millisecond)
	defer cancel()
	if err := runner.InterruptTurn(ctx, threadID, turnID, agentCfg); err != nil {
		slog.Warn("orchestrator.turn_interrupt_failed", "issue", h.attempt.Identifier, "error", err)
	}
}

func (h *workerHandle) stopDrainTimer() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.drainTimer != nil {
		h.drainTimer.Stop()
		h.drainTimer = nil
	}
}

func (o *Orchestrator) drainTimeoutFor(cfg *config.SymphonyConfig) time.Duration {
	if cfg == nil {
		return 6 * time.Minute
	}
	return time.Duration(cfg.DrainTimeoutMs()) * time.Millisecond
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
		cfg.TrackerAssignee(),
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
	o.state.PollWebhookFallbackIntervalMs = cfg.PollWebhookFallbackIntervalMs()
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

func (o *Orchestrator) TriggerRefresh(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case o.refreshCh <- struct{}{}:
	default:
	}
}

func (o *Orchestrator) SetWebhookMode(enabled bool) {
	o.mu.Lock()
	o.webhookMode = enabled
	o.mu.Unlock()
}

func (o *Orchestrator) currentConfig() *config.SymphonyConfig {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cfg
}

func (o *Orchestrator) workerHandlesSnapshot() []*workerHandle {
	o.mu.Lock()
	defer o.mu.Unlock()
	handles := make([]*workerHandle, 0, len(o.workersByIssue))
	for _, handle := range o.workersByIssue {
		handles = append(handles, handle)
	}
	return handles
}

func isPauseState(cfg *config.SymphonyConfig, state string) bool {
	if cfg == nil {
		return false
	}
	return cfg.PauseNorm()[config.NormalizeState(state)]
}

func (o *Orchestrator) runTick(ctx context.Context) {
	if o.tickFn != nil {
		o.tickFn(ctx)
		return
	}
	o.tick(ctx)
}

func (o *Orchestrator) computeInterval() int {
	o.mu.Lock()
	webhookMode := o.webhookMode
	o.mu.Unlock()

	o.state.mu.Lock()
	defer o.state.mu.Unlock()

	if webhookMode {
		return o.state.PollWebhookFallbackIntervalMs
	}

	active := len(o.state.Running) > 0 || len(o.state.RetryQueue) > 0
	if active {
		return o.state.PollIntervalMs
	}
	return o.state.PollIntervalIdleMs
}

// -- poll loop --

func (o *Orchestrator) pollLoop(ctx context.Context) error {
	o.runTick(ctx)

	for {
		intervalMs := o.computeInterval()

		select {
		case <-o.stopCh:
			return o.gracefulStop(o.isDraining())
		case <-ctx.Done():
			return o.gracefulStop(false)
		case <-o.refreshCh:
		case <-o.after(time.Duration(intervalMs) * time.Millisecond):
		}
		o.runTick(ctx)
	}
}

func (o *Orchestrator) gracefulStop(drain bool) error {
	slog.Info("orchestrator.stopping", "project", o.name)

	o.state.mu.Lock()
	o.clearRetryQueueLocked(true)
	o.state.mu.Unlock()

	handles := o.workerHandlesSnapshot()
	if drain {
		timeout := o.drainTimeoutFor(o.currentConfig())
		for _, handle := range handles {
			handle.requestDrain(timeout)
		}
	} else {
		for _, handle := range handles {
			handle.attempt.SetCancelReason(CancelReasonShutdown)
			handle.cancel()
		}
	}

	o.workers.Wait()

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
	if _, _, paused := o.admissionPauseState(time.Now().UTC()); paused {
		return
	}

	o.mu.Lock()
	cfg := o.cfg
	tr := o.tracker
	o.mu.Unlock()

	if cfg == nil || tr == nil {
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
		if _, _, paused := o.admissionPauseState(time.Now().UTC()); paused {
			return
		}

		if !isUrgentIssue(issue) {
			o.state.mu.Lock()
			slots := o.state.MaxConcurrentAgents - o.runningConcurrentCountLocked(cfg)
			urgentRunning := o.hasRunningUrgentLocked(cfg)
			o.state.mu.Unlock()

			if urgentRunning || slots <= 0 {
				break
			}
			if !o.hasGlobalCapacityForIssue(cfg, issue) {
				break
			}
		}

		if o.canDispatch(cfg, issue) {
			if !o.dispatch(ctx, cfg, issue, 1, 0, false) && !isUrgentIssue(issue) {
				break
			}
		}
	}
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
	paths := []string{o.workflowPath}
	if strings.TrimSpace(o.workflowBasePath) != "" && o.workflowBasePath != o.workflowPath {
		paths = append(paths, o.workflowBasePath)
	}
	for _, path := range paths {
		go o.watchWorkflowPath(ctx, path)
	}
}

func (o *Orchestrator) watchWorkflowPath(ctx context.Context, path string) {
	if err := filewatch.Run(ctx, o.stopCh, path, o.workflowWatchDebounce, filewatch.Callbacks{
		Reload: o.reloadWorkflow,
		OnReloaded: func() {
			slog.Info("orchestrator.workflow_reloaded", "project", o.name, "path", path)
		},
		OnReloadError: func(err error) {
			slog.Error("orchestrator.workflow_reload_failed", "project", o.name, "path", path, "error", err)
		},
		OnWatchError: func(err error) {
			slog.Error("orchestrator.workflow_watch_failed", "project", o.name, "path", path, "error", err)
		},
	}); err != nil && ctx.Err() == nil && !o.isStopping() {
		slog.Error("orchestrator.workflow_watch_failed", "project", o.name, "path", path, "error", err)
	}
}

func (o *Orchestrator) reloadWorkflow() error {
	wf, err := workflow.LoadMerged(o.workflowBasePath, o.workflowPath)
	if err != nil {
		return err
	}
	cfg := config.New(wf.Config)
	if errs := cfg.Validate(); len(errs) > 0 {
		return fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}

	o.mu.Lock()
	o.wf = wf
	o.cfg = cfg
	o.mu.Unlock()
	o.state.mu.Lock()
	o.state.PollIntervalMs = cfg.PollIntervalMs()
	o.state.PollIntervalIdleMs = cfg.PollIntervalIdleMs()
	o.state.PollWebhookFallbackIntervalMs = cfg.PollWebhookFallbackIntervalMs()
	o.state.MaxConcurrentAgents = cfg.MaxConcurrentAgents()
	o.state.mu.Unlock()
	return nil
}
