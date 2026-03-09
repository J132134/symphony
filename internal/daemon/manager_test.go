package daemon

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"symphony/internal/config"
	"symphony/internal/orchestrator"
)

type fakeManagedOrchestrator struct {
	state        *orchestrator.State
	run          func(context.Context) error
	refreshCalls int
}

func (f *fakeManagedOrchestrator) Run(ctx context.Context) error {
	if f.run == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.run(ctx)
}

func (f *fakeManagedOrchestrator) BeginDrain()                   {}
func (f *fakeManagedOrchestrator) DrainAndStop()                 {}
func (f *fakeManagedOrchestrator) IsIdle() bool                  { return true }
func (f *fakeManagedOrchestrator) GetState() *orchestrator.State { return f.state }
func (f *fakeManagedOrchestrator) TriggerRefresh(context.Context) {
	f.refreshCalls++
}

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
	lastEvent := now.Add(time.Minute)
	attempt := &orchestrator.RunAttempt{Identifier: "J-17", StartedAt: now}
	attempt.SetStatus(orchestrator.StatusStreamingTurn)
	attempt.SetTurnCount(2)
	attempt.UpdateLastEvent(lastEvent)
	runningState.Running["1"] = attempt

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
	if len(summary.RunningIssueIDs) != 1 || summary.RunningIssueIDs[0] != "J-17" {
		t.Fatalf("running_issue_ids = %#v, want [J-17]", summary.RunningIssueIDs)
	}
	if got := summary.Projects[0].RunningIssues; len(got) != 1 {
		t.Fatalf("running_issues len = %d, want 1", len(got))
	} else {
		if got[0].Status != string(orchestrator.StatusStreamingTurn) {
			t.Fatalf("status = %q, want %q", got[0].Status, orchestrator.StatusStreamingTurn)
		}
		if got[0].TurnCount != 2 {
			t.Fatalf("turn_count = %d, want 2", got[0].TurnCount)
		}
		if got[0].LastEventAt != lastEvent.Format(time.RFC3339) {
			t.Fatalf("last_event_at = %q, want %q", got[0].LastEventAt, lastEvent.Format(time.RFC3339))
		}
	}
	if summary.Projects[2].Name != "gamma" || summary.Projects[2].Status != "error" {
		t.Fatalf("unexpected gamma project summary: %+v", summary.Projects[2])
	}
	if summary.Projects[2].LastError != "workflow load failed" {
		t.Fatalf("last_error = %q, want workflow load failed", summary.Projects[2].LastError)
	}
}

func TestManagerGetProjectsIgnoresNonFailureRetriesForErrorStatus(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		kind orchestrator.RetryKind
	}{
		{name: "capacity", kind: orchestrator.RetryKindCapacity},
		{name: "continuation", kind: orchestrator.RetryKindContinuation},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			orch := orchestrator.New("", 0, "alpha", nil)
			st := orch.GetState()
			st.RecordTrackerSuccess(now)
			st.Lock()
			st.RetryQueue["issue-1"] = &orchestrator.RetryEntry{
				Identifier: "J-18",
				Kind:       tc.kind,
				Error:      "waiting",
			}
			st.Unlock()

			mgr := &Manager{
				cfg: &config.DaemonConfig{},
				runners: map[string]*projectRunner{
					"alpha": {proj: config.ProjectConfig{Name: "alpha"}, orch: orch},
				},
			}

			projects := mgr.GetProjects()
			if len(projects) != 1 {
				t.Fatalf("project count = %d, want 1", len(projects))
			}
			if projects[0].Status != "idle" {
				t.Fatalf("status = %q, want idle", projects[0].Status)
			}
			if projects[0].RetryCount != 1 {
				t.Fatalf("retry_count = %d, want 1", projects[0].RetryCount)
			}
		})
	}
}

func TestProjectRunnerQuarantinesAfterRestartBudget(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 3, 6, 14, 0, 0, 0, time.UTC)
	step := 0
	pr := newProjectRunner(
		config.ProjectConfig{Name: "alpha", Workflow: "/tmp/alpha"},
		nil,
		config.ProjectHealthConfig{
			RestartBudgetCount:         3,
			RestartBudgetWindowMinutes: 15,
			ProbeIntervalSeconds:       11,
		},
	)
	pr.now = func() time.Time {
		ts := base.Add(time.Duration(step) * time.Second)
		step++
		return ts
	}
	pr.after = func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		if d < 11*time.Second {
			ch <- base.Add(d)
		}
		return ch
	}
	pr.newOrch = func(config.ProjectConfig, *orchestrator.SessionLimiter) managedOrchestrator {
		return &fakeManagedOrchestrator{
			state: orchestrator.NewState(),
			run: func(context.Context) error {
				return errors.New("boom")
			},
		}
	}
	pr.probe = func(context.Context, config.ProjectConfig) error {
		return errors.New("still broken")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		pr.run(ctx)
	}()

	waitFor(t, func() bool {
		snapshot := pr.projectSnapshot()
		return snapshot.Health == "quarantined" && snapshot.CrashCount == 3 && snapshot.QuarantinedAt != ""
	})

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner shutdown")
	}
}

