// Command audiosocket-tester exercises an AudioSocket endpoint: it sends a
// tone and optionally verifies that audio comes back. It is a diagnostic tool
// for validating the Asterisk/MikoPBX to bridge path without placing a call.
package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cloudwaddie/audiosocket-bridge/internal/audiosocket"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9092", "AudioSocket server address")
	uuidText := flag.String("uuid", "409c8f2a-1122-3344-5566-778899aabbcc", "call UUID")
	duration := flag.Duration("duration", 10*time.Second, "test duration")
	freq := flag.Float64("tone", 440, "tone frequency in Hz")
	rate := flag.Int("rate", 8000, "sample rate in Hz")
	expectEcho := flag.Bool("expect-echo", true, "fail when no audio is returned")
	flag.Parse()

	uuid, err := parseUUID(*uuidText)
	if err != nil {
		fatal(err)
	}
	audioKind, ok := audiosocket.AudioKindForRate(*rate)
	if !ok {
		fatal(fmt.Errorf("unsupported sample rate %d", *rate))
	}

	conn, err := net.DialTimeout("tcp", *addr, 5*time.Second)
	if err != nil {
		fatal(fmt.Errorf("dial %s: %w", *addr, err))
	}
	defer conn.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if _, err := conn.Write(audiosocket.AppendFrame(nil, audiosocket.KindUUID, uuid)); err != nil {
		fatal(fmt.Errorf("send uuid: %w", err))
	}

	var receivedFrames, receivedBytes, readErrors atomic.Uint64
	var lastRMSBits atomic.Uint64
	var firstReply atomic.Int64
	start := time.Now()

	go func() {
		r := bufio.NewReaderSize(conn, 8192)
		for {
			kind, payload, err := audiosocket.ReadFrame(r, 65535, nil)
			if err != nil {
				if ctx.Err() == nil {
					readErrors.Add(1)
				}
				return
			}
			switch {
			case kind == audiosocket.KindHangup:
				return
			case kind.IsAudio():
				if kind != audioKind {
					fmt.Printf("unexpected audio kind 0x%02x\n", uint8(kind))
					continue
				}
				firstReply.CompareAndSwap(0, int64(time.Since(start)))
				receivedFrames.Add(1)
				receivedBytes.Add(uint64(len(payload)))
				if rms, _ := level(payload); !math.IsInf(rms, 0) {
					lastRMSBits.Store(math.Float64bits(rms))
				}
			case kind == audiosocket.KindError:
				fmt.Printf("server error frame: %s\n", strings.TrimSpace(string(payload)))
			}
		}
	}()

	frameBytes := *rate / 50 * 2 // 20 ms
	frame := make([]byte, frameBytes)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	var sentFrames atomic.Uint64
	deadline := time.NewTimer(*duration)
	defer deadline.Stop()
	sample := 0

	fmt.Printf("sending %s of %.0f Hz tone to %s as %s\n", *duration, *freq, *addr, *uuidText)

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-deadline.C:
			break loop
		case <-ticker.C:
			fillTone(frame, sample, *rate, *freq)
			sample += frameBytes / 2
			if _, err := conn.Write(audiosocket.AppendFrame(nil, audioKind, frame)); err != nil {
				fmt.Printf("write failed: %v\n", err)
				break loop
			}
			sentFrames.Add(1)
		}
	}

	time.Sleep(150 * time.Millisecond) // let the tail arrive

	sent := sentFrames.Load()
	got := receivedFrames.Load()
	lost := uint64(0)
	if sent > got {
		lost = sent - got
	}
	fmt.Printf("frames sent=%d received=%d lost=%d bytes_received=%d read_errors=%d\n",
		sent, got, lost, receivedBytes.Load(), readErrors.Load())
	if ts := firstReply.Load(); ts != 0 {
		fmt.Printf("first reply after %s\n", time.Duration(time.Now().UnixNano()-ts))
	}
	if v := math.Float64frombits(lastRMSBits.Load()); v != 0 && !math.IsInf(v, 0) {
		fmt.Printf("last returned frame level: %.1f dBFS\n", v)
	}

	if *expectEcho && got == 0 {
		fmt.Println("FAIL: no audio returned")
		os.Exit(1)
	}
	fmt.Println("OK")
}

func fillTone(dst []byte, start, rate int, freq float64) {
	for i := 0; i < len(dst)/2; i++ {
		v := int16(9000 * math.Sin(2*math.Pi*freq*float64(start+i)/float64(rate)))
		dst[i*2] = byte(uint16(v))
		dst[i*2+1] = byte(uint16(v) >> 8)
	}
}

func level(pcm []byte) (float64, float64) {
	n := len(pcm) / 2
	if n == 0 {
		return math.Inf(-1), math.Inf(-1)
	}
	var sum, peak float64
	for i := 0; i < n; i++ {
		v := float64(int16(uint16(pcm[i*2]) | uint16(pcm[i*2+1])<<8))
		sum += v * v
		if a := math.Abs(v); a > peak {
			peak = a
		}
	}
	toDB := func(v float64) float64 {
		if v <= 0 {
			return math.Inf(-1)
		}
		return 20 * math.Log10(v/32768)
	}
	return toDB(math.Sqrt(sum / float64(n))), toDB(peak)
}

func parseUUID(s string) ([]byte, error) {
	clean := strings.ReplaceAll(strings.TrimSpace(s), "-", "")
	if len(clean) != 32 {
		return nil, fmt.Errorf("uuid must be 32 hex digits, got %q", s)
	}
	b, err := hex.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("invalid uuid: %w", err)
	}
	return b, nil
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "audiosocket-tester: %v\n", err)
	os.Exit(1)
}
