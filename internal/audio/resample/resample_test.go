package resample

import (
	"math"
	"testing"
)

func sine(rate int, freq float64, n int, amp float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(amp * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)))
	}
	return out
}

func rms(s []int16) float64 {
	var sum float64
	for _, v := range s {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(s)))
}

func TestPassthrough(t *testing.T) {
	r := New(8000, 8000)
	in := sine(8000, 440, 160, 1000)
	out := r.Process(in)
	if len(out) != len(in) || &out[0] != &in[0] {
		t.Fatal("expected passthrough to return the input slice")
	}
}

func TestDownsampleAmplitude(t *testing.T) {
	r := New(48000, 8000)
	in := sine(48000, 1000, 48000, 10000) // 1 second
	out := r.Process(in)

	if len(out) < 7900 || len(out) > 8050 {
		t.Fatalf("output length = %d, want about 8000", len(out))
	}
	// Skip the filter warm-up.
	got := rms(out[200:])
	want := 10000 / math.Sqrt2
	if math.Abs(got-want)/want > 0.05 {
		t.Fatalf("rms = %.1f, want %.1f", got, want)
	}

	// Count positive zero crossings to confirm the frequency.
	crossings := 0
	for i := 1; i < len(out); i++ {
		if out[i-1] < 0 && out[i] >= 0 {
			crossings++
		}
	}
	seconds := float64(len(out)-1) / 8000
	if freq := float64(crossings) / seconds; math.Abs(freq-1000) > 30 {
		t.Fatalf("frequency = %.1f Hz, want 1000 Hz", freq)
	}
}

func TestUpsampleAmplitude(t *testing.T) {
	r := New(8000, 48000)
	in := sine(8000, 500, 8000, 8000) // 1 second
	out := r.Process(in)

	if len(out) < 47500 || len(out) > 48500 {
		t.Fatalf("output length = %d, want about 48000", len(out))
	}
	got := rms(out[2000:])
	want := 8000 / math.Sqrt2
	if math.Abs(got-want)/want > 0.05 {
		t.Fatalf("rms = %.1f, want %.1f", got, want)
	}
}

func TestDCGain(t *testing.T) {
	r := New(48000, 8000)
	in := make([]int16, 48000)
	for i := range in {
		in[i] = 1234
	}
	out := r.Process(in)
	for i := 200; i < len(out); i++ {
		if d := int(out[i]) - 1234; d < -2 || d > 2 {
			t.Fatalf("sample %d = %d, want 1234", i, out[i])
		}
	}
}

func TestStreamingMatchesSinglePass(t *testing.T) {
	in := sine(48000, 700, 24000, 9000)

	whole := New(48000, 8000).Process(in)

	streamed := New(48000, 8000)
	var chunked []int16
	for i := 0; i < len(in); i += 480 {
		end := i + 480
		if end > len(in) {
			end = len(in)
		}
		chunked = append(chunked, streamed.Process(in[i:end])...)
	}

	if len(chunked) != len(whole) {
		t.Fatalf("lengths differ: chunked=%d whole=%d", len(chunked), len(whole))
	}
	for i := range whole {
		if chunked[i] != whole[i] {
			t.Fatalf("sample %d differs: %d vs %d", i, chunked[i], whole[i])
		}
	}
}
