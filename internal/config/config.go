// Package config loads the bridge configuration from environment variables.
// Environment-only configuration keeps the container image free of files and
// works well with Coolify, Docker Compose and systemd.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Mode selects the audio backend.
type Mode string

const (
	// ModeEcho returns inbound PCM to Asterisk unchanged.
	ModeEcho Mode = "echo"
	// ModeVirtual routes audio to and from PulseAudio/PipeWire virtual devices.
	ModeVirtual Mode = "virtual"
)

// Config is the complete runtime configuration.
type Config struct {
	Mode            Mode
	ListenAddr      string
	HealthAddr      string
	LogLevel        slog.Level
	LogFormat       string
	CallRate        int
	FrameDuration   time.Duration
	MaxFrameBytes   int
	ReadBufferBytes int
	InBufferMS      int
	OutQueueFrames  int
	UUIDTimeout     time.Duration
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	MaxCallDuration time.Duration
	ShutdownTimeout time.Duration
	// CollectAudioStats enables aggregate RMS/peak metering. Audio content is
	// never logged or stored.
	CollectAudioStats bool
	// Prompts plays the baked call prompts (connected, error, busy) to the
	// caller.
	Prompts bool
	Virtual           VirtualConfig
}

// VirtualConfig configures the PulseAudio/PipeWire backend.
type VirtualConfig struct {
	PlaybackSink      string
	CaptureSource     string
	DeviceRate        int
	DeviceChannels    int
	Latency           time.Duration
	OutBufferMS       int
	SilenceOnUnderrun bool
	PacatBin          string
	ParecBin          string
	RestartInitial    time.Duration
	RestartMax        time.Duration
	MaxRestarts       int
}

// FrameSamples returns the number of samples in one outbound frame.
func (c Config) FrameSamples() int {
	return int(int64(c.CallRate) * c.FrameDuration.Milliseconds() / 1000)
}

// FrameBytes returns the number of PCM bytes in one outbound frame.
func (c Config) FrameBytes() int { return c.FrameSamples() * 2 }

// InBufferBytes returns the call-to-device smoothing buffer size.
func (c Config) InBufferBytes() int { return c.CallRate * 2 * c.InBufferMS / 1000 }

// OutBufferBytes returns the device-to-call jitter buffer size.
func (c Config) OutBufferBytes() int { return c.CallRate * 2 * c.Virtual.OutBufferMS / 1000 }

