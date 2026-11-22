package audiosocket

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwaddie/audiosocket-bridge/internal/audio"
	"github.com/cloudwaddie/audiosocket-bridge/internal/bridge"
	"github.com/cloudwaddie/audiosocket-bridge/internal/prompts"
)

// SessionConfig holds the per-connection tunables.
type SessionConfig struct {
	UUIDTimeout       time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	MaxFrameBytes     int
	ReadBufferBytes   int
	OutQueueFrames    int
	MaxCallDuration   time.Duration
	FrameDuration     time.Duration
	CallRate          int
	CollectAudioStats bool
	// Prompts plays the baked call prompts (connected, error, busy) to the
	// caller.
	Prompts bool
}

// Session owns a single AudioSocket call. One session is served by one
// goroutine for reading and one for writing; the audio endpoint is attached by
// the server after the UUID frame has been validated.
type Session struct {
	conn net.Conn
	writeMu sync.Mutex // serialize audio and hangup writes
	hangupRequested atomic.Bool
	cfg  SessionConfig
	log  *slog.Logger
	ep   bridge.Endpoint
	out  *audio.FrameQueue

	uuid      string
	remote    string
	startedAt time.Time
	endedAt   time.Time
	audioKind Kind

	reader  *bufio.Reader
	scratch []byte

	inFrames      atomic.Uint64
	inBytes       atomic.Uint64
	outFrames     atomic.Uint64
	outBytes      atomic.Uint64
	promptFrames  atomic.Uint64
	dtmfCount     atomic.Uint64
	unsupported   atomic.Uint64
	malformed     atomic.Uint64
	dropped       atomic.Uint64
	writeErrors   atomic.Uint64
	rmsDB         atomic.Uint64
	peakDB        atomic.Uint64
	lastErr       atomic.Pointer[string]
	keypadCounts  [10]atomic.Uint64
	micMuted      atomic.Bool
	micCheckPeak  atomic.Uint64
	echoUntil     atomic.Int64
	keypadPrefix  time.Time // owned by readLoop
	micCheckUntil time.Time // owned by readLoop
}

// NewSession wraps an accepted connection.
func NewSession(conn net.Conn, cfg SessionConfig, log *slog.Logger) *Session {
	s := &Session{
		conn:   conn,
		cfg:    cfg,
		log:    log,
		out:    audio.NewFrameQueue(cfg.OutQueueFrames),
		remote: conn.RemoteAddr().String(),
	}
	kind, ok := AudioKindForRate(cfg.CallRate)
	if !ok {
		kind = KindAudio
	}
	s.audioKind = kind
	s.rmsDB.Store(math.Float64bits(math.Inf(-1)))
	s.peakDB.Store(math.Float64bits(math.Inf(-1)))
	s.micCheckPeak.Store(math.Float64bits(math.Inf(-1)))
	return s
}

// Identify reads and validates the mandatory UUID frame. It must be called
// before Attach and Run.
func (s *Session) Identify() error {
	s.reader = bufio.NewReaderSize(s.conn, s.cfg.ReadBufferBytes)
	if s.cfg.UUIDTimeout > 0 {
		_ = s.conn.SetReadDeadline(time.Now().Add(s.cfg.UUIDTimeout))
	}
	kind, payload, err := ReadFrame(s.reader, s.cfg.MaxFrameBytes, s.scratch)
	if err != nil {
		return fmt.Errorf("read uuid frame: %w", err)
	}
	if kind != KindUUID {
		return fmt.Errorf("first frame is %s, want uuid", kind)
	}
	uuid, err := UUIDString(payload)
	if err != nil {
		return err
	}
	s.uuid = uuid
	s.scratch = payload[:0]
	_ = s.conn.SetReadDeadline(time.Time{})
	s.log = s.log.With("uuid", uuid)
	return nil
}

// Attach binds the audio endpoint for this call.
func (s *Session) Attach(ep bridge.Endpoint) { s.ep = ep }

// UUID returns the identified call UUID.
func (s *Session) UUID() string { return s.uuid }

// Reject reports why the call cannot be served and asks the peer to hang up.
// Asterisk treats the error frame as fatal, so the hangup frame is best effort.
func (s *Session) Reject(reason string) {
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
	buf := AppendFrame(nil, KindError, []byte(reason))
	buf = AppendHangup(buf)
	_, _ = s.conn.Write(buf)
}

