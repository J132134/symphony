package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCheckForUpdatesUsesShutdownPathWithoutWaitingForIdle(t *testing.T) {
	t.Parallel()

	prevPrepare := prepareUpdateFn
	prevExit := updaterExitFn
	t.Cleanup(func() {
		prepareUpdateFn = prevPrepare
		updaterExitFn = prevExit
	})

	prepareUpdateFn = func() (bool, error) {
		return true, nil
	}

	var cancelOnce sync.Once
	stopped := make(chan struct{})
	exited := make(chan int, 1)
	done := make(chan struct{})
	close(done)

	mgr := &Manager{
		cancel:           func() { cancelOnce.Do(func() { close(stopped) }) },
		done:             done,
		restartRequested: true,
		restartReady:     make(chan struct{}),
	}

	updaterExitFn = func(code int) {
		exited <- code
	}

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		CheckForUpdates(mgr)
	}()

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("CheckForUpdates should trigger Shutdown without waiting for idle")
	}

	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	case <-time.After(time.Second):
		t.Fatal("CheckForUpdates should exit after Shutdown+Wait")
	}

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("CheckForUpdates should return after updaterExitFn")
	}
}

func TestInstallBuiltBinaryReplacesExistingBinaryAndCleansBackup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("new-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile(src) error = %v", err)
	}
	if err := os.WriteFile(dst, []byte("old-binary"), 0o700); err != nil {
		t.Fatalf("WriteFile(dst) error = %v", err)
	}

	if err := installBuiltBinary(src, dst); err != nil {
		t.Fatalf("installBuiltBinary() error = %v", err)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile(dst) error = %v", err)
	}
	if string(data) != "new-binary" {
		t.Fatalf("dst content = %q, want %q", data, "new-binary")
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("Stat(dst) error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("dst mode = %o, want %o", got, 0o700)
	}
	if _, err := os.Stat(dst + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("backup exists after successful install: %v", err)
	}
}

func TestInstallBuiltBinaryRollsBackOnReplaceFailure(t *testing.T) {
	t.Parallel()

	prevRename := renameFileFn
	prevRemove := removeFileFn
	t.Cleanup(func() {
		renameFileFn = prevRename
		removeFileFn = prevRemove
	})

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("new-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile(src) error = %v", err)
	}
	if err := os.WriteFile(dst, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile(dst) error = %v", err)
	}

	renameFileFn = func(oldPath, newPath string) error {
		if strings.Contains(filepath.Base(oldPath), "dst.new-") && newPath == dst {
			return fmt.Errorf("forced replace failure")
		}
		return os.Rename(oldPath, newPath)
	}
	removeFileFn = os.Remove

	err := installBuiltBinary(src, dst)
	if err == nil || !strings.Contains(err.Error(), "replace binary") {
		t.Fatalf("installBuiltBinary() error = %v, want replace failure", err)
	}

	data, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatalf("ReadFile(dst) error = %v", readErr)
	}
	if string(data) != "old-binary" {
		t.Fatalf("dst content after rollback = %q, want %q", data, "old-binary")
	}
	if _, statErr := os.Stat(dst + ".bak"); !os.IsNotExist(statErr) {
		t.Fatalf("backup exists after rollback: %v", statErr)
	}
	matches, globErr := filepath.Glob(filepath.Join(dir, "dst.new-*"))
	if globErr != nil {
		t.Fatalf("Glob() error = %v", globErr)
	}
	if len(matches) != 0 {
		t.Fatalf("staged files left behind after rollback: %v", matches)
	}
}
