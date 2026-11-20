package audio

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestBufferWriteRead(t *testing.T) {
	b := NewBuffer(64)
	if _, dropped := b.Write([]byte("hello")); dropped != 0 {
		t.Fatalf("dropped = %d", dropped)
	}
	out := make([]byte, 16)
	n, err := b.Read(context.Background(), out)
	if err != nil || string(out[:n]) != "hello" {
		t.Fatalf("read %q err %v", out[:n], err)
	}
}

func TestBufferWrapAround(t *testing.T) {
	b := NewBuffer(16)
	b.Write([]byte("abcdefghij"))
	out := make([]byte, 4)
	b.ReadAvailable(out)
	b.Write([]byte("klmnop"))
	got := make([]byte, 16)
	n := b.ReadAvailable(got)
	if want := "efghijklmnop"; string(got[:n]) != want {
		t.Fatalf("got %q, want %q", got[:n], want)
	}
}

func TestBufferDropsOldest(t *testing.T) {
	b := NewBuffer(8)
	if _, dropped := b.Write([]byte("abcdef")); dropped != 0 {
		t.Fatalf("unexpected drop %d", dropped)
	}
	if _, dropped := b.Write([]byte("ghij")); dropped != 2 {
		t.Fatalf("dropped = %d, want 2", dropped)
	}
	got := make([]byte, 8)
	n := b.ReadAvailable(got)
	if want := "cdefghij"; string(got[:n]) != want {
		t.Fatalf("got %q, want %q", got[:n], want)
	}
	if b.Dropped() != 2 {
		t.Fatalf("Dropped() = %d", b.Dropped())
	}
}

func TestBufferOversizedWriteKeepsTail(t *testing.T) {
	b := NewBuffer(4)
	if _, dropped := b.Write([]byte("abcdefgh")); dropped != 4 {
		t.Fatalf("dropped = %d, want 4", dropped)
	}
	got := make([]byte, 4)
	n := b.ReadAvailable(got)
	if string(got[:n]) != "efgh" {
		t.Fatalf("got %q", got[:n])
	}
}

func TestBufferReadExactCancel(t *testing.T) {
	b := NewBuffer(8)
	b.Write([]byte{1, 2})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	buf := make([]byte, 8)
	n, err := b.ReadExact(ctx, buf)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if n != 2 {
		t.Fatalf("n = %d, want 2", n)
	}
}

func TestBufferCloseUnblocks(t *testing.T) {
	b := NewBuffer(8)
	done := make(chan error, 1)
	go func() {
		_, err := b.Read(context.Background(), make([]byte, 4))
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	b.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err = %v, want ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("read did not unblock")
	}
}

func TestFrameQueueDropsOldest(t *testing.T) {
	q := NewFrameQueue(2)
	q.Push([]byte("one"))
	q.Push([]byte("two"))
	if dropped := q.Push([]byte("three")); !dropped {
		t.Fatal("expected drop")
	}
	ctx := context.Background()
	for _, want := range []string{"two", "three"} {
		got, err := q.Pop(ctx)
		if err != nil {
			t.Fatalf("Pop: %v", err)
		}
		if !bytes.Equal(got, []byte(want)) {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	if q.Dropped() != 1 {
		t.Fatalf("Dropped() = %d", q.Dropped())
	}
}

func TestLevel(t *testing.T) {
	silence := make([]byte, 320)
	rms, peak := Level(silence)
	if rms > -60 || peak > -60 {
		t.Fatalf("silence level rms=%v peak=%v", rms, peak)
	}

	full := make([]byte, 320)
	for i := 0; i < 160; i++ {
		full[i*2] = 0x00
		full[i*2+1] = 0x80 // -32768
	}
	rms, peak = Level(full)
	if rms < -1 || peak < -1 {
		t.Fatalf("full scale level rms=%v peak=%v", rms, peak)
	}
}
