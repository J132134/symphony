package status

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"symphony/internal/orchestrator"
)

type fakeSummarySource struct {
	summary      Summary
	states       map[string]*orchestrator.State
	refreshCalls int
}

type fakeStateSource struct {
	states map[string]*orchestrator.State
}

func (f *fakeSummarySource) GetAllStates() map[string]*orchestrator.State {
	if f.states != nil {
		return f.states
	}
	return map[string]*orchestrator.State{}
}

func (f *fakeStateSource) GetAllStates() map[string]*orchestrator.State {
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
			Status:            "error",
			SubprocessCount:   2,
			RetryCount:        3,
			FailureRetryCount: 2,
			CapacityWaitCount: 1,
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
	if summary.RetryCount != 3 {
		t.Fatalf("summary retry_count = %d, want 3", summary.RetryCount)
	}
	if summary.FailureRetryCount != 2 {
		t.Fatalf("summary failure_retry_count = %d, want 2", summary.FailureRetryCount)
	}
	if summary.CapacityWaitCount != 1 {
		t.Fatalf("summary capacity_wait_count = %d, want 1", summary.CapacityWaitCount)
	}
}

func TestHandleSummaryBuildsRetryBreakdownWithoutSummarySource(t *testing.T) {
	t.Parallel()

	st := orchestrator.NewState()
	st.RecordTrackerSuccess(nowForRetryTests())
	st.RetryQueue["1"] = &orchestrator.RetryEntry{
		Identifier: "J-18",
		Kind:       orchestrator.RetryKindFailure,
		Error:      "agent crashed",
	}
	st.RetryQueue["2"] = &orchestrator.RetryEntry{
		Identifier:   "J-21",
		Kind:         orchestrator.RetryKindCapacity,
		FailureCount: 1,
		DeferCount:   2,
		Error:        "no slots",
	}

	server := New(&fakeStateSource{
		states: map[string]*orchestrator.State{"alpha": st},
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
	if summary.RetryCount != 2 {
		t.Fatalf("summary retry_count = %d, want 2", summary.RetryCount)
	}
	if summary.FailureRetryCount != 1 {
		t.Fatalf("summary failure_retry_count = %d, want 1", summary.FailureRetryCount)
	}
	if summary.CapacityWaitCount != 1 {
		t.Fatalf("summary capacity_wait_count = %d, want 1", summary.CapacityWaitCount)
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

func nowForRetryTests() time.Time {
	return time.Date(2026, 3, 6, 14, 0, 0, 0, time.UTC)
}

func TestHandleDashboardIncludesRetryBreakdown(t *testing.T) {
	t.Parallel()

	st := orchestrator.NewState()
	st.Running["run-1"] = &orchestrator.RunAttempt{Identifier: "J-17", Status: orchestrator.StatusStreamingTurn}
	st.RetryQueue["1"] = &orchestrator.RetryEntry{
		Identifier: "J-18",
		Kind:       orchestrator.RetryKindFailure,
		Error:      "agent crashed",
	}
	st.RetryQueue["2"] = &orchestrator.RetryEntry{
		Identifier:   "J-21",
		Kind:         orchestrator.RetryKindCapacity,
		FailureCount: 1,
		DeferCount:   2,
		Error:        "no slots",
	}
	st.CompletedCount = 3
	st.Totals.TotalTokens = 1234

	server := New(&fakeSummarySource{
		states: map[string]*orchestrator.State{"alpha": st},
	}, 7777)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	server.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"Retrying</div></div>",
		"Failure Retry</div></div>",
		"Capacity Wait</div></div>",
		">2</div><div class=\"stat-label\">Retrying",
		">1</div><div class=\"stat-label\">Failure Retry",
		">1</div><div class=\"stat-label\">Capacity Wait",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, body)
		}
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

func TestHandleStateIncludesRetrySemantics(t *testing.T) {
	t.Parallel()

	st := orchestrator.NewState()
	st.RetryQueue["1"] = &orchestrator.RetryEntry{
		Identifier:   "J-21",
		Kind:         orchestrator.RetryKindCapacity,
		Attempt:      2,
		FailureCount: 1,
		DeferCount:   3,
		Error:        "no slots",
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
		Retrying map[string]struct {
			Attempt      int    `json:"attempt"`
			Kind         string `json:"kind"`
			FailureCount int    `json:"failure_count"`
			DeferCount   int    `json:"defer_count"`
			Error        string `json:"error"`
		} `json:"retrying"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode state: %v", err)
	}

	retry, ok := payload.Retrying["J-21"]
	if !ok {
		t.Fatalf("retrying payload missing J-21: %#v", payload.Retrying)
	}
	if retry.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2", retry.Attempt)
	}
	if retry.Kind != string(orchestrator.RetryKindCapacity) {
		t.Fatalf("kind = %q, want %q", retry.Kind, orchestrator.RetryKindCapacity)
	}
	if retry.FailureCount != 1 {
		t.Fatalf("failure_count = %d, want 1", retry.FailureCount)
	}
	if retry.DeferCount != 3 {
		t.Fatalf("defer_count = %d, want 3", retry.DeferCount)
	}
	if retry.Error != "no slots" {
		t.Fatalf("error = %q, want %q", retry.Error, "no slots")
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
		Name             string `json:"name"`
		Retrying         int    `json:"retrying"`
		FailureRetrying  int    `json:"failure_retrying"`
		CapacityWaiting  int    `json:"capacity_waiting"`
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
	if payload[0].FailureRetrying != 0 {
		t.Fatalf("failure_retrying = %d, want 0", payload[0].FailureRetrying)
	}
	if payload[0].CapacityWaiting != 0 {
		t.Fatalf("capacity_waiting = %d, want 0", payload[0].CapacityWaiting)
	}
}

func TestHandleProjectsIncludesRetryBreakdown(t *testing.T) {
	t.Parallel()

	st := orchestrator.NewState()
	st.RetryQueue["1"] = &orchestrator.RetryEntry{
		Identifier: "J-18",
		Kind:       orchestrator.RetryKindFailure,
		Error:      "agent crashed",
	}
	st.RetryQueue["2"] = &orchestrator.RetryEntry{
		Identifier:   "J-21",
		Kind:         orchestrator.RetryKindCapacity,
		FailureCount: 1,
		DeferCount:   2,
		Error:        "no slots",
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
		Name             string `json:"name"`
		Retrying         int    `json:"retrying"`
		FailureRetrying  int    `json:"failure_retrying"`
		CapacityWaiting  int    `json:"capacity_waiting"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode projects: %v", err)
	}
	if len(payload) != 1 {
		t.Fatalf("projects len = %d, want 1", len(payload))
	}
	if payload[0].Retrying != 2 {
		t.Fatalf("retrying = %d, want 2", payload[0].Retrying)
	}
	if payload[0].FailureRetrying != 1 {
		t.Fatalf("failure_retrying = %d, want 1", payload[0].FailureRetrying)
	}
	if payload[0].CapacityWaiting != 1 {
		t.Fatalf("capacity_waiting = %d, want 1", payload[0].CapacityWaiting)
	}
}
