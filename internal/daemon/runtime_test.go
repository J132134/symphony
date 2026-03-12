package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"symphony/internal/config"
	"symphony/internal/orchestrator"
)

func TestRuntimeReloadsDaemonGlobalChangesAndKeepsCurrentRuntimeOnInvalidConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("# workflow\n"), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	writeConfigToken(t, configPath, "alpha")

	ln, port := listenOnRandomPort(t)
	_ = ln.Close()

	alphaCfg := &config.DaemonConfig{
		ConfigPath:    configPath,
		Projects:      []config.ProjectConfig{{Name: "alpha", Workflow: workflowPath}},
		ProjectHealth: config.ProjectHealthConfig{RestartBudgetCount: 3, RestartBudgetWindowMinutes: 15, ProbeIntervalSeconds: 60},
	}
	betaCfg := &config.DaemonConfig{
		ConfigPath:    configPath,
		Projects:      []config.ProjectConfig{{Name: "beta", Workflow: workflowPath}},
		StatusServer:  config.StatusServerConfig{Enabled: true, Port: port},
		ProjectHealth: config.ProjectHealthConfig{RestartBudgetCount: 3, RestartBudgetWindowMinutes: 15, ProbeIntervalSeconds: 60},
	}
	invalidCfg := &config.DaemonConfig{ConfigPath: configPath}

	var mu sync.Mutex
	var started []string
	var stopped []string

	runtime := &Runtime{
		configPath:    configPath,
		watchDebounce: 10 * time.Millisecond,
		loadConfig: func(path string) (*config.DaemonConfig, error) {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			switch string(data) {
			case "alpha":
				return alphaCfg, nil
			case "beta!!":
				return betaCfg, nil
			case "broken":
				return invalidCfg, nil
			default:
				t.Fatalf("unexpected config token: %q", data)
				return nil, nil
			}
		},
		startRuntime: func(parent context.Context, cfg *config.DaemonConfig) (*managedRuntime, error) {
			name := cfg.Projects[0].Name
			mu.Lock()
			started = append(started, name)
			mu.Unlock()

			ctx, cancel := context.WithCancel(parent)
			done := make(chan struct{})
			go func() {
				defer close(done)
				<-ctx.Done()
				mu.Lock()
				stopped = append(stopped, name)
				mu.Unlock()
			}()
			return &managedRuntime{cfg: cfg, cancel: cancel, done: done}, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- runtime.Run(ctx, alphaCfg)
	}()

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Equal(started, []string{"alpha"})
	})

	writeConfigToken(t, configPath, "beta!!")
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Equal(started, []string{"alpha", "beta"}) && slices.Equal(stopped, []string{"alpha"})
	})

	writeConfigToken(t, configPath, "broken")
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if !slices.Equal(started, []string{"alpha", "beta"}) {
		t.Fatalf("invalid config should not start a new runtime, got starts=%v", started)
	}
	if !slices.Equal(stopped, []string{"alpha"}) {
		t.Fatalf("invalid config should keep current runtime running, got stops=%v", stopped)
	}
	mu.Unlock()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runtime exited with error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runtime shutdown")
	}

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(stopped, []string{"alpha", "beta"}) {
		t.Fatalf("expected beta runtime to stop on shutdown, got %v", stopped)
	}
}

