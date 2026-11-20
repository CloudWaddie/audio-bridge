package audiosocket

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// The golden frames in this file come from Asterisk's own implementation:
// include/asterisk/res_audiosocket.h for the kind values and
// res/res_audiosocket.c for the bytes written by ast_audiosocket_init and
// ast_audiosocket_send_frame. They are intentionally written as literals so a
// constant change in this package cannot silently update its own expectations.

func TestKindValuesMatchAsterisk(t *testing.T) {
	cases := []struct {
		name string
		kind Kind
		want byte
	}{
		{"hangup", KindHangup, 0x00},
		{"uuid", KindUUID, 0x01},
		{"dtmf", KindDTMF, 0x03},
		{"audio 8 kHz", KindAudio, 0x10},
		{"audio 12 kHz", KindAudioSlin12, 0x11},
		{"audio 16 kHz", KindAudioSlin16, 0x12},
		{"audio 24 kHz", KindAudioSlin24, 0x13},
		{"audio 32 kHz", KindAudioSlin32, 0x14},
		{"audio 44.1 kHz", KindAudioSlin44, 0x15},
		{"audio 48 kHz", KindAudioSlin48, 0x16},
		{"audio 96 kHz", KindAudioSlin96, 0x17},
		{"audio 192 kHz", KindAudioSlin192, 0x18},
		{"error", KindError, 0xFF},
	}
	for _, tc := range cases {
		if byte(tc.kind) != tc.want {
			t.Errorf("%s: kind = 0x%02x, want 0x%02x", tc.name, byte(tc.kind), tc.want)
		}
	}
}

func TestAsteriskUUIDFrame(t *testing.T) {
	uuid := []byte{
		0x40, 0x9c, 0x8f, 0x2a, 0x11, 0x22, 0x33, 0x44,
		0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc,
	}
	// ast_audiosocket_init: kind 0x01, length 0x0010, then the raw UUID.
	wire := append([]byte{0x01, 0x00, 0x10}, uuid...)

	kind, payload, err := ReadFrame(bytes.NewReader(wire), 65535, nil)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if kind != KindUUID {
		t.Fatalf("kind = 0x%02x, want 0x01", byte(kind))
	}
	if !bytes.Equal(payload, uuid) {
		t.Fatalf("payload = %x", payload)
	}
	if got := AppendFrame(nil, KindUUID, uuid); !bytes.Equal(got, wire) {
		t.Fatalf("encoded = %x, want %x", got, wire)
	}
	if got, err := UUIDString(payload); err != nil || got != "409c8f2a-1122-3344-5566-778899aabbcc" {
		t.Fatalf("UUIDString = %q, %v", got, err)
	}
}

func TestAsteriskAudioFrame(t *testing.T) {
	payload := bytes.Repeat([]byte{0x11, 0x22}, 160) // 320 bytes, 20 ms at 8 kHz
	wire := append([]byte{0x10, 0x01, 0x40}, payload...)

	kind, got, err := ReadFrame(bytes.NewReader(wire), 65535, nil)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if kind != KindAudio {
		t.Fatalf("kind = 0x%02x, want 0x10", byte(kind))
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("payload mismatch")
	}
	if !bytes.Equal(AppendFrame(nil, KindAudio, payload), wire) {
		t.Fatal("encoded audio frame mismatch")
	}
}

func TestAsteriskDTMFFrame(t *testing.T) {
	wire := []byte{0x03, 0x00, 0x01, '5'}
	kind, payload, err := ReadFrame(bytes.NewReader(wire), 65535, nil)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if kind != KindDTMF || string(payload) != "5" {
		t.Fatalf("kind = 0x%02x payload = %q", byte(kind), payload)
	}
	if !bytes.Equal(AppendFrame(nil, KindDTMF, []byte("5")), wire) {
		t.Fatal("encoded DTMF frame mismatch")
	}
}

func TestAsteriskHangupFrame(t *testing.T) {
	wire := []byte{0x00, 0x00, 0x00}
	kind, payload, err := ReadFrame(bytes.NewReader(wire), 65535, nil)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if kind != KindHangup || len(payload) != 0 {
		t.Fatalf("kind = 0x%02x len = %d", byte(kind), len(payload))
	}
	if !bytes.Equal(AppendHangup(nil), wire) {
		t.Fatal("encoded hangup frame mismatch")
	}
}

func TestAsteriskErrorFrame(t *testing.T) {
	wire := []byte{0xFF, 0x00, 0x04, 'b', 'u', 's', 'y'}
	kind, payload, err := ReadFrame(bytes.NewReader(wire), 65535, nil)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if kind != KindError || string(payload) != "busy" {
		t.Fatalf("kind = 0x%02x payload = %q", byte(kind), payload)
	}
	if !bytes.Equal(AppendFrame(nil, KindError, []byte("busy")), wire) {
		t.Fatal("encoded error frame mismatch")
	}
}

func TestAudioKindRateMapping(t *testing.T) {
	rates := []int{8000, 12000, 16000, 24000, 32000, 44100, 48000, 96000, 192000}
	for _, rate := range rates {
		kind, ok := AudioKindForRate(rate)
		if !ok {
			t.Fatalf("rate %d not mapped", rate)
		}
		if got, ok := kind.Rate(); !ok || got != rate {
			t.Fatalf("kind 0x%02x maps to %d, want %d", byte(kind), got, rate)
		}
	}
	if _, ok := AudioKindForRate(22050); ok {
		t.Fatal("22050 should not be a valid AudioSocket rate")
	}
}

// TestIdentifyRejectsLegacyUUIDKind pins the regression: the old scheme used
// 0x00 for UUID, which is now hangup. Such a handshake must be rejected.
func TestIdentifyRejectsLegacyUUIDKind(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sess := NewSession(server, SessionConfig{
		UUIDTimeout:     time.Second,
		MaxFrameBytes:   4096,
		ReadBufferBytes: 4096,
		OutQueueFrames:  4,
	}, log)

	legacy := append([]byte{0x00, 0x00, 0x10}, make([]byte, 16)...)
	go func() { _, _ = client.Write(legacy) }()

	if err := sess.Identify(); err == nil {
		t.Fatal("expected the legacy 0x00 UUID frame to be rejected")
	}
}
