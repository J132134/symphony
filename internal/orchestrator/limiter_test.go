package orchestrator

import "testing"

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