// Hangup requests a remote hangup for this call and unblocks the reader.
// It is safe to call repeatedly and concurrently with the audio writer.
func (s *Session) Hangup() {
	if !s.hangupRequested.CompareAndSwap(false, true) {
		return
	}
	s.writeMu.Lock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := s.conn.Write(AppendHangup(nil))
	s.writeMu.Unlock()
	if err != nil {
		s.log.Warn("could not send AudioSocket hangup frame", "err", err)
	}
	_ = s.conn.Close()
}

// PlayPrompt plays a baked prompt to the caller, best effort. It is meant
// for paths where no audio endpoint runs, such as the busy rejection.
func (s *Session) PlayPrompt(name string) {
	pcm, ok := prompts.AtRate(name, s.cfg.CallRate)
	if !ok {
		return
	}
	s.playPrompt(pcm)
}

// playPrompt writes prompt PCM to the peer, one frame per frame interval,
// so the caller hears it at real-time speed. Errors end the playback.
func (s *Session) playPrompt(pcm []byte) {
	if len(pcm) == 0 {
		return
	}
	frameBytes := s.frameBytes()
	buf := make([]byte, 0, HeaderSize+frameBytes)
	for off := 0; off < len(pcm); off += frameBytes {
		end := off + frameBytes
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := s.conn.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout)); err != nil {
			return
		}
		buf = AppendFrame(buf[:0], s.audioKind, pcm[off:end])
		if _, err := s.conn.Write(buf); err != nil {
			return
		}
		s.outFrames.Add(1)
		s.outBytes.Add(uint64(end - off))
		s.promptFrames.Add(1)
		time.Sleep(s.cfg.FrameDuration)
	}
	_ = s.conn.SetWriteDeadline(time.Time{})
}

// frameBytes returns the outbound frame size in bytes.
func (s *Session) frameBytes() int {
	bytes := int(int64(s.cfg.CallRate)*s.cfg.FrameDuration.Milliseconds()/1000) * 2
	if bytes < 2 {
		// Degenerate config guard: 20 ms at 8 kHz.
		return 320
	}
	return bytes
}

// Run serves the call until the peer hangs up, the context is cancelled or the
// maximum call duration is reached.
func (s *Session) Run(ctx context.Context) error {
	if s.ep == nil {
		return errors.New("audiosocket: no audio endpoint attached")
	}
	if s.reader == nil {
		return errors.New("audiosocket: session not identified")
	}

	runCtx := ctx
	if s.cfg.MaxCallDuration > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, s.cfg.MaxCallDuration)
		defer cancel()
	}

	// The connected prompt plays before the endpoint starts so it reaches the
	// caller unmixed and the endpoint stream begins cleanly afterwards.
	if s.cfg.Prompts {
		s.PlayPrompt(prompts.Connected)
	}

	if err := s.ep.Start(runCtx, func(pcm []byte) {
		if time.Now().UnixNano() >= s.echoUntil.Load() {
			s.enqueueOutbound(pcm)
		}
	}); err != nil {
		if s.cfg.Prompts {
			s.PlayPrompt(prompts.Error)
		}
		s.setError(err)
		return fmt.Errorf("start audio endpoint: %w", err)
	}
	s.startedAt = time.Now()
	s.log.Info("call started", "endpoint", s.ep.Name())

	// Closing the connection is what unblocks the reader, so tie it to the
	// call context. Graceful shutdown and max-duration both rely on this.
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			_ = s.conn.Close()
		case <-watchDone:
		}
	}()
	defer close(watchDone)

	writeCtx, stopWriter := context.WithCancel(runCtx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.writeLoop(writeCtx)
	}()

	readErr := s.readLoop(runCtx)

	s.out.Close()
	stopWriter()
	wg.Wait()

	if err := s.ep.Close(); err != nil {
		s.log.Warn("audio endpoint shutdown", "err", err)
	}
	s.endedAt = time.Now()

	reason := "hangup"
	level := slog.LevelInfo
	switch {
	case readErr == nil:
	case errors.Is(readErr, context.Canceled):
		reason = "shutdown"
	case errors.Is(readErr, context.DeadlineExceeded):
		reason = "max_duration"
	default:
		reason = "error"
		level = slog.LevelWarn
		s.setError(readErr)
	}

	s.log.Log(ctx, level, "call ended",
		"reason", reason,
		"duration_ms", s.durationMS(),
		"frames_in", s.inFrames.Load(),
		"frames_out", s.outFrames.Load(),
		"bytes_in", s.inBytes.Load(),
		"bytes_out", s.outBytes.Load(),
		"dropped_frames", s.dropped.Load(),
		"dtmf", s.dtmfCount.Load(),
	)
	return readErr
}

