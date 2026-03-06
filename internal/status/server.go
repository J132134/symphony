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
	mux.HandleFunc("GET /api/v1/summary", s.handleSummary)
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

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	if summarySource, ok := s.source.(SummarySource); ok {
		writeJSON(w, 200, summarySource.GetSummary())
		return
	}
	writeJSON(w, 200, BuildSummary(s.source.GetAllStates()))
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if refresher, ok := s.source.(RefreshSource); ok {
		refresher.TriggerRefresh(r.Context())
	}
	writeJSON(w, 202, map[string]any{"status": "accepted"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
