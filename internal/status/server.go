// Package status provides an HTTP status server for the orchestrator(s).
package status

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"symphony/internal/orchestrator"
)

// Source is implemented by Orchestrator (single) and DaemonManager (multi-project).
type Source interface {
	GetAllStates() map[string]*orchestrator.State
}

type RefreshSource interface {
	TriggerRefresh(context.Context)
}

// Server is a lightweight HTTP status server.
type Server struct {
	source Source
	port   int
	srv    *http.Server
}

func New(source Source, port int) *Server {
	s := &Server{source: source, port: port}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleDashboard)
	mux.HandleFunc("GET /api/v1/summary", s.handleSummary)
	mux.HandleFunc("GET /api/v1/state", s.handleState)
	mux.HandleFunc("GET /api/v1/projects", s.handleProjects)
	mux.HandleFunc("GET /api/v1/{issue_id}", s.handleIssue)
	mux.HandleFunc("POST /api/v1/refresh", s.handleRefresh)
	s.srv = &http.Server{Handler: mux}
	return s
}

// Run starts the HTTP server and blocks until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.port))
	if err != nil {
		return fmt.Errorf("listen :%d: %w", s.port, err)
	}
	slog.Info("status_server.started", "port", s.port)

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
	}()

	if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// -- handlers --

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	states := s.source.GetAllStates()

	totalRunning, totalRetrying, totalCompleted := 0, 0, 0
	var totalTokens int64
	rows := ""

	for proj, st := range states {
		st.Lock()
		totalRunning += len(st.Running)
		totalRetrying += len(st.RetryQueue)
		totalCompleted += st.CompletedCount
		totalTokens += st.Totals.TotalTokens
		for _, attempt := range st.Running {
			rows += fmt.Sprintf(
				"<tr><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%d</td></tr>",
				proj, attempt.Identifier, attempt.Status,
				attempt.Session.TurnCount, attempt.Session.TotalTokens,
			)
		}
		st.Unlock()
	}
	if rows == "" {
		rows = `<tr><td colspan="5">No running agents</td></tr>`
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, dashboardHTML, totalRunning, totalRetrying, totalCompleted, totalTokens, rows)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	states := s.source.GetAllStates()

	running := map[string]any{}
	retrying := map[string]any{}
	var completed int
	var inTok, outTok, totTok int64

	for _, st := range states {
		st.Lock()
		for _, a := range st.Running {
			running[a.Identifier] = attemptToMap(a)
		}
		for _, e := range st.RetryQueue {
			retrying[e.Identifier] = map[string]any{"attempt": e.Attempt, "error": e.Error}
		}
		completed += st.CompletedCount
		inTok += st.Totals.InputTokens
		outTok += st.Totals.OutputTokens
		totTok += st.Totals.TotalTokens
		st.Unlock()
	}

	writeJSON(w, 200, map[string]any{
		"running":         running,
		"retrying":        retrying,
		"completed_count": completed,
		"codex_totals": map[string]any{
			"input_tokens": inTok, "output_tokens": outTok, "total_tokens": totTok,
		},
	})
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	if summarySource, ok := s.source.(SummarySource); ok {
		writeJSON(w, 200, summarySource.GetSummary())
		return
	}
	writeJSON(w, 200, BuildSummary(s.source.GetAllStates()))
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	states := s.source.GetAllStates()
	result := make([]map[string]any, 0, len(states))
	for name, st := range states {
		st.Lock()
		result = append(result, map[string]any{
			"name":            name,
			"running":         len(st.Running),
			"retrying":        len(st.RetryQueue),
			"completed_count": st.CompletedCount,
			"total_tokens":    st.Totals.TotalTokens,
		})
		st.Unlock()
	}
	writeJSON(w, 200, result)
}

func (s *Server) handleIssue(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("issue_id")
	for _, st := range s.source.GetAllStates() {
		st.Lock()
		for _, a := range st.Running {
			if a.Identifier == id {
				m := attemptDetailMap(a)
				st.Unlock()
				writeJSON(w, 200, m)
				return
			}
		}
		st.Unlock()
	}
	writeJSON(w, 404, map[string]any{"error": "not_found", "message": id + " not running"})
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if refresher, ok := s.source.(RefreshSource); ok {
		refresher.TriggerRefresh(r.Context())
	}
	writeJSON(w, 202, map[string]any{"status": "accepted"})
}

// -- serialization --

func attemptToMap(a *orchestrator.RunAttempt) map[string]any {
	m := map[string]any{
		"issue_id":   a.IssueID,
		"status":     string(a.Status),
		"attempt":    a.Attempt,
		"turn_count": a.Session.TurnCount,
		"tokens":     map[string]any{"input": a.Session.InputTokens, "output": a.Session.OutputTokens, "total": a.Session.TotalTokens},
		"pid":        a.Session.AgentPID,
	}
	if !a.StartedAt.IsZero() {
		m["started_at"] = a.StartedAt.Format(time.RFC3339)
	}
	return m
}

func attemptDetailMap(a *orchestrator.RunAttempt) map[string]any {
	sess := map[string]any{
		"session_id": a.Session.SessionID,
		"thread_id":  a.Session.ThreadID,
		"turn_count": a.Session.TurnCount,
		"pid":        a.Session.AgentPID,
		"tokens":     map[string]any{"input": a.Session.InputTokens, "output": a.Session.OutputTokens, "total": a.Session.TotalTokens},
	}
	if a.Session.LastEventAt != nil {
		sess["last_event_at"] = a.Session.LastEventAt.Format(time.RFC3339)
	}
	m := map[string]any{
		"identifier":     a.Identifier,
		"issue_id":       a.IssueID,
		"status":         string(a.Status),
		"attempt":        a.Attempt,
		"workspace_path": a.WorkspacePath,
		"error":          a.Error,
		"session":        sess,
	}
	if !a.StartedAt.IsZero() {
		m["started_at"] = a.StartedAt.Format(time.RFC3339)
	}
	return m
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

const dashboardHTML = `<!DOCTYPE html>
<html><head><title>Symphony</title>
<meta http-equiv="refresh" content="10">
<style>
body{font-family:system-ui,sans-serif;margin:2rem;background:#111;color:#ddd}
table{border-collapse:collapse;width:100%%;margin:1rem 0}
th,td{border:1px solid #333;padding:.5rem 1rem;text-align:left}
th{background:#222}h1{color:#7af}h2{color:#aaa;margin-top:2rem}
.stat{display:inline-block;margin:0 2rem 1rem 0}
.stat-val{font-size:1.5rem;font-weight:bold;color:#7f7}
.stat-label{font-size:.8rem;color:#888}
</style></head><body>
<h1>Symphony Orchestrator</h1>
<div>
  <div class="stat"><div class="stat-val">%d</div><div class="stat-label">Running</div></div>
  <div class="stat"><div class="stat-val">%d</div><div class="stat-label">Retrying</div></div>
  <div class="stat"><div class="stat-val">%d</div><div class="stat-label">Completed</div></div>
  <div class="stat"><div class="stat-val">%d</div><div class="stat-label">Total Tokens</div></div>
</div>
<h2>Running Sessions</h2>
<table><tr><th>Project</th><th>Issue</th><th>Status</th><th>Turns</th><th>Tokens</th></tr>
%s
</table></body></html>`
