package daemon

import (
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
