// Command prompt-tts bakes call prompts for the audiosocket-bridge.
//
// It fetches one spoken line from an OpenAI-compatible TTS endpoint (for
// example kokoro.cloudwaddie.com), optionally prefixes a synthesized chime,
// trims silence, resamples to the call rate with the same band-limited
// resampler the bridge uses at runtime, normalizes the level and writes raw
// s16le mono PCM:
//
//	prompt-tts -text "Connecting to ChatGPT." -chime connected \
//	    -out internal/prompts/assets/connected.pcm
//
// The output is checked into the repository and embedded in the bridge
// binary, so prompt playback has no runtime dependency on the TTS service.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cloudwaddie/audiosocket-bridge/internal/audio"
	"github.com/cloudwaddie/audiosocket-bridge/internal/audio/resample"
)

func main() {
	url := flag.String("url", "https://kokoro.cloudwaddie.com/v1/audio/speech", "OpenAI-compatible TTS endpoint")
	text := flag.String("text", "", "text to speak (required)")
	voice := flag.String("voice", "af_nicole", "TTS voice")
	speed := flag.Float64("speed", 1.0, "speech speed (0.25-4.0)")
	out := flag.String("out", "", "output PCM file (required)")
	rate := flag.Int("rate", 8000, "output sample rate in Hz")
	chime := flag.String("chime", "none", "chime prefix: none, connected, error, busy")
	peakDB := flag.Float64("peak", -6, "normalize peak to this dBFS")
	timeout := flag.Duration("timeout", 60*time.Second, "HTTP timeout")
	flag.Parse()

	if strings.TrimSpace(*text) == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "prompt-tts: -text and -out are required")
		os.Exit(1)
	}

	speech, srcRate, err := fetchSpeech(*url, *text, *voice, *speed, *timeout)
	if err != nil {
		fatal(err)
	}
	speech = trimEdges(speech, srcRate, 300)

	var samples []int16
	switch *chime {
	case "none":
	case "connected":
		samples = append(samples, chimeTone(srcRate, 660, 0.18)...)
		samples = append(samples, chimeTone(srcRate, 880, 0.22)...)
	case "error":
		samples = append(samples, chimeTone(srcRate, 440, 0.20)...)
		samples = append(samples, chimeTone(srcRate, 330, 0.26)...)
	case "busy":
		samples = append(samples, chimeTone(srcRate, 600, 0.25)...)
		samples = append(samples, make([]int16, srcRate/4)...)
		samples = append(samples, chimeTone(srcRate, 600, 0.25)...)
	default:
		fatal(fmt.Errorf("unknown chime %q", *chime))
	}
	if len(samples) > 0 {
		// Small gap between the chime and the voice.
		samples = append(samples, make([]int16, srcRate/8)...)
	}
	samples = append(samples, speech...)
	// Head and tail room so the prompt does not clip the call stream.
	samples = append(make([]int16, srcRate/10), samples...)
	samples = append(samples, make([]int16, srcRate/5)...)

	converted := resample.New(srcRate, *rate).Process(samples)
	converted = normalize(converted, *peakDB)

	if err := os.WriteFile(*out, audio.SamplesToBytes(nil, converted), 0o644); err != nil {
		fatal(err)
	}
	rms, peak := audio.Level(audio.SamplesToBytes(nil, converted))
	fmt.Printf("wrote %s: %d samples @ %d Hz (%.2fs), peak %.1f dBFS, rms %.1f dBFS\n",
		*out, len(converted), *rate, float64(len(converted))/float64(*rate), peak, rms)
}

// fetchSpeech posts to the TTS endpoint and returns decoded mono s16 samples.
func fetchSpeech(url, text, voice string, speed float64, timeout time.Duration) ([]int16, int, error) {
	body, err := json.Marshal(map[string]any{
		"input":          text,
		"voice":          voice,
		"response_format": "wav",
		"stream":          false,
		"speed":           speed,
	})
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("tts endpoint returned %s", resp.Status)
	}
	wav, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return parseWAV(wav)
}

// parseWAV decodes a RIFF/WAVE file with PCM16 mono data.
func parseWAV(wav []byte) ([]int16, int, error) {
	if len(wav) < 44 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("not a RIFF/WAVE file")
	}
	var (
		format  int
		chans   int
		rate    int
		bits    int
		data    []byte
		sawFmt  bool
		sawData bool
	)
	pos := 12
	for pos+8 <= len(wav) {
		id := string(wav[pos : pos+4])
		size := int(int64(binary.LittleEndian.Uint32(wav[pos+4 : pos+8])))
		// Some encoders write bogus chunk sizes; never read past the file.
		avail := len(wav) - pos - 8
		if size < 0 || size > avail {
			size = avail
		}
		body := wav[pos+8 : pos+8+size]
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, 0, fmt.Errorf("fmt chunk too small")
			}
			format = int(binary.LittleEndian.Uint16(body[0:2]))
			chans = int(binary.LittleEndian.Uint16(body[2:4]))
			rate = int(binary.LittleEndian.Uint32(body[4:8]))
			bits = int(binary.LittleEndian.Uint16(body[14:16]))
			sawFmt = true
		case "data":
			data = body
			sawData = true
		}
		if size <= 0 {
			break
		}
		pos += 8 + size + size%2
	}
	if !sawFmt || !sawData {
		return nil, 0, fmt.Errorf("missing fmt or data chunk")
	}
	if format != 1 && format != 0xFFFE {
		return nil, 0, fmt.Errorf("unsupported WAV format %d", format)
	}
	if chans != 1 || bits != 16 {
		return nil, 0, fmt.Errorf("want mono 16-bit, got %d ch / %d bit", chans, bits)
	}
	return audio.BytesToSamples(nil, data), rate, nil
}

// chimeTone synthesizes a short sine with a 10 ms fade at both ends.
func chimeTone(rate int, hz float64, seconds float64) []int16 {
	n := int(float64(rate) * seconds)
	fade := rate / 100
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		v := math.Sin(2*math.Pi*hz*float64(i)/float64(rate)) * 0.4 * 32768
		switch {
		case i < fade:
			v *= float64(i) / float64(fade)
		case i >= n-fade:
			v *= float64(n-i) / float64(fade)
		}
		out[i] = int16(v)
	}
	return out
}

// trimEdges removes leading and trailing near-silence from the speech.
func trimEdges(samples []int16, rate int, threshold int16) []int16 {
	start := 0
	for start < len(samples) && abs16(samples[start]) < threshold {
		start++
	}
	end := len(samples)
	for end > start && abs16(samples[end-1]) < threshold {
		end--
	}
	if start >= end {
		return nil
	}
	return samples[start:end]
}

func abs16(v int16) int16 {
	if v < 0 {
		return -v
	}
	return v
}

// normalize scales the samples so the peak hits the target dBFS.
func normalize(samples []int16, peakDB float64) []int16 {
	var peak float64
	for _, s := range samples {
		if a := math.Abs(float64(s)); a > peak {
			peak = a
		}
	}
	if peak == 0 {
		return samples
	}
	target := math.Pow(10, peakDB/20) * 32768
	gain := target / peak
	out := make([]int16, len(samples))
	for i, s := range samples {
		v := float64(s) * gain
		if v > math.MaxInt16 {
			v = math.MaxInt16
		}
		if v < math.MinInt16 {
			v = math.MinInt16
		}
		out[i] = int16(math.Round(v))
	}
	return out
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "prompt-tts: %v\n", err)
	os.Exit(1)
}
