package orchestrator

import (
	"testing"
	"time"
)

func TestSessionLimiterAcquireRelease(t *testing.T) {
	t.Parallel()

	limiter := NewSessionLimiter(2)

	if !limiter.TryAcquire() {
		t.Fatal("first acquire should succeed")
	}
	if !limiter.TryAcquire() {
		t.Fatal("second acquire should succeed")
	}
	if limiter.TryAcquire() {
		t.Fatal("third acquire should fail at the limit")
	}
	if got := limiter.InUse(); got != 2 {
		t.Fatalf("InUse() = %d, want 2", got)
	}
	if got := limiter.Available(); got != 0 {
		t.Fatalf("Available() = %d, want 0", got)
	}

	limiter.Release()

	if got := limiter.InUse(); got != 1 {
		t.Fatalf("InUse() after release = %d, want 1", got)
	}
	if !limiter.TryAcquire() {
		t.Fatal("acquire after release should succeed")
	}
}

func TestSessionLimiterPauseUntilBlocksNewAcquires(t *testing.T) {
	t.Parallel()

	limiter := NewSessionLimiter(1)
	limiter.PauseUntil(time.Now().UTC().Add(30 * time.Second))

	if _, ok := limiter.PausedUntil(); !ok {
		t.Fatal("expected limiter to report paused state")
	}
	if limiter.TryAcquire() {
		t.Fatal("acquire should fail while limiter is paused")
	}
	if got := limiter.Available(); got != 0 {
		t.Fatalf("Available() while paused = %d, want 0", got)
	}

	limiter.PauseUntil(time.Now().UTC().Add(-time.Second))
	if _, ok := limiter.PausedUntil(); !ok {
		t.Fatal("later pause should not be shortened by an expired timestamp")
	}
}
