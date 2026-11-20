package audiosocket

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"math"
	"net"
	"testing"
	"time"

	"github.com/cloudwaddie/audiosocket-bridge/internal/bridge"
	"github.com/cloudwaddie/audiosocket-bridge/internal/config"
)

func newTestServer(t *testing.T, uuidTimeout time.Duration) (*Server, func()) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := NewServer(ServerConfig{
		Mode:       "echo",
		ListenAddr: "127.0.0.1:0",
		Session: SessionConfig{
			UUIDTimeout:       uuidTimeout,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      2 * time.Second,
			MaxFrameBytes:     4096,
			ReadBufferBytes:   4096,
			OutQueueFrames:    50,
			FrameDuration:     20 * time.Millisecond,
			CallRate:          8000,
			CollectAudioStats: true,
		},
	}, log, func() (bridge.Endpoint, error) {
		return bridge.New(config.Config{Mode: config.ModeEcho}, log)
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for srv.Addr() == "127.0.0.1:0" {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("server did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}

	stop := func() {
		cancel()
		shutdownCtx, done := context.WithTimeout(context.Background(), 2*time.Second)
		defer done()
		_ = srv.Shutdown(shutdownCtx)
	}
	t.Cleanup(stop)
	return srv, stop
}

func testTone(n int) []byte {
	buf := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/8000))
		buf[i*2] = byte(uint16(v))
		buf[i*2+1] = byte(uint16(v) >> 8)
	}
	return buf
}

func dialCall(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, bufio.NewReader(conn)
}

func sendUUID(t *testing.T, conn net.Conn) {
	t.Helper()
	uuid := []byte{0x40, 0x9c, 0x8f, 0x2a, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc}
	frame := AppendFrame(nil, KindUUID, uuid)
	// Split the frame across writes to exercise partial TCP reads.
	for _, part := range [][]byte{frame[:1], frame[1:3], frame[3:9], frame[9:]} {
		if _, err := conn.Write(part); err != nil {
			t.Fatalf("write uuid: %v", err)
		}
	}
}

func readFrame(t *testing.T, r *bufio.Reader, conn net.Conn) (Kind, []byte) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	kind, payload, err := ReadFrame(r, 4096, nil)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return kind, payload
}

func TestEchoCallPreservesAudio(t *testing.T) {
	srv, _ := newTestServer(t, time.Second)
	conn, r := dialCall(t, srv.Addr())
	sendUUID(t, conn)

	tone := testTone(160)
	for i := 0; i < 10; i++ {
		if _, err := conn.Write(AppendFrame(nil, KindAudio, tone)); err != nil {
			t.Fatalf("write audio: %v", err)
		}
	}
	for i := 0; i < 10; i++ {
		kind, payload := readFrame(t, r, conn)
		if kind != KindAudio {
			t.Fatalf("frame %d kind = %v", i, kind)
		}
		if !bytes.Equal(payload, tone) {
			t.Fatalf("frame %d payload differs", i)
		}
	}

	snap := srv.Snapshot()
	if snap.ActiveCall == nil {
		t.Fatal("expected an active call")
	}
	if snap.ActiveCall.InFrames != 10 || snap.ActiveCall.OutFrames != 10 {
		t.Fatalf("frames in=%d out=%d", snap.ActiveCall.InFrames, snap.ActiveCall.OutFrames)
	}
	if snap.ActiveCall.PeakDBFS == nil {
		t.Fatal("expected audio level statistics")
	}
}

func TestSecondCallRejectedWhileBusy(t *testing.T) {
	srv, _ := newTestServer(t, time.Second)

	first, firstReader := dialCall(t, srv.Addr())
	sendUUID(t, first)
	first.Write(AppendFrame(nil, KindAudio, testTone(160)))
	if kind, _ := readFrame(t, firstReader, first); kind != KindAudio {
		t.Fatalf("first call did not echo, kind=%v", kind)
	}

	second, secondReader := dialCall(t, srv.Addr())
	sendUUID(t, second)
	kind, payload := readFrame(t, secondReader, second)
	if kind != KindError {
		t.Fatalf("second call kind = %v, want error", kind)
	}
	if string(payload) != "busy" {
		t.Fatalf("second call payload = %q", payload)
	}
	if got := srv.Snapshot().Counters.Rejected; got != 1 {
		t.Fatalf("rejected = %d, want 1", got)
	}

	// After the first call ends, a new call must be accepted.
	_ = first.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		third, thirdReader := dialCall(t, srv.Addr())
		sendUUID(t, third)
		third.Write(AppendFrame(nil, KindAudio, testTone(160)))
		kind, _ := readFrame(t, thirdReader, third)
		if kind == KindAudio {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("call not accepted after hangup, kind=%v", kind)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHandshakeTimeoutClosesConnection(t *testing.T) {
	srv, _ := newTestServer(t, 150*time.Millisecond)
	conn, r := dialCall(t, srv.Addr())

	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	if _, _, err := ReadFrame(r, 4096, nil); err == nil {
		t.Fatal("expected connection to be closed after handshake timeout")
	}
	if srv.Snapshot().Counters.Failed != 1 {
		t.Fatalf("failed = %d, want 1", srv.Snapshot().Counters.Failed)
	}
}

func TestGracefulShutdownEndsCall(t *testing.T) {
	srv, stop := newTestServer(t, time.Second)
	conn, r := dialCall(t, srv.Addr())
	sendUUID(t, conn)
	conn.Write(AppendFrame(nil, KindAudio, testTone(160)))
	if kind, _ := readFrame(t, r, conn); kind != KindAudio {
		t.Fatalf("kind = %v", kind)
	}

	stop()

	if srv.Ready() {
		t.Fatal("server still reports ready after shutdown")
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	if _, _, err := ReadFrame(r, 4096, nil); err == nil {
		t.Fatal("expected the call to be closed on shutdown")
	}
}
