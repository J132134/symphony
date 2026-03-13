package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"symphony/internal/status"
)

type stubSummaryClient struct {
	summary status.Summary
	err     error
}

func (s stubSummaryClient) Summary() (status.Summary, error) {
	return s.summary, s.err
}

func TestRunStatusJSON(t *testing.T) {
	t.Parallel()

	prev := newSummaryClient
	t.Cleanup(func() { newSummaryClient = prev })
	newSummaryClient = func(string) summaryClient {
		return stubSummaryClient{summary: status.Summary{
			Status: "running",
			Projects: []status.ProjectSummary{{
				Name: "alpha",
				RunningIssues: []status.RunningIssueSummary{{
					Identifier: "J-54",
					Status:     "streaming_turn",
					TurnCount:  2,
				}},
			}},
		}}
	}

	var out bytes.Buffer
	if err := runStatus(&out, []string{"--url", "http://daemon.local", "--json"}); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	var summary status.Summary
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if summary.Status != "running" {
		t.Fatalf("status = %q, want running", summary.Status)
	}
	if len(summary.Projects) != 1 || len(summary.Projects[0].RunningIssues) != 1 {
		t.Fatalf("projects = %#v, want running issue detail", summary.Projects)
	}
}

func TestFormatStatusSummaryIncludesRunningIssueDetails(t *testing.T) {
	t.Parallel()

	out := formatStatusSummary(status.Summary{
		Status:          "running",
		ProjectCount:    1,
		SubprocessCount: 1,
		RetryCount:      0,
		Projects: []status.ProjectSummary{{
			Name:             "alpha",
			Status:           "running",
			Health:           "healthy",
			TrackerConnected: true,
			RunningIssues: []status.RunningIssueSummary{{
				Identifier:        "J-54",
				Status:            "streaming_turn",
				IssueState:        "In Progress",
				TurnCount:         3,
				LastEventAt:       "2026-03-09T01:00:00Z",
				CurrentActivityAt: "2026-03-09T00:59:30Z",
				CurrentActivity:   "Tool Call: linear_graphql {\"query\":\"issue(id:J-54)\"}",
				SessionID:         "session-1234567890",
				ThreadID:          "thread-123",
				TurnID:            "turn-abc",
				PID:               "4321",
				InputTokens:       4200,
				OutputTokens:      380,
				TotalTokens:       42100,
				RecentEvents: []status.RunningEventDetail{
					{
						OccurredAt: "2026-03-09T00:59:00Z",
						Detail:     "Tool Call: linear_graphql {\"query\":\"issue(id:J-54)\"}",
					},
				},
			}},
		}},
	})

	for _, want := range []string{
		"Status: running",
		"[alpha] running (healthy)",
		"J-54 | streaming_turn | turn 3 | last event 2026-03-09T01:00:00Z",
		"tracker state: In Progress",
		"current: Tool Call: linear_graphql",
		"current at: 2026-03-09T00:59:30Z",
		"runtime: session session-1234567890 | thread thread-123 | turn turn-abc | pid 4321 | tokens in 4,200 / out 380 / total 42,100",
		"recent events:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestResolveStatusBaseURLUsesConfiguredPort(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("status_server:\n  port: 8787\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	baseURL, err := resolveStatusBaseURL("", configPath)
	if err != nil {
		t.Fatalf("resolveStatusBaseURL() error = %v", err)
	}
	if baseURL != "http://127.0.0.1:8787" {
		t.Fatalf("baseURL = %q, want http://127.0.0.1:8787", baseURL)
	}
}

func TestResolveStatusBaseURLErrorsWhenDisabledInConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("status_server:\n  enabled: false\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := resolveStatusBaseURL("", configPath)
	if err == nil {
		t.Fatal("resolveStatusBaseURL() error = nil, want disabled status server error")
	}
	if !strings.Contains(err.Error(), "status server is disabled") {
		t.Fatalf("error = %q, want disabled status server context", err)
	}
	if !strings.Contains(err.Error(), configPath) {
		t.Fatalf("error = %q, want config path %q", err, configPath)
	}
}

func TestResolveStatusBaseURLFallsBackToDefault(t *testing.T) {
	t.Parallel()

	baseURL, err := resolveStatusBaseURL("", "")
	if err != nil {
		t.Fatalf("resolveStatusBaseURL() error = %v", err)
	}
	if baseURL != status.DefaultBaseURL {
		t.Fatalf("baseURL = %q, want %q", baseURL, status.DefaultBaseURL)
	}
}

func TestRenderStatusDashboardIncludesRunningAndBackoff(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 12, 13, 0, 0, 0, time.UTC)
	priority := 1
	out := renderStatusDashboard(status.Summary{
		Status:       "running",
		Version:      "v0.4.0",
		ProjectCount: 1,
		Projects: []status.ProjectSummary{{
			Name:            "symphony",
			SubprocessCount: 1,
			RetryCount:      1,
			RunningIssues: []status.RunningIssueSummary{{
				Identifier:        "SYM-12",
				Status:            "streaming_turn",
				IssueState:        "In Progress",
				Priority:          &priority,
				TurnCount:         2,
				StartedAt:         now.Add(-2 * time.Minute).Format(time.RFC3339),
				LastEvent:         "Server Notification: Item Completed: {\"status\":\"patched\"}",
				CurrentActivityAt: now.Add(-20 * time.Second).Format(time.RFC3339),
				CurrentActivity:   "Tool Call: apply_patch {\"path\":\"cmd/symphony/status.go\"}",
				SessionID:         "019ce23639d97ea097f22d7883fe9820",
				ThreadID:          "thread-123",
				TurnID:            "turn-abc",
				PID:               "4321",
				InputTokens:       4200,
				OutputTokens:      380,
				TotalTokens:       42100,
				RecentEvents: []status.RunningEventDetail{
					{
						OccurredAt: now.Add(-30 * time.Second).Format(time.RFC3339),
						Detail:     "Tool Call: apply_patch {\"path\":\"cmd/symphony/status.go\"}",
					},
					{
						OccurredAt: now.Add(-15 * time.Second).Format(time.RFC3339),
						Detail:     "Server Notification: Item Completed: {\"status\":\"patched\"}",
					},
				},
			}},
			RetryEntries: []status.RetrySummary{{
				Identifier: "SYM-99",
				Kind:       "failure",
				DueAt:      now.Add(15 * time.Second).Format(time.RFC3339),
				Error:      "agent crashed",
			}},
		}},
		TotalTokens: 42100,
	}, nil, now, now.Add(3*time.Second), 3*time.Second)

	for _, want := range []string{
		"SYMPHONY STATUS",
		"Next refresh: 3s",
		"Running",
		"SYM-12",
		"42.1k",
		"ACTIVITY",
		"Running details",
		"current: Tool Call: apply_patch",
		"current at: 2026-03-12T12:59:40Z",
		"runtime: session 019ce23639d97ea097f22d7883fe9820 | thread thread-123 | turn turn-abc | pid 4321 | tokens in 4,200 / out 380 / total 42,100",
		"Backoff queue",
		"SYM-99",
		"agent crashed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, out)
		}
	}
}
