package status

import (
	"net/http"
	"strings"
	"testing"

	"symphony/internal/testutil"
)

func TestClientSummary(t *testing.T) {
	t.Parallel()

	client := NewClient("http://status.test")
	client.http = testutil.NewHandlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/summary" {
			t.Fatalf("path = %q, want /api/v1/summary", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, Summary{
			Status:          "running",
			SubprocessCount: 2,
		})
	}))

	summary, err := client.Summary()
	if err != nil {
		t.Fatalf("Summary() error = %v", err)
	}
	if summary.Status != "running" {
		t.Fatalf("status = %q, want running", summary.Status)
	}
	if summary.SubprocessCount != 2 {
		t.Fatalf("subprocess_count = %d, want 2", summary.SubprocessCount)
	}
}

func TestClientProjects(t *testing.T) {
	t.Parallel()

	client := NewClient("http://status.test")
	client.http = testutil.NewHandlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects" {
			t.Fatalf("path = %q, want /api/v1/projects", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, []ProjectSummary{{
			Name: "alpha",
			RunningIssues: []RunningIssueSummary{{
				Identifier: "J-54",
				Status:     "streaming_turn",
				TurnCount:  3,
			}},
		}})
	}))

	projects, err := client.Projects()
	if err != nil {
		t.Fatalf("Projects() error = %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("len(projects) = %d, want 1", len(projects))
	}
	if len(projects[0].RunningIssues) != 1 || projects[0].RunningIssues[0].Identifier != "J-54" {
		t.Fatalf("running_issues = %#v, want J-54", projects[0].RunningIssues)
	}
}

func TestClientRefresh(t *testing.T) {
	t.Parallel()

	client := NewClient("http://status.test")
	client.http = testutil.NewHandlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/v1/refresh" {
			t.Fatalf("path = %q, want /api/v1/refresh", r.URL.Path)
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
	}))

	if err := client.Refresh(); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
}

func TestClientSummaryReturnsStatusError(t *testing.T) {
	t.Parallel()

	client := NewClient("http://status.test")
	client.http = testutil.NewHandlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))

	_, err := client.Summary()
	if err == nil {
		t.Fatal("Summary() error = nil, want status error")
	}
	if !strings.Contains(err.Error(), "fetch /api/v1/summary: status 502") {
		t.Fatalf("error = %q, want summary status context", err)
	}
}

func TestClientProjectsReturnsDecodeError(t *testing.T) {
	t.Parallel()

	client := NewClient("http://status.test")
	client.http = testutil.NewHandlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{"))
	}))

	_, err := client.Projects()
	if err == nil {
		t.Fatal("Projects() error = nil, want decode error")
	}
	if !strings.Contains(err.Error(), "decode /api/v1/projects") {
		t.Fatalf("error = %q, want decode context", err)
	}
}

func TestClientRefreshReturnsStatusError(t *testing.T) {
	t.Parallel()

	client := NewClient("http://status.test")
	client.http = testutil.NewHandlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))

	err := client.Refresh()
	if err == nil {
		t.Fatal("Refresh() error = nil, want status error")
	}
	if !strings.Contains(err.Error(), "refresh: status 503") {
		t.Fatalf("error = %q, want refresh status context", err)
	}
}
