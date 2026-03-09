package status

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"symphony/internal/orchestrator"
)

type fakeSummarySource struct {
	summary            Summary
	projects           []ProjectSummary
	states             map[string]*orchestrator.State
	refreshCalls       int
	webhookCalls       int
	lastWebhookIssueID string
	lastWebhookState   string
	webhookHandled     bool
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

func (f *fakeSummarySource) TriggerRefreshForIssue(_ context.Context, issueID, stateName string) bool {
	f.webhookCalls++
	f.lastWebhookIssueID = issueID
	f.lastWebhookState = stateName
	return f.webhookHandled
}

func TestHandleSummaryPrefersSourceSummary(t *testing.T) {
	t.Parallel()

	server := New(&fakeSummarySource{
		summary: Summary{
			Status:          "error",
			SubprocessCount: 2,
		},
	}, 7777, "")

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
	server := New(source, 7777, "")

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
	}, 7777, "")

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
	state.Running["run-1"] = attempt

	server := New(&fakeSummarySource{
		states: map[string]*orchestrator.State{"alpha": state},
	}, 7777, "")

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
}

func TestHandleLinearWebhookTriggersIssueRefresh(t *testing.T) {
	t.Parallel()

	body := []byte(`{"action":"update","type":"Issue","data":{"id":"issue-123","state":{"name":"In Progress"}}}`)
	secret := "super-secret"
	source := &fakeSummarySource{webhookHandled: true}
	server := New(source, 7777, secret)

	req := httptest.NewRequest(http.MethodPost, "/webhook/linear", bytes.NewReader(body))
	req.Header.Set("X-Linear-Signature", signedBody(body, secret))
	rec := httptest.NewRecorder()
	server.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status code = %d, want 202", rec.Code)
	}
	if source.webhookCalls != 1 {
		t.Fatalf("webhook_calls = %d, want 1", source.webhookCalls)
	}
	if source.lastWebhookIssueID != "issue-123" {
		t.Fatalf("issue_id = %q, want issue-123", source.lastWebhookIssueID)
	}
	if source.lastWebhookState != "In Progress" {
		t.Fatalf("state = %q, want In Progress", source.lastWebhookState)
	}
}

func TestHandleLinearWebhookRejectsInvalidSignature(t *testing.T) {
	t.Parallel()

	body := []byte(`{"action":"update","type":"Issue","data":{"id":"issue-123","state":{"name":"In Progress"}}}`)
	source := &fakeSummarySource{webhookHandled: true}
	server := New(source, 7777, "super-secret")

	req := httptest.NewRequest(http.MethodPost, "/webhook/linear", bytes.NewReader(body))
	req.Header.Set("X-Linear-Signature", "not-valid")
	rec := httptest.NewRecorder()
	server.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status code = %d, want 401", rec.Code)
	}
	if source.webhookCalls != 0 {
		t.Fatalf("webhook_calls = %d, want 0", source.webhookCalls)
	}
}

func TestHandleLinearWebhookIgnoresNonIssueUpdate(t *testing.T) {
	t.Parallel()

	body := []byte(`{"action":"create","type":"Issue","data":{"id":"issue-123","state":{"name":"In Progress"}}}`)
	secret := "super-secret"
	source := &fakeSummarySource{webhookHandled: true}
	server := New(source, 7777, secret)

	req := httptest.NewRequest(http.MethodPost, "/webhook/linear", bytes.NewReader(body))
	req.Header.Set("Linear-Signature", "sha256="+signedBody(body, secret))
	rec := httptest.NewRecorder()
	server.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status code = %d, want 202", rec.Code)
	}
	if source.webhookCalls != 0 {
		t.Fatalf("webhook_calls = %d, want 0", source.webhookCalls)
	}
}

func TestHandleLinearWebhookReturnsIgnoredWhenSourceDoesNotHandle(t *testing.T) {
	t.Parallel()

	body := []byte(`{"action":"update","type":"Issue","data":{"id":"issue-123","state":{"name":"Backlog"}}}`)
	secret := "super-secret"
	source := &fakeSummarySource{webhookHandled: false}
	server := New(source, 7777, secret)

	req := httptest.NewRequest(http.MethodPost, "/webhook/linear", bytes.NewReader(body))
	req.Header.Set("X-Linear-Signature", signedBody(body, secret))
	rec := httptest.NewRecorder()
	server.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status code = %d, want 202", rec.Code)
	}
	if source.webhookCalls != 1 {
		t.Fatalf("webhook_calls = %d, want 1", source.webhookCalls)
	}
}

func TestHandleLinearWebhookRequiresConfiguredSecret(t *testing.T) {
	t.Parallel()

	body := []byte(`{"action":"update","type":"Issue","data":{"id":"issue-123","state":{"name":"In Progress"}}}`)
	source := &fakeSummarySource{webhookHandled: true}
	server := New(source, 7777, "")

	req := httptest.NewRequest(http.MethodPost, "/webhook/linear", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status code = %d, want 503", rec.Code)
	}
	if source.webhookCalls != 0 {
		t.Fatalf("webhook_calls = %d, want 0", source.webhookCalls)
	}
}

func signedBody(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
