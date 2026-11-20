package config

import (
	"os"
	"strings"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "BRIDGE_") {
			t.Setenv(key, "")
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mode != ModeEcho {
		t.Fatalf("mode = %q", cfg.Mode)
	}
	if cfg.ListenAddr != ":9092" {
		t.Fatalf("listen = %q", cfg.ListenAddr)
	}
	if cfg.CallRate != 8000 {
		t.Fatalf("rate = %d", cfg.CallRate)
	}
	if cfg.FrameBytes() != 320 {
		t.Fatalf("frame bytes = %d, want 320", cfg.FrameBytes())
	}
	if cfg.Virtual.DeviceRate != 8000 {
		t.Fatalf("device rate = %d", cfg.Virtual.DeviceRate)
	}
	if cfg.InBufferBytes() != 8000 {
		t.Fatalf("in buffer = %d", cfg.InBufferBytes())
	}
}

func TestLoadOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("BRIDGE_MODE", "virtual")
	t.Setenv("BRIDGE_LISTEN_ADDR", "127.0.0.1:9999")
	t.Setenv("BRIDGE_CALL_RATE", "16000")
	t.Setenv("BRIDGE_FRAME_MS", "10ms")
	t.Setenv("BRIDGE_LOG_LEVEL", "debug")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mode != ModeVirtual || cfg.ListenAddr != "127.0.0.1:9999" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.FrameBytes() != 320 {
		t.Fatalf("frame bytes = %d, want 320 at 16 kHz/10ms", cfg.FrameBytes())
	}
}

func TestLoadRejectsInvalidMode(t *testing.T) {
	clearEnv(t)
	t.Setenv("BRIDGE_MODE", "telepathy")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid mode")
	}
}

func TestLoadRejectsBadDuration(t *testing.T) {
	clearEnv(t)
	t.Setenv("BRIDGE_UUID_TIMEOUT", "soon")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestValidateRejectsTinyBuffer(t *testing.T) {
	clearEnv(t)
	t.Setenv("BRIDGE_OUT_QUEUE_FRAMES", "1")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for tiny queue")
	}
}
