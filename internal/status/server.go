// Package status provides an HTTP status server for the orchestrator(s).
package status

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
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

type LinearWebhookSource interface {
	TriggerRefreshForIssue(context.Context, string, string) bool
}

type ProjectSource interface {
	GetProjects() []ProjectSummary
}

// Server is a lightweight HTTP status server.
type Server struct {
	source        Source
	port          int
	webhookSecret string
	srv           *http.Server
}

func New(source Source, port int, webhookSecret string) *Server {
	s := &Server{source: source, port: port, webhookSecret: webhookSecret}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/summary", s.handleSummary)
	mux.HandleFunc("GET /api/v1/projects", s.handleProjects)
	mux.HandleFunc("POST /api/v1/refresh", s.handleRefresh)
	mux.HandleFunc("POST /webhook/linear", s.handleLinearWebhook)
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

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	if projectsSource, ok := s.source.(ProjectSource); ok {
		if projects := projectsSource.GetProjects(); projects != nil {
			writeJSON(w, 200, projects)
			return
		}
	}
	writeJSON(w, 200, BuildSummary(s.source.GetAllStates()).Projects)
}
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if refresher, ok := s.source.(RefreshSource); ok {
		refresher.TriggerRefresh(r.Context())
	}
	writeJSON(w, 202, map[string]any{"status": "accepted"})
}

type linearWebhookPayload struct {
	Action string `json:"action"`
	Type   string `json:"type"`
	Data   struct {
		ID    string `json:"id"`
		State struct {
			Name string `json:"name"`
		} `json:"state"`
	} `json:"data"`
}

func (s *Server) handleLinearWebhook(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(s.webhookSecret) == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "linear webhook secret is not configured"})
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	if !verifyLinearSignature(body, s.webhookSecret, linearSignatureHeader(r.Header)) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid signature"})
		return
	}

	var payload linearWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json payload"})
		return
	}
	if payload.Action != "update" || payload.Type != "Issue" {
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "ignored"})
		return
	}

	issueID := strings.TrimSpace(payload.Data.ID)
	stateName := strings.TrimSpace(payload.Data.State.Name)
	if issueID == "" || stateName == "" {
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "ignored"})
		return
	}

	handled := false
	if source, ok := s.source.(LinearWebhookSource); ok {
		handled = source.TriggerRefreshForIssue(r.Context(), issueID, stateName)
	}
	if handled {
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "ignored"})
}

func linearSignatureHeader(header http.Header) string {
	sig := strings.TrimSpace(header.Get("X-Linear-Signature"))
	if sig != "" {
		return sig
	}
	return strings.TrimSpace(header.Get("Linear-Signature"))
}

func verifyLinearSignature(body []byte, secret, signature string) bool {
	secret = strings.TrimSpace(secret)
	signature = strings.TrimSpace(strings.TrimPrefix(signature, "sha256="))
	if secret == "" || signature == "" {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeEq(int32(len(signature)), int32(len(expected))) != 1 {
		return false
	}
	return hmac.Equal([]byte(strings.ToLower(signature)), []byte(expected))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