// Load reads the configuration from the environment.
func Load() (Config, error) {
	l := &loader{}
	cfg := Config{
		Mode:              Mode(strings.ToLower(l.str("BRIDGE_MODE", string(ModeEcho)))),
		ListenAddr:        l.str("BRIDGE_LISTEN_ADDR", ":9092"),
		HealthAddr:        l.str("BRIDGE_HEALTH_ADDR", ":8080"),
		LogLevel:          parseLevel(l, "BRIDGE_LOG_LEVEL", "info"),
		LogFormat:         strings.ToLower(l.str("BRIDGE_LOG_FORMAT", "json")),
		CallRate:          l.integer("BRIDGE_CALL_RATE", 8000),
		FrameDuration:     l.duration("BRIDGE_FRAME_MS", 20*time.Millisecond),
		MaxFrameBytes:     l.integer("BRIDGE_MAX_FRAME_BYTES", 4096),
		ReadBufferBytes:   l.integer("BRIDGE_READ_BUFFER_BYTES", 8192),
		InBufferMS:        l.integer("BRIDGE_IN_BUFFER_MS", 500),
		OutQueueFrames:    l.integer("BRIDGE_OUT_QUEUE_FRAMES", 50),
		UUIDTimeout:       l.duration("BRIDGE_UUID_TIMEOUT", 3*time.Second),
		ReadTimeout:       l.duration("BRIDGE_READ_TIMEOUT", 0),
		WriteTimeout:      l.duration("BRIDGE_WRITE_TIMEOUT", 10*time.Second),
		MaxCallDuration:   l.duration("BRIDGE_MAX_CALL_DURATION", 0),
		ShutdownTimeout:   l.duration("BRIDGE_SHUTDOWN_TIMEOUT", 10*time.Second),
		CollectAudioStats: l.boolean("BRIDGE_COLLECT_AUDIO_STATS", true),
		Prompts:           l.boolean("BRIDGE_PROMPTS", true),
		Virtual: VirtualConfig{
			PlaybackSink:      l.str("BRIDGE_PA_PLAYBACK_SINK", "audiosocket_mic"),
			CaptureSource:     l.str("BRIDGE_PA_CAPTURE_SOURCE", "audiosocket_spk.monitor"),
			DeviceRate:        l.integer("BRIDGE_PA_DEVICE_RATE", 8000),
			DeviceChannels:    l.integer("BRIDGE_PA_DEVICE_CHANNELS", 1),
			Latency:           l.duration("BRIDGE_PA_LATENCY", 40*time.Millisecond),
			OutBufferMS:       l.integer("BRIDGE_PA_OUT_BUFFER_MS", 400),
			SilenceOnUnderrun: l.boolean("BRIDGE_PA_SILENCE_ON_UNDERRUN", true),
			PacatBin:          l.str("BRIDGE_PA_PACAT_BIN", "pacat"),
			ParecBin:          l.str("BRIDGE_PA_PAREC_BIN", "parec"),
			RestartInitial:    l.duration("BRIDGE_PA_RESTART_INITIAL", 500*time.Millisecond),
			RestartMax:        l.duration("BRIDGE_PA_RESTART_MAX", 30*time.Second),
			MaxRestarts:       l.integer("BRIDGE_PA_MAX_RESTARTS", 10),
		},
	}
	if len(l.problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n  %s", strings.Join(l.problems, "\n  "))
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks the configuration for consistency.
func (c Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	switch c.Mode {
	case ModeEcho, ModeVirtual:
	default:
		add("BRIDGE_MODE must be %q or %q, got %q", ModeEcho, ModeVirtual, c.Mode)
	}
	if strings.TrimSpace(c.ListenAddr) == "" {
		add("BRIDGE_LISTEN_ADDR must not be empty")
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		add("BRIDGE_LOG_FORMAT must be \"json\" or \"text\", got %q", c.LogFormat)
	}
	switch c.CallRate {
	case 8000, 12000, 16000, 24000, 32000, 44100, 48000, 96000, 192000:
	default:
		add("BRIDGE_CALL_RATE must be an AudioSocket slin rate (8000, 12000, 16000, 24000, 32000, 44100, 48000, 96000, 192000), got %d", c.CallRate)
	}
	if c.FrameDuration < 5*time.Millisecond || c.FrameDuration > 100*time.Millisecond {
		add("BRIDGE_FRAME_MS must be between 5ms and 100ms, got %s", c.FrameDuration)
	}
	if c.FrameSamples() < 1 {
		add("BRIDGE_FRAME_MS yields zero samples at %d Hz", c.CallRate)
	}
	if c.MaxFrameBytes < 64 || c.MaxFrameBytes > 65535 {
		add("BRIDGE_MAX_FRAME_BYTES must be between 64 and 65535, got %d", c.MaxFrameBytes)
	}
	if c.ReadBufferBytes < 1024 {
		add("BRIDGE_READ_BUFFER_BYTES must be at least 1024, got %d", c.ReadBufferBytes)
	}
	if c.InBufferMS < 20 {
		add("BRIDGE_IN_BUFFER_MS must be at least 20, got %d", c.InBufferMS)
	}
	if c.OutQueueFrames < 2 || c.OutQueueFrames > 10000 {
		add("BRIDGE_OUT_QUEUE_FRAMES must be between 2 and 10000, got %d", c.OutQueueFrames)
	}
	if c.UUIDTimeout < 0 || c.ReadTimeout < 0 || c.WriteTimeout <= 0 {
		add("timeouts must be non-negative and BRIDGE_WRITE_TIMEOUT must be positive")
	}
	if c.MaxCallDuration < 0 || c.ShutdownTimeout <= 0 {
		add("BRIDGE_MAX_CALL_DURATION must be non-negative and BRIDGE_SHUTDOWN_TIMEOUT positive")
	}
	if c.Mode == ModeVirtual {
		if c.Virtual.PlaybackSink == "" {
			add("BRIDGE_PA_PLAYBACK_SINK must not be empty")
		}
		if c.Virtual.CaptureSource == "" {
			add("BRIDGE_PA_CAPTURE_SOURCE must not be empty")
		}
		if c.Virtual.DeviceRate < 8000 || c.Virtual.DeviceRate > 192000 {
			add("BRIDGE_PA_DEVICE_RATE must be between 8000 and 192000, got %d", c.Virtual.DeviceRate)
		}
		if c.Virtual.DeviceChannels < 1 || c.Virtual.DeviceChannels > 8 {
			add("BRIDGE_PA_DEVICE_CHANNELS must be between 1 and 8, got %d", c.Virtual.DeviceChannels)
		}
		if c.Virtual.Latency < 5*time.Millisecond {
			add("BRIDGE_PA_LATENCY must be at least 5ms, got %s", c.Virtual.Latency)
		}
		if c.Virtual.OutBufferMS < 20 {
			add("BRIDGE_PA_OUT_BUFFER_MS must be at least 20, got %d", c.Virtual.OutBufferMS)
		}
		if c.Virtual.PacatBin == "" || c.Virtual.ParecBin == "" {
			add("BRIDGE_PA_PACAT_BIN and BRIDGE_PA_PAREC_BIN must not be empty")
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

type loader struct {
	problems []string
}

func (l *loader) str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func (l *loader) integer(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		l.problems = append(l.problems, fmt.Sprintf("%s must be an integer, got %q", key, v))
		return def
	}
	return n
}

func (l *loader) boolean(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		l.problems = append(l.problems, fmt.Sprintf("%s must be a boolean, got %q", key, v))
		return def
	}
	return b
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	raw := strings.TrimSpace(v)
	d, err := time.ParseDuration(raw)
	if err != nil {
		// Accept a bare number as milliseconds.
		if ms, convErr := strconv.Atoi(raw); convErr == nil {
			return time.Duration(ms) * time.Millisecond
		}
		l.problems = append(l.problems, fmt.Sprintf("%s must be a duration such as 20ms or 2s, got %q", key, v))
		return def
	}
	return d
}

func parseLevel(l *loader, key, def string) slog.Level {
	raw := strings.ToLower(l.str(key, def))
	switch raw {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		l.problems = append(l.problems, fmt.Sprintf("%s must be debug, info, warn or error, got %q", key, raw))
		return slog.LevelInfo
	}
}
