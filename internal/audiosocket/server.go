package audiosocket

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwaddie/audiosocket-bridge/internal/bridge"
	"github.com/cloudwaddie/audiosocket-bridge/internal/prompts"
)

// ServerConfig configures the AudioSocket listener.
type ServerConfig struct {
	// Mode is reported in logs and health output.
	Mode         string
	ListenAddr   string
	Session      SessionConfig
	MaxPending   int
	ShutdownWait time.Duration
}

// Server accepts AudioSocket connections. Only one call is active at a time;
// additional callers receive an error frame and are disconnected.
type Server struct {
	cfg         ServerConfig
	log         *slog.Logger
	newEndpoint func() (bridge.Endpoint, error)

	ln      net.Listener
	slots   chan struct{}
	pending chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	active *Session

	wg       sync.WaitGroup
	ready    atomic.Bool
	accepted atomic.Uint64
	rejected atomic.Uint64
	completed atomic.Uint64
	failed   atomic.Uint64
	started  time.Time
}

// NewServer creates a server. The factory is called once per accepted call so
// that stateful backends (virtual audio devices) start clean per call.
func NewServer(cfg ServerConfig, log *slog.Logger, factory func() (bridge.Endpoint, error)) *Server {
	if cfg.MaxPending < 1 {
		cfg.MaxPending = 8
	}
	if cfg.ShutdownWait <= 0 {
		cfg.ShutdownWait = 2 * time.Second
	}
	return &Server{
		cfg:         cfg,
		log:         log,
		newEndpoint: factory,
		slots:       make(chan struct{}, 1),
		pending:     make(chan struct{}, cfg.MaxPending),
		conns:       make(map[net.Conn]struct{}),
	}
}

// Serve listens and blocks until the listener fails or ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	s.ctx, s.cancel = context.WithCancel(ctx)

	lc := net.ListenConfig{KeepAlive: 30 * time.Second}
	ln, err := lc.Listen(ctx, "tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.ListenAddr, err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	s.started = time.Now()
	s.ready.Store(true)
	s.log.Info("audiosocket listener started", "addr", ln.Addr().String(), "mode", s.cfg.Mode)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return fmt.Errorf("accept: %w", err)
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn)
		}()
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	s.track(conn, true)
	defer s.track(conn, false)

	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
		_ = tc.SetNoDelay(true)
	}

	select {
	case s.pending <- struct{}{}:
	default:
		s.rejected.Add(1)
		s.log.Warn("connection rejected: handshake limit reached", "remote", conn.RemoteAddr().String())
		return
	}

	sess := NewSession(conn, s.cfg.Session, s.log.With("remote", conn.RemoteAddr().String()))
	identifyErr := sess.Identify()
	<-s.pending
	if identifyErr != nil {
		s.failed.Add(1)
		s.log.Warn("handshake failed", "remote", conn.RemoteAddr().String(), "err", identifyErr)
		return
	}

	select {
	case s.slots <- struct{}{}:
	default:
		s.rejected.Add(1)
		s.log.Warn("call rejected: another call is active", "uuid", sess.UUID())
		if s.cfg.Session.Prompts {
			sess.PlayPrompt(prompts.Busy)
		}
		sess.Reject("busy")
		return
	}
	defer func() { <-s.slots }()

	ep, err := s.newEndpoint()
	if err != nil {
		s.failed.Add(1)
		s.log.Error("cannot create audio endpoint", "uuid", sess.UUID(), "err", err)
		sess.Reject("audio backend unavailable")
		return
	}

	sess.Attach(ep)
	s.accepted.Add(1)
	s.setActive(sess)
	defer s.setActive(nil)

	if err := sess.Run(s.ctx); err != nil {
		s.failed.Add(1)
		return
	}
	s.completed.Add(1)
}

