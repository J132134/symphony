package daemon

import (
	"errors"
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

func TestManagerGetSummaryIncludesRunnerFailures(t *testing.T) {
	t.Parallel()

	running := orchestrator.New("", 0, "alpha", nil)
	runningState := running.GetState()
	now := time.Date(2026, 3, 6, 14, 0, 0, 0, time.UTC)
	runningState.RecordTrackerSuccess(now)
	runningState.Running["1"] = &orchestrator.RunAttempt{Identifier: "J-17"}

	networkLost := orchestrator.New("", 0, "beta", nil)
	networkState := networkLost.GetState()
	networkState.RecordTrackerFailure(now.Add(time.Minute), errors.New("dial tcp timeout"))

	mgr := &Manager{
		cfg: &config.DaemonConfig{},
		runners: []*projectRunner{
			{proj: config.ProjectConfig{Name: "alpha"}, orch: running},
			{proj: config.ProjectConfig{Name: "beta"}, orch: networkLost},
			{proj: config.ProjectConfig{Name: "gamma"}, lastErr: "workflow load failed"},
		},
	}

	summary := mgr.GetSummary()
	if summary.Status != "error" {
		t.Fatalf("status = %q, want error", summary.Status)
	}
	if summary.ProjectCount != 3 {
		t.Fatalf("project_count = %d, want 3", summary.ProjectCount)
	}
	if summary.SubprocessCount != 1 {
		t.Fatalf("subprocess_count = %d, want 1", summary.SubprocessCount)
	}
	if len(summary.RunningIssueIDs) != 1 || summary.RunningIssueIDs[0] != "J-17" {
		t.Fatalf("running_issue_ids = %#v, want [J-17]", summary.RunningIssueIDs)
	}
	if summary.Projects[2].Name != "gamma" || summary.Projects[2].Status != "error" {
		t.Fatalf("unexpected gamma project summary: %+v", summary.Projects[2])
	}
	if summary.Projects[2].LastError != "workflow load failed" {
		t.Fatalf("last_error = %q, want workflow load failed", summary.Projects[2].LastError)
	}
}
