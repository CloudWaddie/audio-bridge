// Command audiosocket-bridge bridges Asterisk AudioSocket calls to Linux
// virtual audio devices.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/cloudwaddie/audiosocket-bridge/internal/audiosocket"
	"github.com/cloudwaddie/audiosocket-bridge/internal/bridge"
	"github.com/cloudwaddie/audiosocket-bridge/internal/config"
	"github.com/cloudwaddie/audiosocket-bridge/internal/health"
	"github.com/cloudwaddie/audiosocket-bridge/internal/logging"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "audiosocket-bridge: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := logging.New(cfg.LogLevel, cfg.LogFormat, os.Stdout)

	if err := bridge.Preflight(cfg); err != nil {
		return err
	}

	logger.Info("starting audiosocket-bridge",
		"version", version,
		"mode", string(cfg.Mode),
		"listen", cfg.ListenAddr,
		"health", cfg.HealthAddr,
		"call_rate", cfg.CallRate,
		"frame_ms", cfg.FrameDuration.Milliseconds(),
		"max_calls", 1,
	)

	server := audiosocket.NewServer(audiosocket.ServerConfig{
		Mode:       string(cfg.Mode),
		ListenAddr: cfg.ListenAddr,
		Session: audiosocket.SessionConfig{
			UUIDTimeout:       cfg.UUIDTimeout,
			ReadTimeout:       cfg.ReadTimeout,
			WriteTimeout:      cfg.WriteTimeout,
			MaxFrameBytes:     cfg.MaxFrameBytes,
			ReadBufferBytes:   cfg.ReadBufferBytes,
			OutQueueFrames:    cfg.OutQueueFrames,
			MaxCallDuration:   cfg.MaxCallDuration,
			FrameDuration:     cfg.FrameDuration,
			CallRate:          cfg.CallRate,
			CollectAudioStats: cfg.CollectAudioStats,
			Prompts:           cfg.Prompts,
		},
	}, logger, func() (bridge.Endpoint, error) {
		return bridge.New(cfg, logger)
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var healthSrv *health.Server
	if cfg.HealthAddr != "" {
		healthSrv = health.New(cfg.HealthAddr, logger, server.Snapshot, server.HangupCall)
		if err := healthSrv.Start(); err != nil {
			return fmt.Errorf("start health server: %w", err)
		}
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(ctx) }()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-serveErr:
		if err != nil {
			logger.Error("listener failed", "err", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if healthSrv != nil {
		if err := healthSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("health server shutdown", "err", err)
		}
	}
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("shutdown incomplete", "err", err)
	}
	logger.Info("stopped")
	return nil
}
