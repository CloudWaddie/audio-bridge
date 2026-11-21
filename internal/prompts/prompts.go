// Package prompts provides the short spoken call prompts baked into the
// bridge binary. The PCM assets are generated with cmd/prompt-tts from
// kokoro.cloudwaddie.com (or any OpenAI-compatible TTS endpoint) and are raw
// s16le mono in narrowband (8 kHz) and wideband (24 kHz) versions.
package prompts

import (
	_ "embed"

	"github.com/cloudwaddie/audiosocket-bridge/internal/audio"
	"github.com/cloudwaddie/audiosocket-bridge/internal/audio/resample"
)

//go:embed assets/connected.pcm
var connectedPCM []byte

//go:embed assets/error.pcm
var errorPCM []byte

//go:embed assets/busy.pcm
var busyPCM []byte

//go:embed assets/connected-24k.pcm
var connectedWideband []byte

//go:embed assets/error-24k.pcm
var errorWideband []byte

//go:embed assets/busy-24k.pcm
var busyWideband []byte

// Prompt names accepted by Get.
const (
	// Connected plays when a call is bridged, before the audio endpoint
	// starts.
	Connected = "connected"
	// Error plays when the audio backend fails to start.
	Error = "error"
	// Busy plays to a second caller before the busy error frame.
	Busy = "busy"
)

// Get returns the raw PCM for a prompt. The second return is false when the
// name is unknown. The audio is read-only; callers must not modify it.
func Get(name string) ([]byte, bool) {
	return AtRate(name, 8000)
}

// AtRate returns read-only PCM at the call rate. Wideband calls use the
// 24 kHz recordings so they retain speech frequencies above narrowband.
func AtRate(name string, rate int) ([]byte, bool) {
	if rate <= 0 {
		return nil, false
	}
	var narrowband, wideband []byte
	switch name {
	case Connected:
		narrowband, wideband = connectedPCM, connectedWideband
	case Error:
		narrowband, wideband = errorPCM, errorWideband
	case Busy:
		narrowband, wideband = busyPCM, busyWideband
	default:
		return nil, false
	}
	switch rate {
	case 8000:
		return narrowband, true
	case 24000:
		return wideband, true
	}
	samples := audio.BytesToSamples(nil, wideband)
	converted := resample.New(24000, rate).Process(samples)
	return audio.SamplesToBytes(nil, converted), true
}

// Names lists the baked prompts.
func Names() []string { return []string{Connected, Error, Busy} }