func (s *Session) readLoop(ctx context.Context) error {
	scratch := s.scratch
	if scratch == nil {
		scratch = make([]byte, 0, s.cfg.MaxFrameBytes)
	}
	for {
		if s.cfg.ReadTimeout > 0 {
			_ = s.conn.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
		}
		kind, payload, err := ReadFrame(s.reader, s.cfg.MaxFrameBytes, scratch)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return fmt.Errorf("read timeout after %s", s.cfg.ReadTimeout)
			}
			return err
		}
		scratch = payload[:0]

		switch kind {
		case s.audioKind:
			s.handleAudio(payload)
		case KindHangup:
			s.log.Info("peer requested hangup")
			return nil
		case KindDTMF:
			s.dtmfCount.Add(1)
			s.handleKeypad(payload, time.Now())
		case KindError:
			s.log.Warn("peer reported error", "code", strings.TrimSpace(string(payload)))
		case KindUUID:
			s.log.Warn("duplicate uuid frame ignored")
		default:
			if kind.IsAudio() {
				// A rate we did not negotiate; dropping it is safer than
				// feeding mismatched audio into the call.
				s.log.Warn("audio frame with unexpected rate", "kind", kind.String(), "want", s.audioKind.String())
			}
			s.unsupported.Add(1)
		}
	}
}

// handleKeypad recognizes one two-digit command at a time. The prefix
// expires after a second and is never interpreted as a standalone action.
func (s *Session) handleKeypad(payload []byte, now time.Time) {
	if len(payload) != 1 {
		s.keypadPrefix = time.Time{}
		return
	}
	digit := payload[0]
	if s.keypadPrefix.IsZero() || now.Sub(s.keypadPrefix) > time.Second {
		s.keypadPrefix = time.Time{}
		if digit == '9' {
			s.keypadPrefix = now
		}
		return
	}
	s.keypadPrefix = time.Time{}
	switch digit {
	case '0', '2', '9':
	case '1':
		s.micMuted.Store(!s.micMuted.Load())
	case '3':
		s.micCheckUntil = now.Add(12 * time.Second)
		s.micCheckPeak.Store(math.Float64bits(math.Inf(-1)))
	case '4':
		s.echoUntil.Store(now.Add(5 * time.Second).UnixNano())
	default:
		return
	}
	s.keypadCounts[digit-'0'].Add(1)
}

func (s *Session) handleAudio(payload []byte) {
	if len(payload) == 0 {
		return
	}
	if len(payload)%2 != 0 {
		s.malformed.Add(1)
		payload = payload[:len(payload)-1]
		if len(payload) == 0 {
			return
		}
	}
	s.inFrames.Add(1)
	s.inBytes.Add(uint64(len(payload)))
	micCheck := time.Now().Before(s.micCheckUntil)
	if s.cfg.CollectAudioStats || micCheck {
		rms, peak := audio.Level(payload)
		if s.cfg.CollectAudioStats {
			s.rmsDB.Store(math.Float64bits(rms))
			s.storePeak(peak)
		}
		if micCheck && peak > math.Float64frombits(s.micCheckPeak.Load()) {
			s.micCheckPeak.Store(math.Float64bits(peak))
		}
	}
	if time.Now().UnixNano() < s.echoUntil.Load() {
		s.enqueueOutbound(payload)
		return
	}
	if s.micMuted.Load() {
		// Keep the input clock running while withholding the caller's audio.
		clear(payload)
	}
	if err := s.ep.Write(payload); err != nil {
		s.setError(err)
		s.log.Warn("audio endpoint write failed", "err", err)
	}
}

func (s *Session) storePeak(peak float64) {
	for {
		old := s.peakDB.Load()
		if peak <= math.Float64frombits(old) {
			return
		}
		if s.peakDB.CompareAndSwap(old, math.Float64bits(peak)) {
			return
		}
	}
}

// enqueueOutbound is the sink handed to the audio endpoint. It must not block.
func (s *Session) enqueueOutbound(pcm []byte) {
	if len(pcm) == 0 {
		return
	}
	if s.out.Push(pcm) {
		s.dropped.Add(1)
	}
	s.outFrames.Add(1)
	s.outBytes.Add(uint64(len(pcm)))
}

