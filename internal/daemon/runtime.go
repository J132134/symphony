package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"symphony/internal/config"
	"symphony/internal/filewatch"
	"symphony/internal/status"
)

const defaultConfigWatchDebounce = filewatch.DefaultDebounce

type managedRuntime struct {
	cfg      *config.DaemonConfig
	reloader configApplier
	cancel   context.CancelFunc
	done     chan struct{}
}

func (r *managedRuntime) stop() {
	if r == nil {
		return
	}
	r.cancel()
	<-r.done
}

type configApplier interface {
	ApplyConfig(*config.DaemonConfig)
}

// Runtime supervises the daemon app and hot-reloads it when config.yaml changes.
type Runtime struct {
	configPath    string
	watchDebounce time.Duration

	loadConfig   func(string) (*config.DaemonConfig, error)
	startRuntime func(context.Context, *config.DaemonConfig) (*managedRuntime, error)

	mu      sync.Mutex
	current *managedRuntime
}

func NewRuntime(configPath string) *Runtime {
	return &Runtime{
		configPath:    configPath,
		watchDebounce: defaultConfigWatchDebounce,
		loadConfig:    config.LoadDaemonConfig,
		startRuntime:  startManagedRuntime,
	}
}

func (r *Runtime) Run(ctx context.Context, initialCfg *config.DaemonConfig) error {
	if initialCfg == nil {
		return fmt.Errorf("initial daemon config is required")
	}
	if r.configPath == "" {
		r.configPath = initialCfg.ConfigPath
	}
	if r.configPath == "" {
		return fmt.Errorf("daemon config path is required")
	}
	if r.watchDebounce <= 0 {
		r.watchDebounce = defaultConfigWatchDebounce
	}

	if err := r.swap(ctx, initialCfg); err != nil {
		return err
	}
	defer r.stopCurrent()

	return filewatch.Run(ctx, nil, r.configPath, r.watchDebounce, filewatch.Callbacks{
		Reload: func() error {
			cfg, err := r.loadConfig(r.configPath)
			if err != nil {
				return err
			}
			if errs := cfg.Validate(); len(errs) > 0 {
				return fmt.Errorf(strings.Join(errs, "; "))
			}

			slog.Info("daemon.config_reloading", "path", r.configPath, "projects", projectNames(cfg))
			if r.reloadProjects(cfg) {
				slog.Info("daemon.config_reloaded", "path", r.configPath, "projects", projectNames(cfg), "mode", "incremental")
				return nil
			}
			if err := r.swap(ctx, cfg); err != nil {
				return err
			}
			slog.Info("daemon.config_reloaded", "path", r.configPath, "projects", projectNames(cfg), "mode", "full_swap")
			return nil
		},
		OnReloadError: func(err error) {
			slog.Error("daemon.config_reload_failed", "path", r.configPath, "error", err)
		},
		OnWatchError: func(err error) {
			slog.Error("daemon.config_watch_failed", "path", r.configPath, "error", err)
		},
	})
}

func (r *Runtime) swap(parent context.Context, cfg *config.DaemonConfig) error {
	r.mu.Lock()
	prev := r.current
	r.current = nil
	r.mu.Unlock()

	if prev != nil {
		prev.stop()
	}

	next, err := r.startRuntime(parent, cfg)
	if err != nil {
		return err
	}

	r.mu.Lock()
	r.current = next
	r.mu.Unlock()
	return nil
}

func (r *Runtime) reloadProjects(cfg *config.DaemonConfig) bool {
	r.mu.Lock()
	current := r.current
	if current == nil || current.reloader == nil || !canReloadProjectsIncrementally(current.cfg, cfg) {
		r.mu.Unlock()
		return false
	}
	current.cfg = cfg
	reloader := current.reloader
	r.mu.Unlock()

	reloader.ApplyConfig(cfg)
	return true
}

func (r *Runtime) stopCurrent() {
	r.mu.Lock()
	current := r.current
	r.current = nil
	r.mu.Unlock()
	current.stop()
}

func startManagedRuntime(parent context.Context, cfg *config.DaemonConfig) (*managedRuntime, error) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	mgr := NewManager(cfg)
	go func() {
		defer close(done)
		runDaemonApp(ctx, cfg, mgr)
	}()
	return &managedRuntime{
		cfg:      cfg,
		reloader: mgr,
		cancel:   cancel,
		done:     done,
	}, nil
}

func runDaemonApp(ctx context.Context, cfg *config.DaemonConfig, mgr *Manager) {
	var wg sync.WaitGroup

	if cfg.StatusServer.Enabled {
		srv := status.New(mgr, cfg.StatusServer.Port)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
				slog.Error("status_server.error", "error", err)
			}
		}()
	}

	var stopUpdate chan struct{}
	if cfg.AutoUpdate.Enabled {
		stopUpdate = make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			RunUpdateLoop(mgr, cfg.AutoUpdate.IntervalMinutes, cfg.AutoUpdate.RepoDir, stopUpdate)
		}()
	}

	mgr.Run(ctx)

	if stopUpdate != nil {
		close(stopUpdate)
	}
	wg.Wait()
}

func canReloadProjectsIncrementally(prev, next *config.DaemonConfig) bool {
	if prev == nil || next == nil {
		return false
	}
	return prev.AutoUpdate == next.AutoUpdate &&
		prev.Agent == next.Agent &&
		prev.StatusServer == next.StatusServer
}

func projectNames(cfg *config.DaemonConfig) []string {
	names := make([]string, len(cfg.Projects))
	for i, p := range cfg.Projects {
		names[i] = p.Name
	}
	return names
}