func TestRuntimeAppliesProjectOnlyReloadIncrementally(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("# workflow\n"), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	writeConfigToken(t, configPath, "alpha")

	alphaCfg := &config.DaemonConfig{
		ConfigPath:    configPath,
		Projects:      []config.ProjectConfig{{Name: "alpha", Workflow: workflowPath}},
		ProjectHealth: config.ProjectHealthConfig{RestartBudgetCount: 3, RestartBudgetWindowMinutes: 15, ProbeIntervalSeconds: 60},
	}
	betaCfg := &config.DaemonConfig{
		ConfigPath:    configPath,
		Projects:      []config.ProjectConfig{{Name: "beta", Workflow: workflowPath}},
		ProjectHealth: config.ProjectHealthConfig{RestartBudgetCount: 3, RestartBudgetWindowMinutes: 15, ProbeIntervalSeconds: 60},
	}

	var mu sync.Mutex
	starts := 0
	stops := 0
	reloader := &recordingConfigApplier{}

	runtime := &Runtime{
		configPath:    configPath,
		watchDebounce: 10 * time.Millisecond,
		loadConfig: func(path string) (*config.DaemonConfig, error) {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			switch string(data) {
			case "alpha":
				return alphaCfg, nil
			case "beta!!":
				return betaCfg, nil
			default:
				t.Fatalf("unexpected config token: %q", data)
				return nil, nil
			}
		},
		startRuntime: func(parent context.Context, cfg *config.DaemonConfig) (*managedRuntime, error) {
			mu.Lock()
			starts++
			mu.Unlock()

			ctx, cancel := context.WithCancel(parent)
			done := make(chan struct{})
			go func() {
				defer close(done)
				<-ctx.Done()
				mu.Lock()
				stops++
				mu.Unlock()
			}()
			return &managedRuntime{cfg: cfg, reloader: reloader, cancel: cancel, done: done}, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- runtime.Run(ctx, alphaCfg)
	}()

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return starts == 1
	})

	writeConfigToken(t, configPath, "beta!!")
	waitFor(t, func() bool {
		return reflect.DeepEqual(reloader.projectSets(), [][]string{{"beta"}})
	})

	mu.Lock()
	if starts != 1 {
		t.Fatalf("project-only reload should not start a new runtime, got %d", starts)
	}
	if stops != 0 {
		t.Fatalf("project-only reload should not stop the current runtime, got %d", stops)
	}
	mu.Unlock()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runtime exited with error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runtime shutdown")
	}

	mu.Lock()
	defer mu.Unlock()
	if stops != 1 {
		t.Fatalf("expected runtime to stop once on shutdown, got %d", stops)
	}
}

func TestRuntimeReloadAllowsCurrentStatusServerPort(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("# workflow\n"), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	writeConfigToken(t, configPath, "alpha")

	ln, port := listenOnRandomPort(t)
	defer ln.Close()

	alphaCfg := &config.DaemonConfig{
		ConfigPath:    configPath,
		Projects:      []config.ProjectConfig{{Name: "alpha", Workflow: workflowPath}},
		StatusServer:  config.StatusServerConfig{Enabled: true, Port: port},
		ProjectHealth: config.ProjectHealthConfig{RestartBudgetCount: 3, RestartBudgetWindowMinutes: 15, ProbeIntervalSeconds: 60},
	}
	betaCfg := &config.DaemonConfig{
		ConfigPath:    configPath,
		Projects:      []config.ProjectConfig{{Name: "beta", Workflow: workflowPath}},
		StatusServer:  config.StatusServerConfig{Enabled: true, Port: port},
		ProjectHealth: config.ProjectHealthConfig{RestartBudgetCount: 3, RestartBudgetWindowMinutes: 15, ProbeIntervalSeconds: 60},
	}

	reloader := &recordingConfigApplier{}
	var mu sync.Mutex
	starts := 0
	runtime := &Runtime{
		configPath:    configPath,
		watchDebounce: 10 * time.Millisecond,
		loadConfig: func(path string) (*config.DaemonConfig, error) {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			switch string(data) {
			case "alpha":
				return alphaCfg, nil
			case "beta!!":
				return betaCfg, nil
			default:
				t.Fatalf("unexpected config token: %q", data)
				return nil, nil
			}
		},
		startRuntime: func(parent context.Context, cfg *config.DaemonConfig) (*managedRuntime, error) {
			mu.Lock()
			starts++
			mu.Unlock()

			ctx, cancel := context.WithCancel(parent)
			done := make(chan struct{})
			go func() {
				defer close(done)
				<-ctx.Done()
			}()
			return &managedRuntime{cfg: cfg, reloader: reloader, cancel: cancel, done: done}, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- runtime.Run(ctx, alphaCfg)
	}()

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return starts == 1
	})

	writeConfigToken(t, configPath, "beta!!")
	waitFor(t, func() bool {
		return reflect.DeepEqual(reloader.projectSets(), [][]string{{"beta"}})
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runtime exited with error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runtime shutdown")
	}
}

