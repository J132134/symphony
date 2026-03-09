package daemon

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func newTestMgr() (*Manager, <-chan struct{}) {
	var cancelOnce sync.Once
	stopped := make(chan struct{})
	done := make(chan struct{})
	close(done)
	mgr := &Manager{
		cancel:           func() { cancelOnce.Do(func() { close(stopped) }) },
		done:             done,
		restartRequested: true,
		restartReady:     make(chan struct{}),
	}
	return mgr, stopped
}

func TestCheckForUpdatesUsesShutdownPathWithoutWaitingForIdle(t *testing.T) {
	t.Parallel()

	prevPrepare := prepareUpdateFn
	prevExit := updaterExitFn
	prevExec := updaterExecFn
	t.Cleanup(func() {
		prepareUpdateFn = prevPrepare
		updaterExitFn = prevExit
		updaterExecFn = prevExec
	})

	prepareUpdateFn = func() (bool, error) { return true, nil }
	// exec fails → falls back to exit
	updaterExecFn = func(string, []string, []string) error { return errors.New("exec disabled in test") }

	mgr, stopped := newTestMgr()
	exited := make(chan int, 1)
	updaterExitFn = func(code int) { exited <- code }

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

func TestCheckForUpdatesExecSuccessSkipsExit(t *testing.T) {

	prevPrepare := prepareUpdateFn
	prevExit := updaterExitFn
	prevExec := updaterExecFn
	t.Cleanup(func() {
		prepareUpdateFn = prevPrepare
		updaterExitFn = prevExit
		updaterExecFn = prevExec
	})

	prepareUpdateFn = func() (bool, error) { return true, nil }

	execCalled := make(chan struct{}, 1)
	// exec succeeds: signal and block (simulates process replacement)
	updaterExecFn = func(string, []string, []string) error {
		execCalled <- struct{}{}
		// In real execve this never returns; return nil to simulate success path
		return nil
	}

	exited := make(chan int, 1)
	updaterExitFn = func(code int) { exited <- code }

	mgr, _ := newTestMgr()

	go CheckForUpdates(mgr)

	select {
	case <-execCalled:
	case <-time.After(time.Second):
		t.Fatal("updaterExecFn should have been called")
	}

	// exit should NOT be called when exec returns nil
	select {
	case code := <-exited:
		t.Fatalf("updaterExitFn should not be called after successful exec, got code %d", code)
	case <-time.After(100 * time.Millisecond):
		// expected: no exit
	}
}
