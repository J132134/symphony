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
	st.RetryQueue["1"] = &orchestrator.RetryEntry{
		Identifier: "J-18",
		Kind:       orchestrator.RetryKindFailure,
		Error:      "agent crashed",
	}

	summary := BuildSummary(map[string]*orchestrator.State{"alpha": st})
	if summary.Status != "error" {
		t.Fatalf("status = %q, want error", summary.Status)
	}
	if summary.RetryCount != 1 {
		t.Fatalf("retry_count = %d, want 1", summary.RetryCount)
	}
	if summary.FailureRetryCount != 1 {
		t.Fatalf("failure_retry_count = %d, want 1", summary.FailureRetryCount)
	}
	if summary.CapacityWaitCount != 0 {
		t.Fatalf("capacity_wait_count = %d, want 0", summary.CapacityWaitCount)
	}
	if len(summary.Projects) != 1 || summary.Projects[0].LastError != "agent crashed" {
		t.Fatalf("last_error = %q, want agent crashed", summary.Projects[0].LastError)
	}
}

func TestBuildSummaryTreatsCapacityRetryAsRunning(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 6, 14, 0, 0, 0, time.UTC)

	st := orchestrator.NewState()
	st.RecordTrackerSuccess(now)
	st.RetryQueue["1"] = &orchestrator.RetryEntry{
		Identifier:   "J-21",
		Kind:         orchestrator.RetryKindCapacity,
		Attempt:      2,
		FailureCount: 1,
		DeferCount:   3,
		Error:        "no slots",
	}

	summary := BuildSummary(map[string]*orchestrator.State{"alpha": st})
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
	if summary.Projects[0].LastError != "" {
		t.Fatalf("last_error = %q, want empty", summary.Projects[0].LastError)
	}
}

func TestBuildSummaryPrefersErrorOverNetworkLoss(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 6, 14, 0, 0, 0, time.UTC)

	networkLost := orchestrator.NewState()
	networkLost.RecordTrackerFailure(now, errors.New("dial tcp timeout"))

	retrying := orchestrator.NewState()
	retrying.RecordTrackerSuccess(now.Add(time.Minute))
	retrying.RetryQueue["1"] = &orchestrator.RetryEntry{
		Identifier: "J-18",
		Kind:       orchestrator.RetryKindFailure,
		Error:      "agent crashed",
	}

	summary := BuildSummary(map[string]*orchestrator.State{
		"alpha": networkLost,
		"beta":  retrying,
	})

	if summary.Status != "error" {
		t.Fatalf("status = %q, want error", summary.Status)
	}
}

func TestBuildSummaryPrefersFailureRetryOverCapacityWait(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 6, 14, 0, 0, 0, time.UTC)

	failureRetry := orchestrator.NewState()
	failureRetry.RecordTrackerSuccess(now)
	failureRetry.RetryQueue["1"] = &orchestrator.RetryEntry{
		Identifier: "J-18",
		Kind:       orchestrator.RetryKindFailure,
		Error:      "agent crashed",
	}

	capacityWait := orchestrator.NewState()
	capacityWait.RecordTrackerSuccess(now.Add(time.Minute))
	capacityWait.RetryQueue["2"] = &orchestrator.RetryEntry{
		Identifier:   "J-21",
		Kind:         orchestrator.RetryKindCapacity,
		Attempt:      2,
		FailureCount: 1,
		DeferCount:   3,
		Error:        "no slots",
	}

	summary := BuildSummary(map[string]*orchestrator.State{
		"alpha": failureRetry,
		"beta":  capacityWait,
	})

	if summary.Status != "error" {
		t.Fatalf("status = %q, want error", summary.Status)
	}
	if summary.FailureRetryCount != 1 {
		t.Fatalf("failure_retry_count = %d, want 1", summary.FailureRetryCount)
	}
	if summary.CapacityWaitCount != 1 {
		t.Fatalf("capacity_wait_count = %d, want 1", summary.CapacityWaitCount)
	}
	if len(summary.Projects) != 2 {
		t.Fatalf("project count = %d, want 2", len(summary.Projects))
	}
	if summary.Projects[0].Status != "error" {
		t.Fatalf("alpha status = %q, want error", summary.Projects[0].Status)
	}
	if summary.Projects[1].Status != "running" {
		t.Fatalf("beta status = %q, want running", summary.Projects[1].Status)
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
	if summary.FailureRetryCount != 0 {
		t.Fatalf("failure_retry_count = %d, want 0", summary.FailureRetryCount)
	}
	if summary.CapacityWaitCount != 0 {
		t.Fatalf("capacity_wait_count = %d, want 0", summary.CapacityWaitCount)
	}
	if summary.Status != "idle" {
		t.Fatalf("status = %q, want idle", summary.Status)
	}
}
