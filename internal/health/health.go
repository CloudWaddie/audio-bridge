// Package health exposes liveness, readiness, statistics and Prometheus
// metrics over HTTP. It is meant to be reachable only from the container
// network.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/cloudwaddie/audiosocket-bridge/internal/audiosocket"
)

// Server is the health and statistics HTTP server.
type Server struct {
	log  *slog.Logger
	srv  *http.Server
	snap func() audiosocket.Snapshot
}

// New builds the server. The snapshot function is called per request.
func New(addr string, log *slog.Logger, snap func() audiosocket.Snapshot) *Server {
	s := &Server{log: log, snap: snap}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleLiveness)
	mux.HandleFunc("GET /readyz", s.handleReadiness)
	mux.HandleFunc("GET /stats", s.handleStats)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return s
}

// Start binds the listener and serves in the background.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return err
	}
	s.log.Info("health server started", "addr", ln.Addr().String())
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("health server failed", "err", err)
		}
	}()
	return nil
}

// Shutdown stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

func (s *Server) handleLiveness(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleReadiness(w http.ResponseWriter, _ *http.Request) {
	snap := s.snap()
	ready := snap.Ready
	reason := ""
	switch {
	case !snap.Ready:
		reason = "listener is not accepting calls"
	case snap.ActiveCall != nil && snap.ActiveCall.EndpointStats != nil && !snap.ActiveCall.EndpointStats.Ready:
		ready = false
		reason = "audio backend is not ready"
	}

	w.Header().Set("Content-Type", "application/json")
	code := http.StatusOK
	if !ready {
		code = http.StatusServiceUnavailable
	}
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ready":  ready,
		"mode":   snap.Mode,
		"reason": reason,
	})
}

func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(s.snap())
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(s.snap().Metrics()))
}
