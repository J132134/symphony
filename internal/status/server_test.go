package status

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"symphony/internal/orchestrator"
)

type fakeSummarySource struct {
	summary      Summary
	projects     []ProjectSummary
	states       map[string]*orchestrator.State
	refreshCalls int
}

func (f *fakeSummarySource) GetAllStates() map[string]*orchestrator.State {
	if f.states != nil {
		return f.states
	}
	return map[string]*orchestrator.State{}
}

func (f *fakeSummarySource) GetSummary() Summary {
	return f.summary
}

func (f *fakeSummarySource) GetProjects() []ProjectSummary {
	return f.projects
}

func (f *fakeSummarySource) TriggerRefresh(context.Context) {
	f.refreshCalls++
}

func TestHandleSummaryPrefersSourceSummary(t *testing.T) {
	t.Parallel()

	server := New(&fakeSummarySource{
		summary: Summary{
			Status:          "error",
			SubprocessCount: 2,
		},
	}, 7777)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/summary", nil)
	rec := httptest.NewRecorder()
	server.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}

	var summary Summary
	if err := json.NewDecoder(rec.Body).Decode(&summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary.Status != "error" {
		t.Fatalf("summary status = %q, want error", summary.Status)
	}
	if summary.SubprocessCount != 2 {
		t.Fatalf("summary subprocess_count = %d, want 2", summary.SubprocessCount)
	}
}

func TestHandleRefreshTriggersSource(t *testing.T) {
	t.Parallel()

	source := &fakeSummarySource{}
	server := New(source, 7777)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/refresh", nil)
	rec := httptest.NewRecorder()
	server.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status code = %d, want 202", rec.Code)
	}
	if source.refreshCalls != 1 {
		t.Fatalf("refresh_calls = %d, want 1", source.refreshCalls)
	}
}

func TestHandleProjectsPrefersProjectSource(t *testing.T) {
	t.Parallel()

	server := New(&fakeSummarySource{
		projects: []ProjectSummary{{
			Name:          "alpha",
			Status:        "quarantined",
			Health:        "quarantined",
			CrashCount:    3,
			QuarantinedAt: "2026-03-06T14:00:00Z",
		}},
	}, 7777)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	rec := httptest.NewRecorder()
	server.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}

	var payload []ProjectSummary
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode projects: %v", err)
	}
	if len(payload) != 1 {
		t.Fatalf("projects len = %d, want 1", len(payload))
	}
	if payload[0].Health != "quarantined" {
		t.Fatalf("health = %q, want quarantined", payload[0].Health)
	}
	if payload[0].CrashCount != 3 {
		t.Fatalf("crash_count = %d, want 3", payload[0].CrashCount)
	}
}

func TestHandleProjectsBuildsProjectSummaryFromStates(t *testing.T) {
	t.Parallel()

	state := orchestrator.NewState()
	now := time.Date(2026, 3, 9, 1, 0, 0, 0, time.UTC)
	state.RecordTrackerSuccess(now)
	attempt := &orchestrator.RunAttempt{
		Identifier: "J-54",
		StartedAt:  now,
	}
	attempt.SetStatus(orchestrator.StatusStreamingTurn)
	attempt.SetTurnCount(2)
	attempt.UpdateLastEvent(now.Add(time.Minute))
	attempt.SetCurrentTask(now.Add(30*time.Second), "running tool: apply_patch")
	attempt.SetServerMessage(now.Add(45*time.Second), "diff stream stalled")
	state.Running["run-1"] = attempt

	server := New(&fakeSummarySource{
		states: map[string]*orchestrator.State{"alpha": state},
	}, 7777)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	rec := httptest.NewRecorder()
	server.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}

	var payload []ProjectSummary
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode projects: %v", err)
	}
	if len(payload) != 1 {
		t.Fatalf("projects len = %d, want 1", len(payload))
	}
	if payload[0].SubprocessCount != 1 {
		t.Fatalf("subprocess_count = %d, want 1", payload[0].SubprocessCount)
	}
	if len(payload[0].RunningIssues) != 1 || payload[0].RunningIssues[0].Identifier != "J-54" {
		t.Fatalf("running_issues = %#v, want J-54 detail", payload[0].RunningIssues)
	}
	if payload[0].RunningIssues[0].CurrentTask != "running tool: apply_patch" {
		t.Fatalf("current_task = %q, want running tool: apply_patch", payload[0].RunningIssues[0].CurrentTask)
	}
	if payload[0].RunningIssues[0].ServerMessage != "diff stream stalled" {
		t.Fatalf("server_message = %q, want diff stream stalled", payload[0].RunningIssues[0].ServerMessage)
	}
}
