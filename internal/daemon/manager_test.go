package daemon

import (
	"errors"
	"slices"
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
	mgr.runners = map[string]*projectRunner{"alpha": runner}

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
		runners: map[string]*projectRunner{
			"alpha": {proj: config.ProjectConfig{Name: "alpha"}, orch: running},
			"beta":  {proj: config.ProjectConfig{Name: "beta"}, orch: networkLost},
			"gamma": {proj: config.ProjectConfig{Name: "gamma"}, lastErr: "workflow load failed"},
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
	if summary.FailureRetryCount != 0 {
		t.Fatalf("failure_retry_count = %d, want 0", summary.FailureRetryCount)
	}
	if summary.CapacityWaitCount != 0 {
		t.Fatalf("capacity_wait_count = %d, want 0", summary.CapacityWaitCount)
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

func TestManagerGetSummaryTreatsCapacityRetryAsRunning(t *testing.T) {
	t.Parallel()

	capacityOnly := orchestrator.New("", 0, "alpha", nil)
	st := capacityOnly.GetState()
	now := time.Date(2026, 3, 6, 14, 0, 0, 0, time.UTC)
	st.RecordTrackerSuccess(now)
	st.RetryQueue["1"] = &orchestrator.RetryEntry{
		Identifier:   "J-21",
		Kind:         orchestrator.RetryKindCapacity,
		Attempt:      2,
		FailureCount: 1,
		DeferCount:   3,
		Error:        "no slots",
	}

	mgr := &Manager{
		cfg: &config.DaemonConfig{},
		runners: map[string]*projectRunner{
			"alpha": {proj: config.ProjectConfig{Name: "alpha"}, orch: capacityOnly},
		},
	}

	summary := mgr.GetSummary()
	if summary.Status != "running" {
		t.Fatalf("status = %q, want running", summary.Status)
	}
	if summary.RetryCount != 1 {
		t.Fatalf("retry_count = %d, want 1", summary.RetryCount)
	}
	if summary.FailureRetryCount != 0 {
		t.Fatalf("failure_retry_count = %d, want 0", summary.FailureRetryCount)
	}
	if summary.CapacityWaitCount != 1 {
		t.Fatalf("capacity_wait_count = %d, want 1", summary.CapacityWaitCount)
	}
	if len(summary.Projects) != 1 {
		t.Fatalf("project count = %d, want 1", len(summary.Projects))
	}
	if summary.Projects[0].Status != "running" {
		t.Fatalf("project status = %q, want running", summary.Projects[0].Status)
	}
	if summary.Projects[0].FailureRetryCount != 0 {
		t.Fatalf("project failure_retry_count = %d, want 0", summary.Projects[0].FailureRetryCount)
	}
	if summary.Projects[0].CapacityWaitCount != 1 {
		t.Fatalf("project capacity_wait_count = %d, want 1", summary.Projects[0].CapacityWaitCount)
	}
	if summary.Projects[0].LastError != "" {
		t.Fatalf("last_error = %q, want empty", summary.Projects[0].LastError)
	}
}

func TestManagerApplyConfigReconcilesProjectDiff(t *testing.T) {
	t.Parallel()

	alpha := config.ProjectConfig{Name: "alpha", Workflow: "/tmp/alpha"}
	betaOld := config.ProjectConfig{Name: "beta", Workflow: "/tmp/beta-old"}
	betaNew := config.ProjectConfig{Name: "beta", Workflow: "/tmp/beta-new"}
	gamma := config.ProjectConfig{Name: "gamma", Workflow: "/tmp/gamma"}

	stopCounts := map[string]int{}
	mgr := &Manager{
		cfg: &config.DaemonConfig{Projects: []config.ProjectConfig{alpha, betaOld}},
		runners: map[string]*projectRunner{
			"alpha": {
				proj:   alpha,
				cancel: func() { stopCounts["alpha"]++ },
				done:   closedDone(),
			},
			"beta": {
				proj:   betaOld,
				cancel: func() { stopCounts["beta"]++ },
				done:   closedDone(),
			},
		},
	}

	oldBetaRunner := mgr.runners["beta"]

	mgr.ApplyConfig(&config.DaemonConfig{
		Projects: []config.ProjectConfig{betaNew, gamma},
	})

	if stopCounts["alpha"] != 1 {
		t.Fatalf("alpha stop count = %d, want 1", stopCounts["alpha"])
	}
	if stopCounts["beta"] != 1 {
		t.Fatalf("beta stop count = %d, want 1", stopCounts["beta"])
	}
	if _, ok := mgr.runners["alpha"]; ok {
		t.Fatal("alpha should be removed")
	}
	if got := mgr.runners["beta"].proj; !projectConfigEqual(got, betaNew) {
		t.Fatalf("beta config = %+v, want %+v", got, betaNew)
	}
	if got := mgr.runners["gamma"].proj; !projectConfigEqual(got, gamma) {
		t.Fatalf("gamma config = %+v, want %+v", got, gamma)
	}
	if mgr.runners["beta"] == oldBetaRunner {
		t.Fatal("updated beta runner should be replaced")
	}
}

func TestManagerApplyConfigIgnoresProjectOrderOnlyChanges(t *testing.T) {
	t.Parallel()

	alpha := config.ProjectConfig{Name: "alpha", Workflow: "/tmp/alpha"}
	beta := config.ProjectConfig{Name: "beta", Workflow: "/tmp/beta"}

	stopCounts := map[string]int{}
	mgr := &Manager{
		cfg: &config.DaemonConfig{Projects: []config.ProjectConfig{alpha, beta}},
		runners: map[string]*projectRunner{
			"alpha": {
				proj:   alpha,
				cancel: func() { stopCounts["alpha"]++ },
				done:   closedDone(),
			},
			"beta": {
				proj:   beta,
				cancel: func() { stopCounts["beta"]++ },
				done:   closedDone(),
			},
		},
	}

	before := []*projectRunner{mgr.runners["alpha"], mgr.runners["beta"]}

	mgr.ApplyConfig(&config.DaemonConfig{
		Projects: []config.ProjectConfig{beta, alpha},
	})

	if stopCounts["alpha"] != 0 || stopCounts["beta"] != 0 {
		t.Fatalf("order-only change should not stop runners, got %+v", stopCounts)
	}
	after := []*projectRunner{mgr.runners["alpha"], mgr.runners["beta"]}
	if !slices.Equal(before, after) {
		t.Fatal("order-only change should keep existing runners")
	}
}

func closedDone() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
