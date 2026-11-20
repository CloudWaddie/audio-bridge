package audio

import (
	"context"
	"errors"
	"sync"
)

// ErrClosed is returned when reading from a closed buffer or queue.
var ErrClosed = errors.New("audio buffer closed")

// Buffer is a bounded, concurrency-safe byte ring. It is used to decouple the
// producer of PCM data from its consumer and to absorb jitter. When full it
// drops the oldest bytes so that latency stays bounded instead of growing.
type Buffer struct {
	mu       sync.Mutex
	buf      []byte
	head     int
	size     int
	closed   bool
	dropped  uint64
	written  uint64
	readByte uint64
	notify   chan struct{}
}

// NewBuffer creates a ring holding at most capacity bytes.
func NewBuffer(capacity int) *Buffer {
	if capacity < 1 {
		capacity = 1
	}
	return &Buffer{buf: make([]byte, capacity), notify: make(chan struct{}, 1)}
}

// Capacity returns the ring size in bytes.
func (b *Buffer) Capacity() int { return len(b.buf) }

func (b *Buffer) signal() {
	select {
	case b.notify <- struct{}{}:
	default:
	}
}

// Write appends p, discarding the oldest bytes when the ring overflows. It
// returns the number of bytes accepted and the number of bytes dropped.
func (b *Buffer) Write(p []byte) (int, int) {
	if len(p) == 0 {
		return 0, 0
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, 0
	}
	dropped := 0
	if len(p) > len(b.buf) {
		skip := len(p) - len(b.buf)
		p = p[skip:]
		dropped += skip
	}
	if overflow := b.size + len(p) - len(b.buf); overflow > 0 {
		b.head = (b.head + overflow) % len(b.buf)
		b.size -= overflow
		dropped += overflow
	}
	tail := b.head + b.size
	if tail >= len(b.buf) {
		tail -= len(b.buf)
	}
	n := copy(b.buf[tail:], p)
	if n < len(p) {
		copy(b.buf, p[n:])
	}
	b.size += len(p)
	b.dropped += uint64(dropped)
	b.written += uint64(len(p))
	b.mu.Unlock()
	b.signal()
	return len(p), dropped
}

// ReadAvailable copies up to len(p) bytes without blocking and reports how
// many bytes were copied.
func (b *Buffer) ReadAvailable(p []byte) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.copyOut(p)
}

// copyOut must be called with the lock held.
func (b *Buffer) copyOut(p []byte) int {
	n := len(p)
	if n > b.size {
		n = b.size
	}
	if n == 0 {
		return 0
	}
	first := copy(p[:n], b.buf[b.head:])
	if first < n {
		copy(p[first:n], b.buf)
	}
	b.head = (b.head + n) % len(b.buf)
	b.size -= n
	b.readByte += uint64(n)
	return n
}

// Read blocks until at least one byte is available, the context is done or
// the buffer is closed.
func (b *Buffer) Read(ctx context.Context, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		b.mu.Lock()
		if b.size > 0 {
			n := b.copyOut(p)
			b.mu.Unlock()
			return n, nil
		}
		closed := b.closed
		b.mu.Unlock()
		if closed {
			return 0, ErrClosed
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-b.notify:
		}
	}
}

// ReadExact fills p completely, blocking as needed.
func (b *Buffer) ReadExact(ctx context.Context, p []byte) (int, error) {
	n := 0
	for n < len(p) {
		got, err := b.Read(ctx, p[n:])
		n += got
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// Len returns the number of buffered bytes.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.size
}

// Dropped reports the number of bytes discarded due to overflow.
func (b *Buffer) Dropped() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// Written reports the number of bytes accepted.
func (b *Buffer) Written() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.written
}

// ReadBytes reports the number of bytes consumed.
func (b *Buffer) ReadBytes() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.readByte
}

// Close wakes blocked readers and rejects further writes.
func (b *Buffer) Close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.signal()
}

// FrameQueue is a bounded FIFO of audio frames. It preserves frame boundaries,
// which matters for echo fidelity, and drops the oldest frame on overflow so
// that added latency stays bounded.
type FrameQueue struct {
	mu      sync.Mutex
	items   [][]byte
	capacity int
	closed  bool
	dropped uint64
	notify  chan struct{}
}

// NewFrameQueue creates a queue holding at most capacity frames.
func NewFrameQueue(capacity int) *FrameQueue {
	if capacity < 1 {
		capacity = 1
	}
	return &FrameQueue{capacity: capacity, notify: make(chan struct{}, 1)}
}

func (q *FrameQueue) signal() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// Push copies p into the queue. It never blocks. When the queue is full the
// oldest frame is discarded and dropped reports true.
func (q *FrameQueue) Push(p []byte) (dropped bool) {
	if len(p) == 0 {
		return false
	}
	frame := make([]byte, len(p))
	copy(frame, p)

	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return true
	}
	if len(q.items) >= q.capacity {
		q.items = q.items[1:]
		q.dropped++
		dropped = true
	}
	q.items = append(q.items, frame)
	q.mu.Unlock()
	q.signal()
	return dropped
}

// Pop blocks until a frame is available, the context is done or the queue is
// closed.
func (q *FrameQueue) Pop(ctx context.Context) ([]byte, error) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			frame := q.items[0]
			q.items[0] = nil
			q.items = q.items[1:]
			q.mu.Unlock()
			return frame, nil
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return nil, ErrClosed
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-q.notify:
		}
	}
}

// Len returns the number of queued frames.
func (q *FrameQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Dropped reports the number of frames discarded due to overflow.
func (q *FrameQueue) Dropped() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

// Close wakes blocked readers and rejects further pushes.
func (q *FrameQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.items = nil
	q.mu.Unlock()
	q.signal()
}