func TestProjectRunnerProbeRecoversAndResetsCrashBudget(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 3, 6, 14, 0, 0, 0, time.UTC)
	step := 0
	starts := 0

	pr := newProjectRunner(
		config.ProjectConfig{Name: "alpha", Workflow: "/tmp/alpha"},
		nil,
		config.ProjectHealthConfig{
			RestartBudgetCount:         3,
			RestartBudgetWindowMinutes: 15,
			ProbeIntervalSeconds:       11,
		},
	)
	pr.now = func() time.Time {
		ts := base.Add(time.Duration(step) * time.Second)
		step++
		return ts
	}
	pr.after = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- base
		return ch
	}
	pr.newOrch = func(config.ProjectConfig, *orchestrator.SessionLimiter) managedOrchestrator {
		starts++
		if starts <= 3 {
			return &fakeManagedOrchestrator{
				state: orchestrator.NewState(),
				run: func(context.Context) error {
					return errors.New("boom")
				},
			}
		}
		return &fakeManagedOrchestrator{
			state: orchestrator.NewState(),
			run: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
		}
	}
	probeCalls := 0
	pr.probe = func(context.Context, config.ProjectConfig) error {
		probeCalls++
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		pr.run(ctx)
	}()

	waitFor(t, func() bool {
		snapshot := pr.projectSnapshot()
		return starts >= 4 && probeCalls >= 1 && snapshot.Health == "healthy" && snapshot.CrashCount == 0
	})

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner shutdown")
	}
}

func TestManagerGetSummaryMarksQuarantinedProjects(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 6, 14, 0, 0, 0, time.UTC)
	mgr := &Manager{
		cfg: &config.DaemonConfig{},
		runners: map[string]*projectRunner{
			"alpha": {
				proj:          config.ProjectConfig{Name: "alpha"},
				healthCfg:     config.ProjectHealthConfig{RestartBudgetWindowMinutes: 15},
				healthState:   runnerHealthQuarantined,
				crashTimes:    []time.Time{now.Add(-time.Minute), now},
				quarantinedAt: &now,
				lastErr:       "tracker auth failed",
				now:           func() time.Time { return now },
			},
		},
	}

	summary := mgr.GetSummary()
	if summary.Status != "quarantined" {
		t.Fatalf("status = %q, want quarantined", summary.Status)
	}
	if summary.Projects[0].Health != "quarantined" {
		t.Fatalf("health = %q, want quarantined", summary.Projects[0].Health)
	}
	if summary.Projects[0].CrashCount != 2 {
		t.Fatalf("crash_count = %d, want 2", summary.Projects[0].CrashCount)
	}
	if summary.Projects[0].QuarantinedAt == "" {
		t.Fatal("quarantined_at = empty, want timestamp")
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

func TestManagerWaitBlocksUntilRunStops(t *testing.T) {
	t.Parallel()

	mgr := NewManager(&config.DaemonConfig{
		Projects: []config.ProjectConfig{},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		mgr.Run(ctx)
	}()

	waitForManager(t, func() bool {
		mgr.mu.RLock()
		defer mgr.mu.RUnlock()
		return mgr.done != nil
	})

	waited := make(chan struct{})
	go func() {
		defer close(waited)
		mgr.Wait()
	}()

	select {
	case <-waited:
		t.Fatal("Wait should block before shutdown")
	case <-time.After(100 * time.Millisecond):
	}

	cancel()

	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for manager.Wait")
	}

	<-done
}

func TestTriggerRefreshForIssueRefreshesMatchingProjectOnly(t *testing.T) {
	t.Parallel()

	alphaOrch := &fakeManagedOrchestrator{state: orchestrator.NewState()}
	betaOrch := &fakeManagedOrchestrator{state: orchestrator.NewState()}

	mgr := &Manager{
		cfg: &config.DaemonConfig{},
		runners: map[string]*projectRunner{
			"alpha": {proj: config.ProjectConfig{Name: "alpha", Workflow: "/tmp/alpha"}, orch: alphaOrch},
			"beta":  {proj: config.ProjectConfig{Name: "beta", Workflow: "/tmp/beta"}, orch: betaOrch},
		},
		matchProjectIssue: func(_ context.Context, proj config.ProjectConfig, stateName, issueID string) (bool, error) {
			if issueID != "issue-123" || stateName != "In Progress" {
				return false, nil
			}
			return proj.Name == "beta", nil
		},
	}

	handled := mgr.TriggerRefreshForIssue(context.Background(), "issue-123", "In Progress")
	if !handled {
		t.Fatal("TriggerRefreshForIssue() = false, want true")
	}
	if alphaOrch.refreshCalls != 0 {
		t.Fatalf("alpha refresh_calls = %d, want 0", alphaOrch.refreshCalls)
	}
	if betaOrch.refreshCalls != 1 {
		t.Fatalf("beta refresh_calls = %d, want 1", betaOrch.refreshCalls)
	}
}

func closedDone() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func waitForManager(t *testing.T, check func() bool) {
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
