// Package daemon manages multiple orchestrators as a single long-running process.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"symphony/internal/config"
	"symphony/internal/orchestrator"
	"symphony/internal/status"
	"symphony/internal/tracker"
	"symphony/internal/workflow"
)

type managedOrchestrator interface {
	Run(context.Context) error
	BeginDrain()
	DrainAndStop()
	IsIdle() bool
	GetState() *orchestrator.State
	SetWebhookMode(bool)
	TriggerRefresh(context.Context)
}

type runnerHealthState string

const (
	runnerHealthHealthy     runnerHealthState = "healthy"
	runnerHealthProbing     runnerHealthState = "probing"
	runnerHealthQuarantined runnerHealthState = "quarantined"
)

type projectRunnerSnapshot struct {
	State         *orchestrator.State
	LastErr       string
	Health        string
	CrashCount    int
	QuarantinedAt string
}

// projectRunner manages the lifecycle of one Orchestrator with auto-restart.
type projectRunner struct {
	proj           config.ProjectConfig
	limiter        *orchestrator.SessionLimiter
	healthCfg      config.ProjectHealthConfig
	now            func() time.Time
	after          func(time.Duration) <-chan time.Time
	newOrch        func(config.ProjectConfig, *orchestrator.SessionLimiter) managedOrchestrator
	probe          func(context.Context, config.ProjectConfig) error
	webhookEnabled bool

	mu            sync.Mutex
	orch          managedOrchestrator
	cancel        context.CancelFunc
	done          chan struct{}
	stopping      bool
	draining      bool
	lastErr       string
	healthState   runnerHealthState
	crashTimes    []time.Time
	quarantinedAt *time.Time
}

func newProjectRunner(proj config.ProjectConfig, limiter *orchestrator.SessionLimiter, healthCfg config.ProjectHealthConfig, webhookEnabled bool) *projectRunner {
	return &projectRunner{
		proj:           proj,
		limiter:        limiter,
		healthCfg:      healthCfg,
		now:            time.Now,
		after:          time.After,
		newOrch:        newManagedOrchestrator,
		probe:          probeProjectHealth,
		webhookEnabled: webhookEnabled,
		healthState:    runnerHealthHealthy,
	}
}

func (pr *projectRunner) start(parent context.Context) {
	if parent != nil && parent.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	pr.mu.Lock()
	pr.cancel = cancel
	pr.done = done
	pr.stopping = false
	pr.mu.Unlock()

	go func() {
		defer close(done)
		pr.run(ctx)
	}()
}

func (pr *projectRunner) run(ctx context.Context) {
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		if pr.isStopping() {
			return
		}

		if err := pr.waitUntilHealthy(ctx); err != nil {
			return
		}

		o := pr.newOrch(pr.proj, pr.limiter)
		if pr.webhookEnabled {
			o.SetWebhookMode(true)
		}

		pr.mu.Lock()
		pr.orch = o
		pr.lastErr = ""
		draining := pr.draining
		pr.mu.Unlock()

		if draining {
			o.BeginDrain()
		}

		slog.Info("daemon.project_starting", "project", pr.proj.Name)
		err := o.Run(ctx)

		pr.mu.Lock()
		pr.orch = nil
		if err != nil && ctx.Err() == nil {
			pr.lastErr = err.Error()
		}
		draining = pr.draining
		pr.mu.Unlock()

		if ctx.Err() != nil || pr.isStopping() || draining {
			return
		}

		if err != nil {
			crashCount, quarantined := pr.recordCrash(err)
			if quarantined {
				slog.Error("daemon.project_quarantined",
					"project", pr.proj.Name,
					"error", err,
					"crash_count", crashCount,
					"restart_budget_count", pr.healthCfg.RestartBudgetCount,
				)
				backoff = 5 * time.Second
				continue
			}
			slog.Error("daemon.project_crashed",
				"project", pr.proj.Name,
				"error", err,
				"retry_in", backoff,
				"crash_count", crashCount,
			)
			select {
			case <-ctx.Done():
				return
			case <-pr.after(backoff):
			}
			if backoff < 5*time.Minute {
				backoff *= 2
			}
		} else {
			pr.markHealthy()
			slog.Info("daemon.project_stopped", "project", pr.proj.Name)
			backoff = 5 * time.Second
		}
	}
}

