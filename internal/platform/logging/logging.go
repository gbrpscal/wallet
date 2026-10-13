// Package logging builds the structured JSON logger shared by the process.
package logging

import (
	"log/slog"
	"os"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/gbrpscal/wallet/internal/platform/config"
)

// Module provides the process-wide *slog.Logger.
var Module = fx.Module("logging",
	fx.Provide(New),
)

// New returns a JSON logger writing to stdout at the configured level.
func New(cfg config.Config) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
}

// FxLogger routes Fx lifecycle events through the application logger.
func FxLogger(l *slog.Logger) fxevent.Logger {
	return &fxevent.SlogLogger{Logger: l.With("component", "fx")}
}
