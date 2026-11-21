// Package prompts provides the short spoken call prompts baked into the
// bridge binary. The PCM assets are generated with cmd/prompt-tts from
// kokoro.cloudwaddie.com (or any OpenAI-compatible TTS endpoint) and are raw
// s16le mono at the default call rate (8 kHz).
package prompts

import (
	_ "embed"
)

//go:embed assets/connected.pcm
var connectedPCM []byte

//go:embed assets/error.pcm
var errorPCM []byte

//go:embed assets/busy.pcm
var busyPCM []byte

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
	switch name {
	case Connected:
		return connectedPCM, true
	case Error:
		return errorPCM, true
	case Busy:
		return busyPCM, true
	default:
		return nil, false
	}
}

// Names lists the baked prompts.
func Names() []string { return []string{Connected, Error, Busy} }
