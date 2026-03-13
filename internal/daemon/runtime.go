package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"symphony/internal/config"
	"symphony/internal/filewatch"
	"symphony/internal/orchestrator"
	"symphony/internal/status"
	"symphony/internal/webhook"
)

const defaultConfigWatchDebounce = filewatch.DefaultDebounce

type runtimeLimiterKey struct{}

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
	limiter      *orchestrator.SessionLimiter

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
	if r.limiter == nil {
		r.limiter = orchestrator.NewSessionLimiter(initialCfg.MaxTotalConcurrentSessions())
	}
	r.limiter.SetLimit(initialCfg.MaxTotalConcurrentSessions())

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
			if errs := r.validateReloadConfig(cfg); len(errs) > 0 {
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

func (r *Runtime) validateReloadConfig(cfg *config.DaemonConfig) []string {
	if cfg == nil {
		return []string{"daemon config is required"}
	}

	r.mu.Lock()
	current := r.current
	r.mu.Unlock()

	if current == nil || current.cfg == nil {
		return cfg.Validate()
	}
	if !nextReusesCurrentListenerPort(current.cfg, cfg) {
		return cfg.Validate()
	}

	cloned := *cfg
	if nextStatusServerReusesCurrentListenerPort(current.cfg, cfg) {
		cloned.StatusServer.Enabled = false
	}
	if nextWebhookReusesCurrentListenerPort(current.cfg, cfg) {
		cloned.Webhook.Enabled = false
	}
	return cloned.Validate()
}

func currentListenerUsesPort(current *config.DaemonConfig, port int) bool {
	if current == nil {
		return false
	}
	return (current.StatusServer.Enabled && current.StatusServer.Port == port) ||
		(current.Webhook.Enabled && current.Webhook.Port == port)
}

func nextStatusServerReusesCurrentListenerPort(current, next *config.DaemonConfig) bool {
	if current == nil || next == nil || !next.StatusServer.Enabled {
		return false
	}
	return currentListenerUsesPort(current, next.StatusServer.Port)
}

func nextWebhookReusesCurrentListenerPort(current, next *config.DaemonConfig) bool {
	if current == nil || next == nil || !next.Webhook.Enabled {
		return false
	}
	return currentListenerUsesPort(current, next.Webhook.Port)
}

func nextReusesCurrentListenerPort(current, next *config.DaemonConfig) bool {
	return nextStatusServerReusesCurrentListenerPort(current, next) ||
		nextWebhookReusesCurrentListenerPort(current, next)
}

func (r *Runtime) swap(parent context.Context, cfg *config.DaemonConfig) error {
	if r.limiter == nil {
		r.limiter = orchestrator.NewSessionLimiter(cfg.MaxTotalConcurrentSessions())
	}
	r.limiter.SetLimit(cfg.MaxTotalConcurrentSessions())

	r.mu.Lock()
	prev := r.current
	sameBoundPort := prev != nil && nextReusesCurrentListenerPort(prev.cfg, cfg)
	if sameBoundPort {
		r.current = nil
	}
	r.mu.Unlock()

	nextParent := context.WithValue(parent, runtimeLimiterKey{}, r.limiter)
	if sameBoundPort {
		prev.stop()
		next, err := r.startRuntime(nextParent, cfg)
		if err != nil {
			return err
		}
		r.mu.Lock()
		r.current = next
		r.mu.Unlock()
		return nil
	}

	next, err := r.startRuntime(nextParent, cfg)
	if err != nil {
		return err
	}

	r.mu.Lock()
	prev = r.current
	r.current = next
	r.mu.Unlock()
	if prev != nil {
		prev.stop()
	}
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
	limiter, _ := parent.Value(runtimeLimiterKey{}).(*orchestrator.SessionLimiter)
	mgr := NewManagerWithLimiter(cfg, limiter)
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

	if shouldShareStatusAndWebhookListener(cfg) {
		if strings.TrimSpace(cfg.Webhook.SigningSecret) == "" {
			slog.Warn("webhook_server.signing_secret_missing", "port", cfg.Webhook.Port, "bind", cfg.Webhook.BindAddress)
		}
		mux := http.NewServeMux()
		status.RegisterRoutes(mux, mgr)
		webhook.RegisterRoutes(mux, webhook.NewHandler(cfg.Webhook.SigningSecret, mgr))
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runHTTPServer(ctx, cfg.Webhook.BindAddress, cfg.StatusServer.Port, mux); err != nil && ctx.Err() == nil {
				slog.Error("shared_http_server.error", "error", err)
			}
		}()
		slog.Info("status_server.started", "port", cfg.StatusServer.Port, "bind", cfg.Webhook.BindAddress, "shared", true)
		slog.Info("webhook_server.started", "port", cfg.Webhook.Port, "bind", cfg.Webhook.BindAddress, "shared", true)
	} else if cfg.StatusServer.Enabled {
		srv := status.New(mgr, cfg.StatusServer.Port)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
				slog.Error("status_server.error", "error", err)
			}
		}()
	}

	if cfg.Webhook.Enabled {
		if strings.TrimSpace(cfg.Webhook.SigningSecret) == "" {
			slog.Warn("webhook_server.signing_secret_missing", "port", cfg.Webhook.Port, "bind", cfg.Webhook.BindAddress)
		}
		srv := webhook.NewServer(
			webhook.NewHandler(cfg.Webhook.SigningSecret, mgr),
			cfg.Webhook.Port,
			cfg.Webhook.BindAddress,
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
				slog.Error("webhook_server.error", "error", err)
			}
		}()
		slog.Info("webhook_server.started", "port", cfg.Webhook.Port, "bind", cfg.Webhook.BindAddress)
	}

	var stopUpdate chan struct{}
	if cfg.AutoUpdate.Enabled {
		stopUpdate = make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			RunUpdateLoop(mgr, cfg.AutoUpdate.IntervalMinutes, stopUpdate)
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
		prev.StatusServer == next.StatusServer &&
		prev.Webhook == next.Webhook &&
		prev.ProjectHealth == next.ProjectHealth
}

func shouldShareStatusAndWebhookListener(cfg *config.DaemonConfig) bool {
	return cfg != nil &&
		cfg.StatusServer.Enabled &&
		cfg.Webhook.Enabled &&
		cfg.StatusServer.Port == cfg.Webhook.Port
}

func runHTTPServer(ctx context.Context, bind string, port int, handler http.Handler) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bind, port))
	if err != nil {
		return fmt.Errorf("listen %s:%d: %w", bind, port, err)
	}

	srv := &http.Server{Handler: handler}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func projectNames(cfg *config.DaemonConfig) []string {
	names := make([]string, len(cfg.Projects))
	for i, p := range cfg.Projects {
		names[i] = p.Name
	}
	return names
}
