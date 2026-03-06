package status

import (
	"errors"
	"testing"
	"time"

	"symphony/internal/orchestrator"
)

func TestBuildSummaryPrefersNetworkLoss(t *testing.T) {
	now := time.Date(2026, 3, 6, 14, 0, 0, 0, time.UTC)

	running := orchestrator.NewState()
	running.RecordTrackerSuccess(now)
	running.Running["1"] = &orchestrator.RunAttempt{Identifier: "J-17"}

	networkLost := orchestrator.NewState()
	networkLost.RecordTrackerFailure(now.Add(time.Minute), errors.New("dial tcp timeout"))

	summary := BuildSummary(map[string]*orchestrator.State{
		"alpha": running,
		"beta":  networkLost,
	})

	if summary.Status != "network_lost" {
		t.Fatalf("status = %q, want network_lost", summary.Status)
	}
	if summary.SubprocessCount != 1 {
		t.Fatalf("subprocess_count = %d, want 1", summary.SubprocessCount)
	}
	if len(summary.RunningIssueIDs) != 1 || summary.RunningIssueIDs[0] != "J-17" {
		t.Fatalf("running_issue_ids = %#v, want [J-17]", summary.RunningIssueIDs)
	}
}

func TestBuildSummaryMarksRetryAsError(t *testing.T) {
	now := time.Date(2026, 3, 6, 14, 0, 0, 0, time.UTC)

	st := orchestrator.NewState()
	st.RecordTrackerSuccess(now)
	st.RetryQueue["1"] = &orchestrator.RetryEntry{Identifier: "J-18", Error: "agent crashed"}

	summary := BuildSummary(map[string]*orchestrator.State{"alpha": st})
	if summary.Status != "error" {
		t.Fatalf("status = %q, want error", summary.Status)
	}
	if summary.RetryCount != 1 {
		t.Fatalf("retry_count = %d, want 1", summary.RetryCount)
	}
}

func TestBuildSummaryPrefersErrorOverNetworkLoss(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 6, 14, 0, 0, 0, time.UTC)

	networkLost := orchestrator.NewState()
	networkLost.RecordTrackerFailure(now, errors.New("dial tcp timeout"))

	retrying := orchestrator.NewState()
	retrying.RecordTrackerSuccess(now.Add(time.Minute))
	retrying.RetryQueue["1"] = &orchestrator.RetryEntry{Identifier: "J-18", Error: "agent crashed"}

	summary := BuildSummary(map[string]*orchestrator.State{
		"alpha": networkLost,
		"beta":  retrying,
	})

	if summary.Status != "error" {
		t.Fatalf("status = %q, want error", summary.Status)
	}
}

func TestBuildSummaryIgnoresAbandonedIssuesInRetryCount(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 6, 14, 0, 0, 0, time.UTC)

	st := orchestrator.NewState()
	st.RecordTrackerSuccess(now)
	st.Abandoned["1"] = &orchestrator.AbandonedEntry{
		Identifier:   "J-27",
		State:        "In Progress",
		FailureCount: 3,
		AbandonedAt:  now,
	}

	summary := BuildSummary(map[string]*orchestrator.State{"alpha": st})
	if summary.RetryCount != 0 {
		t.Fatalf("retry_count = %d, want 0", summary.RetryCount)
	}
	if summary.Status != "idle" {
		t.Fatalf("status = %q, want idle", summary.Status)
	}
}

func TestBuildSummaryMarksPausedProject(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()

	st := orchestrator.NewState()
	st.RecordTrackerSuccess(now)
	pausedUntil := now.Add(2 * time.Minute)
	st.PausedUntil = &pausedUntil
	st.PauseReason = "rate_limit_reset"

	summary := BuildSummary(map[string]*orchestrator.State{"alpha": st})
	if summary.Status != "paused" {
		t.Fatalf("status = %q, want paused", summary.Status)
	}
	if len(summary.Projects) != 1 {
		t.Fatalf("projects len = %d, want 1", len(summary.Projects))
	}
	if !summary.Projects[0].Paused {
		t.Fatal("project should be marked paused")
	}
	if summary.Projects[0].Status != "paused" {
		t.Fatalf("project status = %q, want paused", summary.Projects[0].Status)
	}
	if summary.Projects[0].PauseReason != "rate_limit_reset" {
		t.Fatalf("pause_reason = %q, want rate_limit_reset", summary.Projects[0].PauseReason)
	}
	if summary.Projects[0].PausedUntil == "" {
		t.Fatal("paused_until should be set")
	}
}
