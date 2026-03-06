package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"symphony/internal/config"
)

func TestRuntimeReloadsValidConfigAndKeepsCurrentRuntimeOnInvalidConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("# workflow\n"), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	writeConfigToken(t, configPath, "alpha")

	alphaCfg := &config.DaemonConfig{
		ConfigPath: configPath,
		Projects:   []config.ProjectConfig{{Name: "alpha", Workflow: workflowPath}},
	}
	betaCfg := &config.DaemonConfig{
		ConfigPath: configPath,
		Projects:   []config.ProjectConfig{{Name: "beta", Workflow: workflowPath}},
	}
	invalidCfg := &config.DaemonConfig{ConfigPath: configPath}

	var mu sync.Mutex
	var started []string
	var stopped []string

	runtime := &Runtime{
		configPath:    configPath,
		watchInterval: 10 * time.Millisecond,
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
		runApp: func(ctx context.Context, cfg *config.DaemonConfig) {
			name := cfg.Projects[0].Name
			mu.Lock()
			started = append(started, name)
			mu.Unlock()

			<-ctx.Done()

			mu.Lock()
			stopped = append(stopped, name)
			mu.Unlock()
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

func TestReadFileStampDetectsChanges(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	stamp1, err := readFileStamp(path)
	if err != nil {
		t.Fatalf("read missing stamp: %v", err)
	}
	if stamp1.exists {
		t.Fatal("missing file should report exists=false")
	}

	writeConfigToken(t, path, "alpha")
	stamp2, err := readFileStamp(path)
	if err != nil {
		t.Fatalf("read existing stamp: %v", err)
	}
	if !stamp1.changed(stamp2) {
		t.Fatal("expected create to change file stamp")
	}
	if stamp2.changed(stamp2) {
		t.Fatal("same file stamp should not report change")
	}

	writeConfigToken(t, path, "beta!!")
	stamp3, err := readFileStamp(path)
	if err != nil {
		t.Fatalf("read updated stamp: %v", err)
	}
	if !stamp2.changed(stamp3) {
		t.Fatal("expected content update to change file stamp")
	}
}

func writeConfigToken(t *testing.T, path, token string) {
	t.Helper()
	time.Sleep(20 * time.Millisecond)
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
