//go:build !linux

package bridge

import (
	"log/slog"

	"github.com/cloudwaddie/audiosocket-bridge/internal/config"
)

func newVirtual(_ config.Config, _ *slog.Logger) (Endpoint, error) {
	return nil, errVirtualUnavailable
}

func preflightVirtual(_ config.Config) error { return errVirtualUnavailable }
