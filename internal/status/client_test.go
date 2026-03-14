package status

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientSummary(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/summary" {
			t.Fatalf("path = %q, want /api/v1/summary", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, Summary{
			Status:          "running",
			SubprocessCount: 2,
		})
	}))
	defer srv.Close()

	summary, err := NewClient(srv.URL).Summary()
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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects" {
			t.Fatalf("path = %q, want /api/v1/projects", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, []ProjectSummary{{
			Name: "alpha",
			RunningIssues: []RunningIssueSummary{{
				Identifier:    "J-54",
				Status:        "streaming_turn",
				TurnCount:     3,
				CurrentTask:   "running tool: apply_patch",
				ServerMessage: "diff stream stalled",
			}},
		}})
	}))
	defer srv.Close()

	projects, err := NewClient(srv.URL).Projects()
	if err != nil {
		t.Fatalf("Projects() error = %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("len(projects) = %d, want 1", len(projects))
	}
	if len(projects[0].RunningIssues) != 1 || projects[0].RunningIssues[0].Identifier != "J-54" {
		t.Fatalf("running_issues = %#v, want J-54", projects[0].RunningIssues)
	}
	if projects[0].RunningIssues[0].CurrentTask != "running tool: apply_patch" {
		t.Fatalf("current_task = %q, want running tool: apply_patch", projects[0].RunningIssues[0].CurrentTask)
	}
	if projects[0].RunningIssues[0].ServerMessage != "diff stream stalled" {
		t.Fatalf("server_message = %q, want diff stream stalled", projects[0].RunningIssues[0].ServerMessage)
	}
}

func TestClientRefresh(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/v1/refresh" {
			t.Fatalf("path = %q, want /api/v1/refresh", r.URL.Path)
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
	}))
	defer srv.Close()

	if err := NewClient(srv.URL).Refresh(); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
}

func TestClientSummaryReturnsStatusError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL).Summary()
	if err == nil {
		t.Fatal("Summary() error = nil, want status error")
	}
	if !strings.Contains(err.Error(), "fetch /api/v1/summary: status 502") {
		t.Fatalf("error = %q, want summary status context", err)
	}
}

func TestClientProjectsReturnsDecodeError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{"))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL).Projects()
	if err == nil {
		t.Fatal("Projects() error = nil, want decode error")
	}
	if !strings.Contains(err.Error(), "decode /api/v1/projects") {
		t.Fatalf("error = %q, want decode context", err)
	}
}

func TestClientRefreshReturnsStatusError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := NewClient(srv.URL).Refresh()
	if err == nil {
		t.Fatal("Refresh() error = nil, want status error")
	}
	if !strings.Contains(err.Error(), "refresh: status 503") {
		t.Fatalf("error = %q, want refresh status context", err)
	}
}