func (pr *projectRunner) beginDrain() {
	pr.mu.Lock()
	pr.draining = true
	o := pr.orch
	pr.mu.Unlock()
	if o != nil {
		o.BeginDrain()
	}
}

func (pr *projectRunner) stop() {
	pr.mu.Lock()
	cancel := pr.cancel
	o := pr.orch
	done := pr.done
	pr.stopping = true
	pr.draining = true
	pr.cancel = nil
	pr.done = nil
	pr.mu.Unlock()

	if o != nil {
		o.DrainAndStop()
	}
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (pr *projectRunner) isIdle() bool {
	pr.mu.Lock()
	o := pr.orch
	pr.mu.Unlock()
	if o == nil {
		return true
	}
	return o.IsIdle()
}

func (pr *projectRunner) getState() *orchestrator.State {
	pr.mu.Lock()
	o := pr.orch
	pr.mu.Unlock()
	if o == nil {
		return nil
	}
	return o.GetState()
}

func (pr *projectRunner) snapshot() (*orchestrator.State, string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.orch == nil {
		return nil, pr.lastErr
	}
	return pr.orch.GetState(), pr.lastErr
}

func (pr *projectRunner) projectSnapshot() projectRunnerSnapshot {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	nowFn := pr.now
	if nowFn == nil {
		nowFn = time.Now
	}
	pr.trimCrashTimesLocked(nowFn().UTC())

	snapshot := projectRunnerSnapshot{
		LastErr:    pr.lastErr,
		Health:     pr.healthStringLocked(),
		CrashCount: len(pr.crashTimes),
	}
	if pr.orch != nil {
		snapshot.State = pr.orch.GetState()
	}
	if pr.quarantinedAt != nil {
		snapshot.QuarantinedAt = pr.quarantinedAt.Format(time.RFC3339)
	}
	return snapshot
}

func (pr *projectRunner) isStopping() bool {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.stopping
}

func (pr *projectRunner) waitUntilHealthy(ctx context.Context) error {
	for ctx.Err() == nil {
		pr.mu.Lock()
		health := pr.healthState
		pr.mu.Unlock()
		if health != runnerHealthQuarantined && health != runnerHealthProbing {
			return nil
		}

		pr.mu.Lock()
		pr.healthState = runnerHealthProbing
		pr.mu.Unlock()

		if err := pr.probe(ctx, pr.proj); err == nil {
			pr.clearQuarantine()
			slog.Info("daemon.project_recovered", "project", pr.proj.Name)
			return nil
		} else {
			pr.mu.Lock()
			pr.healthState = runnerHealthQuarantined
			pr.lastErr = err.Error()
			pr.mu.Unlock()
			slog.Warn("daemon.project_quarantine_probe_failed", "project", pr.proj.Name, "error", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-pr.after(time.Duration(pr.healthCfg.ProbeIntervalSeconds) * time.Second):
		}
	}
	return ctx.Err()
}

func (pr *projectRunner) recordCrash(err error) (int, bool) {
	now := pr.now().UTC()

	pr.mu.Lock()
	defer pr.mu.Unlock()

	pr.trimCrashTimesLocked(now)
	pr.crashTimes = append(pr.crashTimes, now)
	pr.lastErr = err.Error()

	if len(pr.crashTimes) >= pr.healthCfg.RestartBudgetCount {
		pr.healthState = runnerHealthQuarantined
		pr.quarantinedAt = &now
		return len(pr.crashTimes), true
	}

	pr.healthState = runnerHealthHealthy
	return len(pr.crashTimes), false
}

func (pr *projectRunner) markHealthy() {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.healthState != runnerHealthQuarantined {
		pr.healthState = runnerHealthHealthy
	}
}

func (pr *projectRunner) clearQuarantine() {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.healthState = runnerHealthHealthy
	pr.crashTimes = nil
	pr.quarantinedAt = nil
	pr.lastErr = ""
}

func (pr *projectRunner) trimCrashTimesLocked(now time.Time) {
	if len(pr.crashTimes) == 0 {
		return
	}

	windowMinutes := pr.healthCfg.RestartBudgetWindowMinutes
	if windowMinutes <= 0 {
		windowMinutes = 15
	}
	cutoff := now.Add(-time.Duration(windowMinutes) * time.Minute)
	keep := pr.crashTimes[:0]
	for _, crashAt := range pr.crashTimes {
		if !crashAt.Before(cutoff) {
			keep = append(keep, crashAt)
		}
	}
	pr.crashTimes = keep
}

func (pr *projectRunner) healthStringLocked() string {
	if pr.healthState == "" {
		return string(runnerHealthHealthy)
	}
	return string(pr.healthState)
}

func newManagedOrchestrator(proj config.ProjectConfig, limiter *orchestrator.SessionLimiter) managedOrchestrator {
	return orchestrator.NewWithBase(proj.WorkflowBase, proj.Workflow, 0, proj.Name, limiter)
}

func probeProjectHealth(ctx context.Context, proj config.ProjectConfig) error {
	wf, err := workflow.LoadMerged(proj.WorkflowBase, proj.Workflow)
	if err != nil {
		return fmt.Errorf("load workflow: %w", err)
	}

	cfg := config.New(wf.Config)
	if errs := cfg.Validate(); len(errs) > 0 {
		return fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}

	tr, err := tracker.NewLinearClient(
		cfg.TrackerAPIKey(),
		cfg.TrackerEndpoint(),
		cfg.TrackerProjectSlug(),
		cfg.ActiveStates(),
		cfg.TrackerAssignee(),
	)
	if err != nil {
		return fmt.Errorf("tracker: %w", err)
	}
	if err := tr.Ping(ctx); err != nil {
		return fmt.Errorf("tracker ping: %w", err)
	}
	return nil
}

// Manager coordinates multiple Orchestrators.
type Manager struct {
	mu      sync.RWMutex
	cfg     *config.DaemonConfig
	runners map[string]*projectRunner
	ctx     context.Context
	cancel  context.CancelFunc
	limiter *orchestrator.SessionLimiter

	restartRequested bool
	restartReady     chan struct{}
	done             chan struct{}
}

func NewManager(cfg *config.DaemonConfig) *Manager {
	return NewManagerWithLimiter(cfg, nil)
}

func NewManagerWithLimiter(cfg *config.DaemonConfig, limiter *orchestrator.SessionLimiter) *Manager {
	if limiter == nil {
		limiter = orchestrator.NewSessionLimiter(cfg.MaxTotalConcurrentSessions())
	}
	return &Manager{
		cfg:     cfg,
		runners: make(map[string]*projectRunner, len(cfg.Projects)),
		limiter: limiter,
	}
}

// Run starts all projects and blocks until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	defer close(done)

	m.mu.Lock()
	m.ctx = ctx
	m.cancel = cancel
	m.done = done
	cfg := m.cfg
	m.mu.Unlock()

	m.ApplyConfig(cfg)

	slog.Info("daemon.started", "projects", len(cfg.Projects), "max_total_concurrent_sessions", cfg.MaxTotalConcurrentSessions())
	<-ctx.Done()

	slog.Info("daemon.shutting_down")
	for _, pr := range m.runnersSnapshot() {
		pr.stop()
	}

	m.mu.Lock()
	m.ctx = nil
	m.cancel = nil
	m.done = nil
	m.mu.Unlock()
	slog.Info("daemon.stopped")
}

// Shutdown triggers a graceful stop (for auto-update).
func (m *Manager) Shutdown() {
	m.mu.RLock()
	cancel := m.cancel
	m.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
}

func (m *Manager) Wait() {
	m.mu.RLock()
	done := m.done
	m.mu.RUnlock()
	if done != nil {
		<-done
	}
}

// ApplyConfig incrementally reconciles the managed runners with the latest config.
func (m *Manager) ApplyConfig(cfg *config.DaemonConfig) {
	if cfg == nil {
		return
	}
	if m.limiter != nil {
		m.limiter.SetLimit(cfg.MaxTotalConcurrentSessions())
	}

	nextProjects := make(map[string]config.ProjectConfig, len(cfg.Projects))
	for _, proj := range cfg.Projects {
		nextProjects[proj.Name] = proj
	}

	var toStart []*projectRunner
	var toStop []*projectRunner

	m.mu.Lock()
	if m.runners == nil {
		m.runners = make(map[string]*projectRunner, len(cfg.Projects))
	}
	runCtx := m.ctx
	restartRequested := m.restartRequested

	for name, runner := range m.runners {
		if _, ok := nextProjects[name]; ok {
			continue
		}
		delete(m.runners, name)
		toStop = append(toStop, runner)
	}

	for _, proj := range cfg.Projects {
		runner, ok := m.runners[proj.Name]
		if ok && projectConfigEqual(runner.proj, proj) {
			continue
		}
		if ok {
			delete(m.runners, proj.Name)
			toStop = append(toStop, runner)
		}

		nextRunner := newProjectRunner(proj, m.limiter, cfg.ProjectHealth, cfg.Webhook.Enabled)
		if restartRequested {
			nextRunner.beginDrain()
		}
		m.runners[proj.Name] = nextRunner
		if runCtx != nil {
			toStart = append(toStart, nextRunner)
		}
	}

	m.cfg = cfg
	m.mu.Unlock()

	for _, runner := range toStop {
		runner.stop()
	}
	for _, runner := range toStart {
		runner.start(runCtx)
	}
}

func (m *Manager) RequestRestartWhenIdle() <-chan struct{} {
	m.mu.Lock()
	if m.restartRequested {
		ch := m.restartReady
		m.mu.Unlock()
		return ch
	}

	m.restartRequested = true
	m.restartReady = make(chan struct{})

	runners := make([]*projectRunner, 0, len(m.runners))
	for _, runner := range m.runners {
		runners = append(runners, runner)
	}
	ch := m.restartReady
	m.mu.Unlock()

	for _, pr := range runners {
		pr.beginDrain()
	}

	go m.waitForIdle(ch)
	return ch
}

// TriggerRefresh asks each running orchestrator to poll immediately.
func (m *Manager) TriggerRefresh(ctx context.Context) {
	for _, pr := range m.runnersSnapshot() {
		pr.mu.Lock()
		orch := pr.orch
		pr.mu.Unlock()
		if orch != nil {
			orch.TriggerRefresh(ctx)
		}
	}
}

// GetAllStates returns state for all running projects.
func (m *Manager) GetAllStates() map[string]*orchestrator.State {
	runners := m.runnersSnapshot()
	result := make(map[string]*orchestrator.State, len(runners))
	for _, pr := range runners {
		if st := pr.getState(); st != nil {
			result[pr.proj.Name] = st
		}
	}
	return result
}

func (m *Manager) GetProjects() []status.ProjectSummary {
	runners := m.runnersSnapshot()
	projects := make([]status.ProjectSummary, 0, len(runners))

	for _, pr := range runners {
		snapshot := pr.projectSnapshot()
		project := status.ProjectSummary{
			Name:             pr.proj.Name,
			Status:           "idle",
			Health:           snapshot.Health,
			CrashCount:       snapshot.CrashCount,
			QuarantinedAt:    snapshot.QuarantinedAt,
			TrackerConnected: true,
			LastError:        snapshot.LastErr,
		}

		if snapshot.State == nil {
			if project.Health == "quarantined" || project.Health == "probing" {
				project.Status = "quarantined"
			} else if snapshot.LastErr != "" {
				project.Status = "error"
			}
			projects = append(projects, project)
			continue
		}

		st := snapshot.State
		st.Lock()
		project.SubprocessCount = len(st.Running)
		project.RetryCount = len(st.RetryQueue)
		project.InputTokens = st.Totals.InputTokens
		project.OutputTokens = st.Totals.OutputTokens
		project.TotalTokens = st.Totals.TotalTokens
		for _, attempt := range st.Running {
			project.RunningIssueIDs = append(project.RunningIssueIDs, attempt.Identifier)
			project.RunningIssues = append(project.RunningIssues, status.SummarizeRunningIssue(attempt))
		}
		sort.Strings(project.RunningIssueIDs)
		sort.Slice(project.RunningIssues, func(i, j int) bool {
			return project.RunningIssues[i].Identifier < project.RunningIssues[j].Identifier
		})
		failureRetryCount := 0
		for _, entry := range st.RetryQueue {
			project.RetryEntries = append(project.RetryEntries, status.RetrySummary{
				Identifier: entry.Identifier,
				Kind:       string(entry.Kind),
				DueAt:      entry.DueAt.UTC().Format(time.RFC3339),
				Error:      entry.Error,
			})
			if entry.Kind == orchestrator.RetryKindFailure {
				failureRetryCount++
			}
		}
		sort.Slice(project.RetryEntries, func(i, j int) bool {
			if project.RetryEntries[i].DueAt == project.RetryEntries[j].DueAt {
				return project.RetryEntries[i].Identifier < project.RetryEntries[j].Identifier
			}
			return project.RetryEntries[i].DueAt < project.RetryEntries[j].DueAt
		})
		project.TrackerConnected, project.LastTrackerSuccess, project.LastTrackerError = st.TrackerStatusLocked()
		if st.PausedUntil != nil {
			project.PausedUntil = st.PausedUntil.UTC().Format(time.RFC3339)
			project.PauseReason = st.PauseReason
		}
		if project.RetryCount > 0 && project.LastError == "" {
			for _, retry := range st.RetryQueue {
				if retry.Error != "" {
					project.LastError = retry.Error
					break
				}
			}
		}
		st.Unlock()

		switch {
		case project.Health == "quarantined" || project.Health == "probing":
			project.Status = "quarantined"
		case !project.TrackerConnected:
			project.Status = "network_lost"
		case failureRetryCount > 0 || snapshot.LastErr != "":
			project.Status = "error"
		case project.SubprocessCount > 0:
			project.Status = "running"
		}

		projects = append(projects, project)
	}

	sort.Slice(projects, func(i, j int) bool {
		return projects[i].Name < projects[j].Name
	})
	return projects
}

func (m *Manager) waitForIdle(ready chan struct{}) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		if m.allIdle() {
			close(ready)
			return
		}
		<-ticker.C
	}
}

func (m *Manager) allIdle() bool {
	for _, pr := range m.runnersSnapshot() {
		if !pr.isIdle() {
			return false
		}
	}
	return true
}

func (m *Manager) GetSummary() status.Summary {
	return status.BuildSummaryFromProjects(m.GetProjects())
}

func (m *Manager) runnersSnapshot() []*projectRunner {
	m.mu.RLock()
	defer m.mu.RUnlock()

	runners := make([]*projectRunner, 0, len(m.runners))
	for _, runner := range m.runners {
		runners = append(runners, runner)
	}
	return runners
}

func projectConfigEqual(a, b config.ProjectConfig) bool {
	return a.Name == b.Name && a.WorkflowBase == b.WorkflowBase && a.Workflow == b.Workflow
}
