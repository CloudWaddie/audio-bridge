package prompts

import (
	"testing"

	"github.com/cloudwaddie/audiosocket-bridge/internal/audio"
)

func TestBakedPromptsAreUsable(t *testing.T) {
	for _, name := range Names() {
		pcm, ok := Get(name)
		if !ok {
			t.Fatalf("%s: prompt missing", name)
		}
		if len(pcm) == 0 || len(pcm)%2 != 0 {
			t.Fatalf("%s: want non-empty even-length PCM, got %d bytes", name, len(pcm))
		}
		rms, peak := audio.Level(pcm)
		if peak < -20 {
			t.Fatalf("%s: prompt is too quiet: peak %.1f dBFS", name, peak)
		}
		if rms > -12 {
			t.Fatalf("%s: prompt is too hot: rms %.1f dBFS", name, rms)
		}
	}
}

func TestUnknownPrompt(t *testing.T) {
	if _, ok := Get("nope"); ok {
		t.Fatal("unknown prompt should not resolve")
	}
}

func TestWidebandPromptsPreserveDuration(t *testing.T) {
	for _, name := range Names() {
		source, ok := AtRate(name, 24000)
		if !ok || len(source) == 0 || len(source)%2 != 0 {
			t.Fatalf("%s: missing or invalid 24 kHz PCM", name)
		}
		for _, rate := range []int{12000, 16000, 24000, 32000, 44100, 48000, 96000, 192000} {
			pcm, ok := AtRate(name, rate)
			want := len(source) / 2 * rate / 24000
			// The streaming resampler retains a few milliseconds of the baked trailing silence.
			if got := len(pcm) / 2; !ok || len(pcm)%2 != 0 || got < want-rate*3/1000 || got > want+1 {
				t.Errorf("%s at %d Hz: got %d samples, want about %d", name, rate, got, want)
			}
		}
	}
}
