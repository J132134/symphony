package daemon

import (
	"testing"
	"time"

	"symphony/internal/config"
	"symphony/internal/orchestrator"
)

func TestRequestRestartWhenIdleWaitsForRunningWork(t *testing.T) {
	t.Parallel()

	mgr := &Manager{}
	orch := orchestrator.New("", 0, "alpha", nil)
	st := orch.GetState()
	st.Lock()
	st.Running["issue-1"] = &orchestrator.RunAttempt{IssueID: "issue-1", Identifier: "J-18"}
	st.Unlock()

	runner := &projectRunner{
		proj: config.ProjectConfig{Name: "alpha"},
		orch: orch,
	}
	mgr.runners = []*projectRunner{runner}

	ready := mgr.RequestRestartWhenIdle()
	if ready != mgr.RequestRestartWhenIdle() {
		t.Fatal("restart requests should reuse the same ready channel")
	}

	select {
	case <-ready:
		t.Fatal("restart should wait until running work finishes")
	default:
	}

	st.Lock()
	if !st.Draining {
		st.Unlock()
		t.Fatal("orchestrator should enter draining mode")
	}
	delete(st.Running, "issue-1")
	st.Unlock()

	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for restart readiness")
	}
}
