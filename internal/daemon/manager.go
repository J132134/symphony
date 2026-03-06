// Package daemon manages multiple orchestrators as a single long-running process.
package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"symphony/internal/config"
	"symphony/internal/orchestrator"
)

// projectRunner manages the lifecycle of one Orchestrator with auto-restart.
type projectRunner struct {
	proj    config.ProjectConfig
	limiter *orchestrator.SessionLimiter

	mu       sync.Mutex
	orch     *orchestrator.Orchestrator
	draining bool
}

func (pr *projectRunner) run(ctx context.Context) {
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		pr.mu.Lock()
		draining := pr.draining
		pr.mu.Unlock()
		if draining {
			return
		}

		o := orchestrator.New(pr.proj.Workflow, 0, pr.proj.Name, pr.limiter)

		pr.mu.Lock()
		pr.orch = o
		pr.mu.Unlock()

		slog.Info("daemon.project_starting", "project", pr.proj.Name)
		err := o.Run(ctx)

		pr.mu.Lock()
		pr.orch = nil
		pr.mu.Unlock()

		if ctx.Err() != nil {
			return
		}
		pr.mu.Lock()
		draining = pr.draining
		pr.mu.Unlock()
		if draining {
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
	o := pr.orch
	pr.mu.Unlock()
	if o != nil {
		o.Stop()
	}
}

func (pr *projectRunner) isIdle() bool {
	pr.mu.Lock()
	o := pr.orch
	draining := pr.draining
	pr.mu.Unlock()
	if o == nil {
		return draining
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

// Manager coordinates multiple Orchestrators.
type Manager struct {
	mu      sync.Mutex
	cfg     *config.DaemonConfig
	runners []*projectRunner
	cancel  context.CancelFunc
	limiter *orchestrator.SessionLimiter

	restartRequested bool
	restartReady     chan struct{}
}

func NewManager(cfg *config.DaemonConfig) *Manager {
	return &Manager{
		cfg:     cfg,
		limiter: orchestrator.NewSessionLimiter(cfg.MaxTotalConcurrentSessions()),
	}
}

// Run starts all projects and blocks until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.cancel = cancel
	m.mu.Unlock()
	defer cancel()

	runners := make([]*projectRunner, len(m.cfg.Projects))
	for i, proj := range m.cfg.Projects {
		pr := &projectRunner{proj: proj, limiter: m.limiter}
		runners[i] = pr
		go pr.run(ctx)
	}
	m.mu.Lock()
	m.runners = runners
	restartRequested := m.restartRequested
	m.mu.Unlock()
	if restartRequested {
		for _, pr := range runners {
			pr.beginDrain()
		}
	}

	slog.Info("daemon.started", "projects", len(m.cfg.Projects), "max_total_concurrent_sessions", m.cfg.MaxTotalConcurrentSessions())
	<-ctx.Done()

	slog.Info("daemon.shutting_down")
	for _, pr := range runners {
		pr.stop()
	}
	slog.Info("daemon.stopped")
}

// Shutdown triggers a graceful stop (for auto-update).
func (m *Manager) Shutdown() {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
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
	runners := append([]*projectRunner(nil), m.runners...)
	ch := m.restartReady
	m.mu.Unlock()

	for _, pr := range runners {
		pr.beginDrain()
	}

	go m.waitForIdle(ch)
	return ch
}

// GetAllStates returns state for all running projects.
func (m *Manager) GetAllStates() map[string]*orchestrator.State {
	m.mu.Lock()
	runners := append([]*projectRunner(nil), m.runners...)
	m.mu.Unlock()

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
	m.mu.Lock()
	runners := append([]*projectRunner(nil), m.runners...)
	m.mu.Unlock()

	for _, pr := range runners {
		if !pr.isIdle() {
			return false
		}
	}
	return true
}
