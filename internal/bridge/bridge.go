// Package bridge defines the pluggable audio backend that sits between the
// AudioSocket protocol and the sound system. The AudioSocket layer only knows
// this interface, so a future ChatGPT Voice backend can be added without
// touching protocol handling.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/cloudwaddie/audiosocket-bridge/internal/config"
)

// Sink receives call-bound PCM (s16le, mono, call rate). Implementations must
// copy the data if they retain it; the slice is reused by the caller.
type Sink func(pcm []byte)

// Endpoint is a bidirectional audio backend for one call.
type Endpoint interface {
	// Name identifies the backend in logs and health output.
	Name() string
	// Start prepares the backend. Audio destined for Asterisk is delivered to
	// sink from a backend goroutine.
	Start(ctx context.Context, sink Sink) error
	// Write delivers inbound call audio to the backend. It must not block.
	Write(pcm []byte) error
	// Stats returns aggregate counters. It is safe to call concurrently.
	Stats() Stats
	// Close releases backend resources. It is idempotent.
	Close() error
}

// Stats contains aggregate backend counters. No audio is ever retained.
type Stats struct {
	Name         string            `json:"name"`
	Ready        bool              `json:"ready"`
	InFrames     uint64            `json:"in_frames"`
	InBytes      uint64            `json:"in_bytes"`
	OutFrames    uint64            `json:"out_frames"`
	OutBytes     uint64            `json:"out_bytes"`
	DroppedBytes uint64            `json:"dropped_bytes"`
	Underruns    uint64            `json:"underruns"`
	Restarts     uint64            `json:"restarts"`
	LastError    string            `json:"last_error,omitempty"`
	Details      map[string]string `json:"details,omitempty"`
}

type counters struct {
	ready     atomic.Bool
	inFrames  atomic.Uint64
	inBytes   atomic.Uint64
	outFrames atomic.Uint64
	outBytes  atomic.Uint64
	dropped   atomic.Uint64
	underruns atomic.Uint64
	restarts  atomic.Uint64
	lastErr   atomic.Pointer[string]
}

func (c *counters) fail(err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	c.lastErr.Store(&msg)
}

func (c *counters) snapshot(name string) Stats {
	st := Stats{
		Name:      name,
		Ready:     c.ready.Load(),
		InFrames:  c.inFrames.Load(),
		InBytes:   c.inBytes.Load(),
		OutFrames: c.outFrames.Load(),
		OutBytes:  c.outBytes.Load(),
		DroppedBytes: c.dropped.Load(),
		Underruns: c.underruns.Load(),
		Restarts:  c.restarts.Load(),
	}
	if p := c.lastErr.Load(); p != nil {
		st.LastError = *p
	}
	return st
}

// New builds the endpoint selected by cfg.Mode.
func New(cfg config.Config, log *slog.Logger) (Endpoint, error) {
	switch cfg.Mode {
	case config.ModeEcho:
		return newEcho(log), nil
	case config.ModeVirtual:
		return newVirtual(cfg, log)
	default:
		return nil, fmt.Errorf("unsupported bridge mode %q", cfg.Mode)
	}
}

// Preflight validates that the selected backend can run in this environment.
func Preflight(cfg config.Config) error {
	if cfg.Mode != config.ModeVirtual {
		return nil
	}
	return preflightVirtual(cfg)
}

// ErrNotStarted is returned when an endpoint is used before Start.
var ErrNotStarted = errors.New("audio endpoint not started")

// errVirtualUnavailable is returned when the platform lacks PulseAudio support.
var errVirtualUnavailable = errors.New("virtual audio mode requires Linux with PulseAudio or PipeWire")