func (s *Session) writeLoop(ctx context.Context) {
	buf := make([]byte, 0, HeaderSize+s.cfg.MaxFrameBytes)
	for {
		payload, err := s.out.Pop(ctx)
		if err != nil {
			return
		}
		buf = AppendFrame(buf[:0], s.audioKind, payload)
		s.writeMu.Lock()
		if s.hangupRequested.Load() {
			s.writeMu.Unlock()
			return
		}
		deadlineErr := s.conn.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
		var writeErr error
		if deadlineErr == nil {
			_, writeErr = s.conn.Write(buf)
		}
		s.writeMu.Unlock()
		if deadlineErr != nil {
			return
		}
		if err := writeErr; err != nil {
			s.writeErrors.Add(1)
			if ctx.Err() == nil {
				s.setError(err)
				s.log.Warn("write to peer failed", "err", err)
			}
			// Unblock the reader; the call is over.
			_ = s.conn.Close()
			return
		}
	}
}

func (s *Session) setError(err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	s.lastErr.Store(&msg)
}

func (s *Session) durationMS() int64 {
	end := s.endedAt
	if end.IsZero() {
		end = time.Now()
	}
	if s.startedAt.IsZero() {
		return 0
	}
	return end.Sub(s.startedAt).Milliseconds()
}

// CallSnapshot is a point-in-time view of the call. It contains aggregates
// only, never audio samples.
type CallSnapshot struct {
	UUID            string        `json:"uuid"`
	RemoteAddr      string        `json:"remote_addr"`
	Endpoint        string        `json:"endpoint"`
	StartedAt       time.Time     `json:"started_at"`
	DurationSeconds float64       `json:"duration_seconds"`
	InFrames        uint64        `json:"in_frames"`
	InBytes         uint64        `json:"in_bytes"`
	OutFrames       uint64        `json:"out_frames"`
	OutBytes        uint64        `json:"out_bytes"`
	DroppedFrames   uint64        `json:"dropped_frames"`
	WriteErrors     uint64        `json:"write_errors"`
	DTMF            uint64        `json:"dtmf_digits"`
	KeypadCounts    [10]uint64    `json:"keypad_counts"`
	EchoActive      bool          `json:"echo_active"`
	MicMuted        bool          `json:"mic_muted"`
	MicCheckPeak    *float64      `json:"mic_check_peak_dbfs,omitempty"`
	CallRate        int           `json:"call_rate"`
	AudioStats      bool          `json:"audio_stats_enabled"`
	Unsupported     uint64        `json:"unsupported_frames"`
	Malformed       uint64        `json:"malformed_frames"`
	PromptFrames    uint64        `json:"prompt_frames"`
	LastRMSDBFS     *float64      `json:"last_rms_dbfs,omitempty"`
	PeakDBFS        *float64      `json:"peak_dbfs,omitempty"`
	LastError       string        `json:"last_error,omitempty"`
	EndpointStats   *bridge.Stats `json:"endpoint_stats,omitempty"`
}

// Snapshot returns the current call metrics.
func (s *Session) Snapshot() CallSnapshot {
	snap := CallSnapshot{
		UUID:            s.uuid,
		RemoteAddr:      s.remote,
		StartedAt:       s.startedAt,
		DurationSeconds: float64(s.durationMS()) / 1000,
		InFrames:        s.inFrames.Load(),
		InBytes:         s.inBytes.Load(),
		OutFrames:       s.outFrames.Load(),
		OutBytes:        s.outBytes.Load(),
		DroppedFrames:   s.dropped.Load(),
		WriteErrors:     s.writeErrors.Load(),
		DTMF:            s.dtmfCount.Load(),
		EchoActive:      time.Now().UnixNano() < s.echoUntil.Load(),
		MicMuted:        s.micMuted.Load(),
		CallRate:        s.cfg.CallRate,
		AudioStats:      s.cfg.CollectAudioStats,
		Unsupported:     s.unsupported.Load(),
		Malformed:       s.malformed.Load(),
		PromptFrames:    s.promptFrames.Load(),
	}
	for index := range snap.KeypadCounts {
		snap.KeypadCounts[index] = s.keypadCounts[index].Load()
	}
	if peak := math.Float64frombits(s.micCheckPeak.Load()); !math.IsInf(peak, 0) {
		snap.MicCheckPeak = &peak
	}
	if s.ep != nil {
		snap.Endpoint = s.ep.Name()
		ep := s.ep.Stats()
		snap.EndpointStats = &ep
	}
	if s.cfg.CollectAudioStats {
		if v := math.Float64frombits(s.rmsDB.Load()); !math.IsInf(v, 0) {
			snap.LastRMSDBFS = &v
		}
		if v := math.Float64frombits(s.peakDB.Load()); !math.IsInf(v, 0) {
			snap.PeakDBFS = &v
		}
	}
	if p := s.lastErr.Load(); p != nil {
		snap.LastError = *p
	}
	return snap
}
