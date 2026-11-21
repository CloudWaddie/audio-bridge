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
