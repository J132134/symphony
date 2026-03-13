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
	lastEvent := now.Add(2 * time.Minute)
	attempt := &orchestrator.RunAttempt{
		Identifier: "J-17",
		StartedAt:  now,
	}
	attempt.SetStatus(orchestrator.StatusStreamingTurn)
	attempt.SetTurnCount(3)
	attempt.UpdateLastEvent(lastEvent)
	attempt.SetSessionIdentity("thread-1", "session-1234567890", "4321")
	attempt.AddTokens(100, 20, 120)
	attempt.SetLastEventDetail("item.completed", "done")
	attempt.RecordEvent(lastEvent.Add(-time.Minute), "tool_call", "linear_graphql {\"query\":\"issue(id:J-17)\"}")
	attempt.RecordEvent(lastEvent, "approval_request", "Item CommandExecution RequestApproval: {\"command\":\"git status\"}")
	attempt.RecordEvent(lastEvent.Add(time.Second), "token_usage", "")
	running.Running["1"] = attempt
	running.Totals.InputTokens = 100
	running.Totals.OutputTokens = 20
	running.Totals.TotalTokens = 120

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
	if summary.TotalTokens != 120 {
		t.Fatalf("total_tokens = %d, want 120", summary.TotalTokens)
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
		if got[0].TurnCount != 3 {
			t.Fatalf("turn_count = %d, want 3", got[0].TurnCount)
		}
		if got[0].LastEventAt != lastEvent.Format(time.RFC3339) {
			t.Fatalf("last_event_at = %q, want %q", got[0].LastEventAt, lastEvent.Format(time.RFC3339))
		}
		if got[0].LastEvent != "Approval Request: Item CommandExecution RequestApproval: {\"command\":\"git status\"}" {
			t.Fatalf("last_event = %q, want approval request detail", got[0].LastEvent)
		}
		if got[0].CurrentActivity != "Approval Request: Item CommandExecution RequestApproval: {\"command\":\"git status\"}" {
			t.Fatalf("current_activity = %q, want approval request detail", got[0].CurrentActivity)
		}
		if len(got[0].RecentEvents) != 2 {
			t.Fatalf("recent_events len = %d, want 2", len(got[0].RecentEvents))
		}
		if got[0].RecentEvents[0].Detail != "Tool Call: linear_graphql {\"query\":\"issue(id:J-17)\"}" {
			t.Fatalf("recent_events[0].detail = %q, want tool call detail", got[0].RecentEvents[0].Detail)
		}
		if got[0].RecentEvents[1].Detail != "Approval Request: Item CommandExecution RequestApproval: {\"command\":\"git status\"}" {
			t.Fatalf("recent_events[1].detail = %q, want approval request detail", got[0].RecentEvents[1].Detail)
		}
		if got[0].SessionID != "session-1234567890" {
			t.Fatalf("session_id = %q, want session-1234567890", got[0].SessionID)
		}
		if got[0].TotalTokens != 120 {
			t.Fatalf("total_tokens = %d, want 120", got[0].TotalTokens)
		}
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
}

func TestSummarizeRunningIssueKeepsCurrentActivityOnBookkeepingEvents(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 6, 14, 0, 0, 0, time.UTC)
	attempt := &orchestrator.RunAttempt{
		Identifier: "J-19",
		StartedAt:  now,
	}
	attempt.SetStatus(orchestrator.StatusStreamingTurn)
	attempt.RecordEvent(now.Add(time.Minute), "tool_call", "apply_patch {\"path\":\"cmd/symphony/status.go\"}")
	attempt.RecordEvent(now.Add(2*time.Minute), "approval_granted", "Command Execution")
	attempt.RecordEvent(now.Add(3*time.Minute), "server_notification", "Item Completed")

	summary := SummarizeRunningIssue(attempt)
	if summary.CurrentActivity != "Tool Call: apply_patch {\"path\":\"cmd/symphony/status.go\"}" {
		t.Fatalf("current_activity = %q, want tool call detail", summary.CurrentActivity)
	}
	if summary.LastEvent != "Server Notification: Item Completed" {
		t.Fatalf("last_event = %q, want server notification detail", summary.LastEvent)
	}
	if len(summary.RecentEvents) != 3 {
		t.Fatalf("recent_events len = %d, want 3", len(summary.RecentEvents))
	}
}

func TestBuildSummaryIgnoresNonFailureRetriesForErrorStatus(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 6, 14, 0, 0, 0, time.UTC)

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

			st := orchestrator.NewState()
			st.RecordTrackerSuccess(now)
			st.RetryQueue["1"] = &orchestrator.RetryEntry{
				Identifier: "J-18",
				Kind:       tc.kind,
				DueAt:      now.Add(time.Minute),
				Error:      "transient wait",
			}

			summary := BuildSummary(map[string]*orchestrator.State{"alpha": st})
			if summary.Status != "idle" {
				t.Fatalf("status = %q, want idle", summary.Status)
			}
			if summary.RetryCount != 1 {
				t.Fatalf("retry_count = %d, want 1", summary.RetryCount)
			}
		})
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

func TestBuildSummaryFromProjectsPrefersQuarantined(t *testing.T) {
	t.Parallel()

	summary := BuildSummaryFromProjects([]ProjectSummary{
		{Name: "alpha", Status: "running", Health: "healthy", SubprocessCount: 1},
		{Name: "beta", Status: "quarantined", Health: "quarantined", CrashCount: 3},
	})

	if summary.Status != "quarantined" {
		t.Fatalf("status = %q, want quarantined", summary.Status)
	}
	if summary.ProjectCount != 2 {
		t.Fatalf("project_count = %d, want 2", summary.ProjectCount)
	}
	if summary.Projects[1].Health != "quarantined" {
		t.Fatalf("health = %q, want quarantined", summary.Projects[1].Health)
	}
}
