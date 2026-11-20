package bridge

import (
	"context"
	"log/slog"
)

// echo returns inbound audio to Asterisk unchanged. Frames are forwarded
// immediately, so added latency is a single goroutine hand-off.
type echo struct {
	log  *slog.Logger
	c    counters
	sink Sink
}

func newEcho(log *slog.Logger) *echo { return &echo{log: log} }

func (e *echo) Name() string { return "echo" }

func (e *echo) Start(_ context.Context, sink Sink) error {
	if sink == nil {
		return ErrNotStarted
	}
	e.sink = sink
	e.c.ready.Store(true)
	return nil
}

func (e *echo) Write(pcm []byte) error {
	if e.sink == nil {
		return ErrNotStarted
	}
	e.c.inFrames.Add(1)
	e.c.inBytes.Add(uint64(len(pcm)))
	// Byte-for-byte passthrough preserves audio integrity.
	e.sink(pcm)
	e.c.outFrames.Add(1)
	e.c.outBytes.Add(uint64(len(pcm)))
	return nil
}

func (e *echo) Stats() Stats { return e.c.snapshot("echo") }

func (e *echo) Close() error {
	e.c.ready.Store(false)
	e.sink = nil
	return nil
}
