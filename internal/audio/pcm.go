// Package audio contains the PCM helpers shared by the AudioSocket protocol
// layer and the audio backends. All audio is 16-bit signed little-endian
// linear PCM, which is what Asterisk's "slin" formats use.
package audio

import (
	"encoding/binary"
	"math"
)

// BytesToSamples decodes little-endian s16 PCM into dst, reusing its capacity.
// A trailing odd byte is ignored.
func BytesToSamples(dst []int16, b []byte) []int16 {
	n := len(b) / 2
	if cap(dst) < n {
		dst = make([]int16, n)
	} else {
		dst = dst[:n]
	}
	for i := 0; i < n; i++ {
		dst[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return dst
}

// SamplesToBytes encodes s16 PCM into dst, reusing its capacity.
func SamplesToBytes(dst []byte, s []int16) []byte {
	n := len(s) * 2
	if cap(dst) < n {
		dst = make([]byte, n)
	} else {
		dst = dst[:n]
	}
	for i, v := range s {
		binary.LittleEndian.PutUint16(dst[i*2:], uint16(v))
	}
	return dst
}

// MonoToChannels duplicates mono s16 samples into an interleaved buffer with
// ch channels.
func MonoToChannels(dst []byte, s []int16, ch int) []byte {
	if ch <= 1 {
		return SamplesToBytes(dst, s)
	}
	n := len(s) * ch * 2
	if cap(dst) < n {
		dst = make([]byte, n)
	} else {
		dst = dst[:n]
	}
	for i, v := range s {
		for c := 0; c < ch; c++ {
			binary.LittleEndian.PutUint16(dst[(i*ch+c)*2:], uint16(v))
		}
	}
	return dst
}

// ChannelsToMono averages interleaved s16 channels down to mono.
func ChannelsToMono(dst []int16, b []byte, ch int) []int16 {
	if ch <= 1 {
		return BytesToSamples(dst, b)
	}
	frames := len(b) / 2 / ch
	if cap(dst) < frames {
		dst = make([]int16, frames)
	} else {
		dst = dst[:frames]
	}
	for i := 0; i < frames; i++ {
		var sum int32
		for c := 0; c < ch; c++ {
			sum += int32(int16(binary.LittleEndian.Uint16(b[(i*ch+c)*2:])))
		}
		dst[i] = int16(sum / int32(ch))
	}
	return dst
}

// Level returns the RMS and peak amplitude of a PCM frame in dBFS. It never
// retains or exposes the audio itself, only an aggregate level.
func Level(pcm []byte) (rmsDB, peakDB float64) {
	n := len(pcm) / 2
	if n == 0 {
		return math.Inf(-1), math.Inf(-1)
	}
	var sum float64
	var peak float64
	for i := 0; i < n; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
		sum += v * v
		if a := math.Abs(v); a > peak {
			peak = a
		}
	}
	return toDBFS(math.Sqrt(sum / float64(n))), toDBFS(peak)
}

func toDBFS(v float64) float64 {
	if v <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(v/32768.0)
}

// Silence returns a zeroed PCM buffer of n bytes.
func Silence(n int) []byte { return make([]byte, n) }
