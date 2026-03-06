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

func TestSessionLimiterUrgentBypassesLimitAndBlocksNonUrgent(t *testing.T) {
	t.Parallel()

	limiter := NewSessionLimiter(1)

	if !limiter.TryAcquireIssue("issue-1", false, nil) {
		t.Fatal("non-urgent acquire should succeed")
	}
	if !limiter.ForceAcquireIssue("issue-urgent", true, nil) {
		t.Fatal("urgent acquire should bypass the hard limit")
	}
	if !limiter.HasUrgent() {
		t.Fatal("limiter should report an in-flight urgent issue")
	}
	if limiter.TryAcquireIssue("issue-2", false, nil) {
		t.Fatal("non-urgent acquire should be blocked while urgent is running")
	}

	limiter.ReleaseIssue("issue-urgent")

	if limiter.HasUrgent() {
		t.Fatal("urgent flag should clear after release")
	}
	if limiter.TryAcquireIssue("issue-2", false, nil) {
		t.Fatal("non-urgent acquire should still wait until the limit drops below capacity")
	}

	limiter.ReleaseIssue("issue-1")

	if !limiter.TryAcquireIssue("issue-2", false, nil) {
		t.Fatal("non-urgent acquire should succeed once urgent and previous work are released")
	}
}

func TestSessionLimiterPreemptNonUrgent(t *testing.T) {
	t.Parallel()

	limiter := NewSessionLimiter(3)
	preempted := map[string]int{}

	if !limiter.TryAcquireIssue("issue-1", false, func() { preempted["issue-1"]++ }) {
		t.Fatal("issue-1 acquire should succeed")
	}
	if !limiter.TryAcquireIssue("issue-2", false, func() { preempted["issue-2"]++ }) {
		t.Fatal("issue-2 acquire should succeed")
	}
	if !limiter.ForceAcquireIssue("issue-urgent", true, nil) {
		t.Fatal("urgent acquire should succeed")
	}

	got := limiter.PreemptNonUrgent("issue-2")
	if len(got) != 1 || got[0] != "issue-1" {
		t.Fatalf("PreemptNonUrgent() = %v, want [issue-1]", got)
	}
	if preempted["issue-1"] != 1 {
		t.Fatalf("issue-1 callback count = %d, want 1", preempted["issue-1"])
	}
	if preempted["issue-2"] != 0 {
		t.Fatalf("issue-2 callback count = %d, want 0", preempted["issue-2"])
	}
}
