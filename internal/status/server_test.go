package status

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"symphony/internal/orchestrator"
)

type fakeSummarySource struct {
	summary      Summary
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

func TestHandleStateOmitsAbandonedIssuesFromRetrying(t *testing.T) {
	t.Parallel()

	st := orchestrator.NewState()
	st.Abandoned["1"] = &orchestrator.AbandonedEntry{
		Identifier:   "J-27",
		State:        "In Progress",
		FailureCount: 4,
	}

	server := New(&fakeSummarySource{
		states: map[string]*orchestrator.State{"alpha": st},
	}, 7777)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	rec := httptest.NewRecorder()
	server.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}

	var payload struct {
		Retrying map[string]any `json:"retrying"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if len(payload.Retrying) != 0 {
		t.Fatalf("retrying = %#v, want empty", payload.Retrying)
	}
}

func TestHandleProjectsOmitsAbandonedIssuesFromRetryingCount(t *testing.T) {
	t.Parallel()

	st := orchestrator.NewState()
	st.Abandoned["1"] = &orchestrator.AbandonedEntry{
		Identifier:   "J-27",
		State:        "In Progress",
		FailureCount: 4,
	}

	server := New(&fakeSummarySource{
		states: map[string]*orchestrator.State{"alpha": st},
	}, 7777)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	rec := httptest.NewRecorder()
	server.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}

	var payload []struct {
		Name     string `json:"name"`
		Retrying int    `json:"retrying"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode projects: %v", err)
	}
	if len(payload) != 1 {
		t.Fatalf("projects len = %d, want 1", len(payload))
	}
	if payload[0].Name != "alpha" {
		t.Fatalf("project name = %q, want alpha", payload[0].Name)
	}
	if payload[0].Retrying != 0 {
		t.Fatalf("retrying = %d, want 0", payload[0].Retrying)
	}
}
