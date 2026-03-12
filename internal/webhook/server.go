package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

type Server struct {
	handler *Handler
	port    int
	bind    string
	srv     *http.Server
}

func NewServer(handler *Handler, port int, bind string) *Server {
	s := &Server{
		handler: handler,
		port:    port,
		bind:    bind,
	}
	mux := http.NewServeMux()
	mux.Handle("POST /webhook/linear", handler)
	mux.HandleFunc("GET /webhook/health", s.handleHealth)
	s.srv = &http.Server{Handler: mux}
	return s
}

func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.bind, s.port))
	if err != nil {
		return fmt.Errorf("listen %s:%d: %w", s.bind, s.port, err)
	}

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

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}
