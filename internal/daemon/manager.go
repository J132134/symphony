// Package daemon manages multiple orchestrators as a single long-running process.
package daemon

import (
	"context"
	"log/slog"
	"sync"

	"symphony/internal/config"
	"symphony/internal/orchestrator"
)

// projectRunner manages the lifecycle of one Orchestrator with auto-restart.
type projectRunner struct {
	proj config.ProjectConfig

	mu   sync.Mutex
	orch *orchestrator.Orchestrator
}

func (pr *projectRunner) run(ctx context.Context) {
	for ctx.Err() == nil {
		o := orchestrator.New(pr.proj.Workflow, 0, pr.proj.Name)

		pr.mu.Lock()
		pr.orch = o
		pr.mu.Unlock()

		slog.Info("daemon.project_starting", "project", pr.proj.Name)
		if err := o.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("daemon.project_crashed", "project", pr.proj.Name, "error", err)
		} else {
			slog.Info("daemon.project_stopped", "project", pr.proj.Name)
		}

		pr.mu.Lock()
		pr.orch = nil
		pr.mu.Unlock()

		if ctx.Err() != nil {
			return
		}
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
	cfg     *config.DaemonConfig
	runners []*projectRunner
	cancel  context.CancelFunc
}

func NewManager(cfg *config.DaemonConfig) *Manager {
	return &Manager{cfg: cfg}
}

// Run starts all projects and blocks until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	defer cancel()

	m.runners = make([]*projectRunner, len(m.cfg.Projects))
	for i, proj := range m.cfg.Projects {
		pr := &projectRunner{proj: proj}
		m.runners[i] = pr
		go pr.run(ctx)
	}

	slog.Info("daemon.started", "projects", len(m.cfg.Projects))
	<-ctx.Done()

	slog.Info("daemon.shutting_down")
	for _, pr := range m.runners {
		pr.stop()
	}
	slog.Info("daemon.stopped")
}

// Shutdown triggers a graceful stop (for auto-update).
func (m *Manager) Shutdown() {
	if m.cancel != nil {
		m.cancel()
	}
}

// GetAllStates returns state for all running projects.
func (m *Manager) GetAllStates() map[string]*orchestrator.State {
	result := make(map[string]*orchestrator.State, len(m.runners))
	for _, pr := range m.runners {
		if st := pr.getState(); st != nil {
			result[pr.proj.Name] = st
		}
	}
	return result
}
