package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"symphony/internal/config"
	"symphony/internal/status"
)

const defaultConfigWatchInterval = 5 * time.Second

type fileStamp struct {
	exists  bool
	modTime time.Time
	size    int64
}

func (s fileStamp) changed(next fileStamp) bool {
	if s.exists != next.exists || s.size != next.size {
		return true
	}
	if !s.exists {
		return false
	}
	return !s.modTime.Equal(next.modTime)
}

func readFileStamp(path string) (fileStamp, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fileStamp{}, nil
		}
		return fileStamp{}, err
	}
	return fileStamp{
		exists:  true,
		modTime: info.ModTime(),
		size:    info.Size(),
	}, nil
}

type managedRuntime struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (r *managedRuntime) stop() {
	if r == nil {
		return
	}
	r.cancel()
	<-r.done
}

// Runtime supervises the daemon app and hot-reloads it when config.yaml changes.
type Runtime struct {
	configPath    string
	watchInterval time.Duration

	loadConfig func(string) (*config.DaemonConfig, error)
	runApp     func(context.Context, *config.DaemonConfig)

	mu      sync.Mutex
	current *managedRuntime
}

func NewRuntime(configPath string) *Runtime {
	return &Runtime{
		configPath:    configPath,
		watchInterval: defaultConfigWatchInterval,
		loadConfig:    config.LoadDaemonConfig,
		runApp:        runDaemonApp,
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
	if r.watchInterval <= 0 {
		r.watchInterval = defaultConfigWatchInterval
	}

	if err := r.swap(ctx, initialCfg); err != nil {
		return err
	}
	defer r.stopCurrent()

	lastSeen, err := readFileStamp(r.configPath)
	if err != nil {
		return fmt.Errorf("stat daemon config: %w", err)
	}

	ticker := time.NewTicker(r.watchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			next, err := readFileStamp(r.configPath)
			if err != nil {
				slog.Error("daemon.config_watch_failed", "path", r.configPath, "error", err)
				continue
			}
			if !lastSeen.changed(next) {
				continue
			}
			lastSeen = next

			if !next.exists {
				slog.Error("daemon.config_reload_failed", "path", r.configPath, "error", "config file not found")
				continue
			}

			cfg, err := r.loadConfig(r.configPath)
			if err != nil {
				slog.Error("daemon.config_reload_failed", "path", r.configPath, "error", err)
				continue
			}
			if errs := cfg.Validate(); len(errs) > 0 {
				slog.Error("daemon.config_reload_failed", "path", r.configPath, "error", strings.Join(errs, "; "))
				continue
			}

			slog.Info("daemon.config_reloading", "path", r.configPath, "projects", projectNames(cfg))
			if err := r.swap(ctx, cfg); err != nil {
				slog.Error("daemon.config_reload_failed", "path", r.configPath, "error", err)
				continue
			}
			slog.Info("daemon.config_reloaded", "path", r.configPath, "projects", projectNames(cfg))
		}
	}
}

func (r *Runtime) swap(parent context.Context, cfg *config.DaemonConfig) error {
	r.mu.Lock()
	prev := r.current
	r.current = nil
	r.mu.Unlock()

	if prev != nil {
		prev.stop()
	}

	nextCtx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.runApp(nextCtx, cfg)
	}()

	r.mu.Lock()
	r.current = &managedRuntime{cancel: cancel, done: done}
	r.mu.Unlock()
	return nil
}

func (r *Runtime) stopCurrent() {
	r.mu.Lock()
	current := r.current
	r.current = nil
	r.mu.Unlock()
	current.stop()
}

func runDaemonApp(ctx context.Context, cfg *config.DaemonConfig) {
	mgr := NewManager(cfg)

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

func projectNames(cfg *config.DaemonConfig) []string {
	names := make([]string, len(cfg.Projects))
	for i, p := range cfg.Projects {
		names[i] = p.Name
	}
	return names
}
