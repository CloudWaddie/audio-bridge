package audiosocket

import (
	"bytes"
	"errors"
	"testing"
	"testing/iotest"
)

func TestFrameRoundTrip(t *testing.T) {
	payload := []byte{1, 2, 3, 4, 5, 6}
	encoded := AppendFrame(nil, KindAudio, payload)

	kind, got, err := ReadFrame(bytes.NewReader(encoded), 4096, nil)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if kind != KindAudio {
		t.Fatalf("kind = %v, want audio", kind)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload = %v, want %v", got, payload)
	}
}

func TestReadFramePartialReads(t *testing.T) {
	var stream []byte
	for i := 0; i < 20; i++ {
		stream = AppendFrame(stream, KindAudio, bytes.Repeat([]byte{byte(i)}, 320))
	}

	r := iotest.OneByteReader(bytes.NewReader(stream))
	for i := 0; i < 20; i++ {
		kind, payload, err := ReadFrame(r, 4096, nil)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if kind != KindAudio || len(payload) != 320 || payload[0] != byte(i) {
			t.Fatalf("frame %d corrupted", i)
		}
	}
}

func TestReadFrameRejectsOversize(t *testing.T) {
	stream := AppendFrame(nil, KindAudio, make([]byte, 64))
	_, _, err := ReadFrame(bytes.NewReader(stream), 32, nil)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}

func TestReadFrameZeroLength(t *testing.T) {
	stream := AppendFrame(nil, KindDTMF, nil)
	kind, payload, err := ReadFrame(bytes.NewReader(stream), 32, nil)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if kind != KindDTMF || len(payload) != 0 {
		t.Fatalf("got kind=%v len=%d", kind, len(payload))
	}
}

func TestUUIDString(t *testing.T) {
	raw := []byte{0x40, 0x9c, 0x8f, 0x2a, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc}
	want := "409c8f2a-1122-3344-5566-778899aabbcc"

	got, err := UUIDString(raw)
	if err != nil {
		t.Fatalf("UUIDString: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	text, err := UUIDString([]byte(want))
	if err != nil {
		t.Fatalf("text UUID: %v", err)
	}
	if text != want {
		t.Fatalf("text got %q, want %q", text, want)
	}

	if _, err := UUIDString([]byte("not-a-uuid")); err == nil {
		t.Fatal("expected error for invalid uuid")
	}
	if _, err := UUIDString(raw[:15]); err == nil {
		t.Fatal("expected error for short uuid")
	}
}