func TestRuntimeSwapStartsNewBeforeStoppingOld(t *testing.T) {
	t.Parallel()

	cfg := &config.DaemonConfig{
		Projects: []config.ProjectConfig{{Name: "alpha", Workflow: "/tmp/a"}},
	}
	runtime := &Runtime{}
	var mu sync.Mutex
	var events []string

	runtime.startRuntime = func(parent context.Context, cfg *config.DaemonConfig) (*managedRuntime, error) {
		mu.Lock()
		events = append(events, "start:"+cfg.Projects[0].Name)
		mu.Unlock()
		done := make(chan struct{})
		return &managedRuntime{
			cfg: cfg,
			cancel: func() {
				mu.Lock()
				events = append(events, "stop:"+cfg.Projects[0].Name)
				mu.Unlock()
				close(done)
			},
			done: done,
		}, nil
	}

	if err := runtime.swap(context.Background(), cfg); err != nil {
		t.Fatalf("swap(alpha): %v", err)
	}

	nextCfg := &config.DaemonConfig{
		Projects: []config.ProjectConfig{{Name: "beta", Workflow: "/tmp/b"}},
	}
	if err := runtime.swap(context.Background(), nextCfg); err != nil {
		t.Fatalf("swap(beta): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := events, []string{"start:alpha", "start:beta", "stop:alpha"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestRuntimeSwapStopsOldBeforeStartingNewWhenStatusServerPortIsReused(t *testing.T) {
	t.Parallel()

	port := 7777
	alpha := &config.DaemonConfig{
		Projects:     []config.ProjectConfig{{Name: "alpha", Workflow: "/tmp/a"}},
		StatusServer: config.StatusServerConfig{Enabled: true, Port: port},
	}
	beta := &config.DaemonConfig{
		Projects:     []config.ProjectConfig{{Name: "beta", Workflow: "/tmp/b"}},
		StatusServer: config.StatusServerConfig{Enabled: true, Port: port},
		Agent:        config.DaemonAgentConfig{MaxTotalConcurrentSessions: 5},
	}

	runtime := &Runtime{}
	var mu sync.Mutex
	var events []string

	runtime.startRuntime = func(parent context.Context, cfg *config.DaemonConfig) (*managedRuntime, error) {
		mu.Lock()
		events = append(events, "start:"+cfg.Projects[0].Name)
		mu.Unlock()
		done := make(chan struct{})
		return &managedRuntime{
			cfg: cfg,
			cancel: func() {
				mu.Lock()
				events = append(events, "stop:"+cfg.Projects[0].Name)
				mu.Unlock()
				close(done)
			},
			done: done,
		}, nil
	}

	if err := runtime.swap(context.Background(), alpha); err != nil {
		t.Fatalf("swap(alpha): %v", err)
	}
	if err := runtime.swap(context.Background(), beta); err != nil {
		t.Fatalf("swap(beta): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := events, []string{"start:alpha", "stop:alpha", "start:beta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestRuntimeSwapStopsOldBeforeStartingNewWhenWebhookPortIsReused(t *testing.T) {
	t.Parallel()

	port := 7778
	alpha := &config.DaemonConfig{
		Projects: []config.ProjectConfig{{Name: "alpha", Workflow: "/tmp/a"}},
		Webhook:  config.WebhookConfig{Enabled: true, Port: port, BindAddress: "127.0.0.1"},
	}
	beta := &config.DaemonConfig{
		Projects: []config.ProjectConfig{{Name: "beta", Workflow: "/tmp/b"}},
		Webhook:  config.WebhookConfig{Enabled: true, Port: port, BindAddress: "127.0.0.1"},
	}

	runtime := &Runtime{}
	var mu sync.Mutex
	var events []string

	runtime.startRuntime = func(parent context.Context, cfg *config.DaemonConfig) (*managedRuntime, error) {
		mu.Lock()
		events = append(events, "start:"+cfg.Projects[0].Name)
		mu.Unlock()
		done := make(chan struct{})
		return &managedRuntime{
			cfg: cfg,
			cancel: func() {
				mu.Lock()
				events = append(events, "stop:"+cfg.Projects[0].Name)
				mu.Unlock()
				close(done)
			},
			done: done,
		}, nil
	}

	if err := runtime.swap(context.Background(), alpha); err != nil {
		t.Fatalf("swap(alpha): %v", err)
	}
	if err := runtime.swap(context.Background(), beta); err != nil {
		t.Fatalf("swap(beta): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := events, []string{"start:alpha", "stop:alpha", "start:beta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestRuntimeSwapSharesLimiterAndUpdatesLimit(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var limits []int
	var ptrs []string

	runtime := &Runtime{
		startRuntime: func(parent context.Context, cfg *config.DaemonConfig) (*managedRuntime, error) {
			limiter, _ := parent.Value(runtimeLimiterKey{}).(*orchestrator.SessionLimiter)
			if limiter == nil {
				t.Fatal("expected shared limiter in runtime context")
			}
			mu.Lock()
			limits = append(limits, limiter.Limit())
			ptrs = append(ptrs, fmt.Sprintf("%p", limiter))
			mu.Unlock()

			done := make(chan struct{})
			return &managedRuntime{
				cfg:    cfg,
				cancel: func() { close(done) },
				done:   done,
			}, nil
		},
	}

	alpha := &config.DaemonConfig{
		Projects: []config.ProjectConfig{{Name: "alpha", Workflow: "/tmp/a"}},
		Agent:    config.DaemonAgentConfig{MaxTotalConcurrentSessions: 2},
	}
	beta := &config.DaemonConfig{
		Projects: []config.ProjectConfig{{Name: "beta", Workflow: "/tmp/b"}},
		Agent:    config.DaemonAgentConfig{MaxTotalConcurrentSessions: 5},
	}

	if err := runtime.swap(context.Background(), alpha); err != nil {
		t.Fatalf("swap(alpha): %v", err)
	}
	if err := runtime.swap(context.Background(), beta); err != nil {
		t.Fatalf("swap(beta): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := limits, []int{2, 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("limiter limits = %v, want %v", got, want)
	}
	if len(ptrs) != 2 || ptrs[0] != ptrs[1] {
		t.Fatalf("expected limiter pointer to be shared, got %v", ptrs)
	}
}

func TestCanReloadProjectsIncrementallyRejectsWebhookChanges(t *testing.T) {
	t.Parallel()

	prev := &config.DaemonConfig{
		AutoUpdate:    config.AutoUpdateConfig{Enabled: true, IntervalMinutes: 30},
		StatusServer:  config.StatusServerConfig{Enabled: true, Port: 7777},
		Webhook:       config.WebhookConfig{Enabled: false, Port: 7778, BindAddress: "127.0.0.1"},
		ProjectHealth: config.ProjectHealthConfig{RestartBudgetCount: 3, RestartBudgetWindowMinutes: 15, ProbeIntervalSeconds: 60},
	}
	next := &config.DaemonConfig{
		AutoUpdate:    prev.AutoUpdate,
		StatusServer:  prev.StatusServer,
		Webhook:       config.WebhookConfig{Enabled: true, Port: 7778, BindAddress: "127.0.0.1"},
		ProjectHealth: prev.ProjectHealth,
	}

	if canReloadProjectsIncrementally(prev, next) {
		t.Fatal("webhook config changes should require full runtime reload")
	}
}

func writeConfigToken(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func waitFor(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func listenOnRandomPort(t *testing.T) (net.Listener, int) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener addr type %T", ln.Addr())
	}
	return ln, addr.Port
}

type recordingConfigApplier struct {
	mu     sync.Mutex
	sets   [][]string
	config []*config.DaemonConfig
}

func (r *recordingConfigApplier) ApplyConfig(cfg *config.DaemonConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.config = append(r.config, cfg)
	r.sets = append(r.sets, projectNames(cfg))
}

func (r *recordingConfigApplier) projectSets() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.sets)
}
