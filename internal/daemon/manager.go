// Package daemon manages multiple orchestrators as a single long-running process.
package daemon

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"symphony/internal/config"
	"symphony/internal/orchestrator"
	"symphony/internal/status"
	"symphony/internal/version"
)

// projectRunner manages the lifecycle of one Orchestrator with auto-restart.
type projectRunner struct {
	proj    config.ProjectConfig
	limiter *orchestrator.SessionLimiter

	mu       sync.Mutex
	orch     *orchestrator.Orchestrator
	cancel   context.CancelFunc
	done     chan struct{}
	stopping bool
	draining bool
	lastErr  string
}

func newProjectRunner(proj config.ProjectConfig, limiter *orchestrator.SessionLimiter) *projectRunner {
	return &projectRunner{
		proj:    proj,
		limiter: limiter,
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

		o := orchestrator.New(pr.proj.Workflow, 0, pr.proj.Name, pr.limiter)

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
			slog.Error("daemon.project_crashed", "project", pr.proj.Name, "error", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 5*time.Minute {
				backoff *= 2
			}
		} else {
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

func (pr *projectRunner) isStopping() bool {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.stopping
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

		nextRunner := newProjectRunner(proj, m.limiter)
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
	runners := m.runnersSnapshot()
	build := version.Current()
	summary := status.Summary{
		Status:    "idle",
		Version:   build.Version,
		GitHash:   build.GitHash,
		Dirty:     build.Dirty,
		Projects:  make([]status.ProjectSummary, 0, len(runners)),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	projectNames := make([]string, 0, len(runners))
	projectMap := make(map[string]status.ProjectSummary, len(runners))
	var hasError bool
	var hasNetworkIssue bool

	for _, pr := range runners {
		st, lastErr := pr.snapshot()
		project := status.ProjectSummary{
			Name:             pr.proj.Name,
			Status:           "idle",
			TrackerConnected: true,
		}
		projectNames = append(projectNames, pr.proj.Name)

		if st == nil {
			if lastErr != "" {
				project.Status = "error"
				project.LastError = lastErr
				hasError = true
			}
			projectMap[pr.proj.Name] = project
			continue
		}

		st.Lock()
		project.SubprocessCount = len(st.Running)
		project.RetryCount = len(st.RetryQueue)
		for _, attempt := range st.Running {
			project.RunningIssueIDs = append(project.RunningIssueIDs, attempt.Identifier)
			summary.RunningIssueIDs = append(summary.RunningIssueIDs, attempt.Identifier)
		}
		sort.Strings(project.RunningIssueIDs)
		project.TrackerConnected, project.LastTrackerSuccess, project.LastTrackerError = st.TrackerStatusLocked()
		if !project.TrackerConnected {
			project.Status = "network_lost"
			hasNetworkIssue = true
		} else if project.RetryCount > 0 {
			project.Status = "error"
			hasError = true
			for _, retry := range st.RetryQueue {
				if retry.Error != "" {
					project.LastError = retry.Error
					break
				}
			}
		} else if project.SubprocessCount > 0 {
			project.Status = "running"
		}
		st.Unlock()

		summary.SubprocessCount += project.SubprocessCount
		summary.RetryCount += project.RetryCount
		projectMap[pr.proj.Name] = project
	}

	sort.Strings(projectNames)
	for _, name := range projectNames {
		project := projectMap[name]
		summary.Projects = append(summary.Projects, project)
	}

	sort.Strings(summary.RunningIssueIDs)
	summary.ProjectCount = len(summary.Projects)

	switch {
	case hasError:
		summary.Status = "error"
	case hasNetworkIssue:
		summary.Status = "network_lost"
	case summary.SubprocessCount > 0:
		summary.Status = "running"
	default:
		summary.Status = "idle"
	}
	return summary
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
	return a.Name == b.Name && a.Workflow == b.Workflow
}
