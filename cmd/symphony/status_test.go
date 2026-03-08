package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
				Identifier:  "J-54",
				Status:      "streaming_turn",
				TurnCount:   3,
				LastEventAt: "2026-03-09T01:00:00Z",
			}},
		}},
	})

	for _, want := range []string{
		"Status: running",
		"[alpha] running (healthy)",
		"J-54 | streaming_turn | turn 3 | last event 2026-03-09T01:00:00Z",
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
