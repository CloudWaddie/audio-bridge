// Package logging builds the structured logger and helpers for capturing
// subprocess diagnostics.
package logging

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
)

// New returns a structured logger writing to w.
func New(level slog.Level, format string, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				a.Value = slog.TimeValue(a.Value.Time().UTC())
			}
			return a
		},
	}
	var h slog.Handler
	if strings.EqualFold(format, "text") {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h).With("app", "audiosocket-bridge")
}

// LineWriter turns a subprocess's stderr into one log record per line. Lines
// are truncated so a misbehaving child cannot flood the log.
type LineWriter struct {
	log   *slog.Logger
	level slog.Level
	mu    sync.Mutex
	buf   []byte
	max   int
}

// NewLineWriter creates a writer that logs complete lines under the given
// component name.
func NewLineWriter(log *slog.Logger, level slog.Level, component string) *LineWriter {
	return &LineWriter{
		log:   log.With("component", component),
		level: level,
		max:   1024,
	}
}

// Write implements io.Writer.
func (w *LineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
		if line != "" {
			w.log.Log(context.Background(), w.level, "subprocess output", "line", truncate(line, w.max))
		}
	}
	if len(w.buf) > w.max {
		w.buf = w.buf[:w.max]
	}
	return len(p), nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
