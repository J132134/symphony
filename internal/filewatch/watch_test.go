package filewatch

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRunTriggersReloadOnlyForWatchedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "config.yaml")
	other := filepath.Join(dir, "other.yaml")
	writeFile(t, target, "alpha")
	writeFile(t, other, "ignored")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	reloads := 0
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, nil, target, 20*time.Millisecond, Callbacks{
			Reload: func() error {
				mu.Lock()
				reloads++
				mu.Unlock()
				return nil
			},
		})
	}()

	writeFile(t, other, "still ignored")
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if reloads != 0 {
		t.Fatalf("unexpected reload count after unrelated change: %d", reloads)
	}
	mu.Unlock()

	writeFile(t, target, "beta")
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return reloads == 1
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch run failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for watcher shutdown")
	}
}

func TestRunTriggersReloadWhenFileIsRecreated(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "WORKFLOW.md")
	writeFile(t, target, "alpha")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	reloads := 0
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, nil, target, 20*time.Millisecond, Callbacks{
			Reload: func() error {
				mu.Lock()
				reloads++
				mu.Unlock()
				return nil
			},
		})
	}()
	time.Sleep(50 * time.Millisecond)

	if err := os.Remove(target); err != nil {
		t.Fatalf("remove target: %v", err)
	}
	writeFile(t, target, "beta")
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return reloads >= 1
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch run failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for watcher shutdown")
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
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