// HangupCall terminates only the call with this UUID.
// Returning false means the call has already ended or its UUID does not match.
func (s *Server) HangupCall(uuid string) bool {
	s.mu.Lock()
	active := s.active
	if active == nil || active.UUID() != uuid {
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	active.Hangup()
	return true
}

// Shutdown stops accepting calls, cancels active sessions and waits for them
// to finish or for ctx to expire.
func (s *Server) Shutdown(ctx context.Context) error {
	s.ready.Store(false)
	if s.cancel != nil {
		s.cancel()
	}
	if s.ln != nil {
		_ = s.ln.Close()
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
	}

	s.closeConns()
	select {
	case <-done:
	case <-time.After(s.cfg.ShutdownWait):
	}
	return ctx.Err()
}

// Ready reports whether the listener is accepting calls.
func (s *Server) Ready() bool { return s.ready.Load() }

// Addr returns the bound listener address, or the configured address before
// Serve has started listening.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return s.ln.Addr().String()
	}
	return s.cfg.ListenAddr
}

func (s *Server) track(conn net.Conn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		s.conns[conn] = struct{}{}
		return
	}
	delete(s.conns, conn)
}

func (s *Server) closeConns() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (s *Server) setActive(sess *Session) {
	s.mu.Lock()
	s.active = sess
	s.mu.Unlock()
}

// Counters are process-lifetime call counters.
type Counters struct {
	Accepted  uint64 `json:"accepted"`
	Rejected  uint64 `json:"rejected"`
	Completed uint64 `json:"completed"`
	Failed    uint64 `json:"failed"`
}

// Snapshot is the data exposed by the health endpoints.
type Snapshot struct {
	Ready         bool          `json:"ready"`
	Mode          string        `json:"mode"`
	UptimeSeconds float64       `json:"uptime_seconds"`
	Counters      Counters      `json:"counters"`
	ActiveCall    *CallSnapshot `json:"active_call,omitempty"`
}

// Snapshot returns a consistent view of the server state.
func (s *Server) Snapshot() Snapshot {
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()

	snap := Snapshot{
		Ready: s.ready.Load(),
		Mode:  s.cfg.Mode,
		Counters: Counters{
			Accepted:  s.accepted.Load(),
			Rejected:  s.rejected.Load(),
			Completed: s.completed.Load(),
			Failed:    s.failed.Load(),
		},
	}
	if !s.started.IsZero() {
		snap.UptimeSeconds = time.Since(s.started).Seconds()
	}
	if active != nil {
		call := active.Snapshot()
		snap.ActiveCall = &call
	}
	return snap
}

// Metrics renders the snapshot in Prometheus text exposition format.
func (s Snapshot) Metrics() string {
	var b strings.Builder
	metric := func(name, help, typ string, value any) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, typ, name, value)
	}
	ready := 0
	if s.Ready {
		ready = 1
	}
	active := 0
	if s.ActiveCall != nil {
		active = 1
	}
	metric("audiosocket_bridge_ready", "Whether the AudioSocket listener is accepting calls.", "gauge", ready)
	metric("audiosocket_bridge_active_calls", "Number of calls currently bridged.", "gauge", active)
	metric("audiosocket_bridge_uptime_seconds", "Process uptime in seconds.", "gauge", s.UptimeSeconds)
	for _, c := range []struct {
		result string
		value  uint64
	}{
		{"accepted", s.Counters.Accepted},
		{"rejected", s.Counters.Rejected},
		{"completed", s.Counters.Completed},
		{"failed", s.Counters.Failed},
	} {
		fmt.Fprintf(&b, "audiosocket_bridge_calls_total{result=%q} %d\n", c.result, c.value)
	}
	if call := s.ActiveCall; call != nil {
		fmt.Fprintf(&b, "audiosocket_bridge_call_frames_total{direction=\"in\"} %d\n", call.InFrames)
		fmt.Fprintf(&b, "audiosocket_bridge_call_frames_total{direction=\"out\"} %d\n", call.OutFrames)
		fmt.Fprintf(&b, "audiosocket_bridge_call_bytes_total{direction=\"in\"} %d\n", call.InBytes)
		fmt.Fprintf(&b, "audiosocket_bridge_call_bytes_total{direction=\"out\"} %d\n", call.OutBytes)
		fmt.Fprintf(&b, "audiosocket_bridge_call_dropped_frames_total %d\n", call.DroppedFrames)
		if ep := call.EndpointStats; ep != nil {
			fmt.Fprintf(&b, "audiosocket_bridge_endpoint_underruns_total %d\n", ep.Underruns)
			fmt.Fprintf(&b, "audiosocket_bridge_endpoint_restarts_total %d\n", ep.Restarts)
		}
	}
	return b.String()
}
