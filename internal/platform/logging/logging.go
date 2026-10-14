// Package logging constrói o logger JSON estruturado compartilhado pelo processo.
package logging

import (
	"log/slog"
	"os"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/gbrpscal/wallet/internal/platform/config"
	"github.com/gbrpscal/wallet/internal/platform/logctx"
)

// Module fornece o *slog.Logger do processo.
var Module = fx.Module("logging",
	fx.Provide(New),
)

// New devolve um logger JSON que escreve no stdout no nível configurado e
// inclui em cada linha os identificadores guardados no contexto (logctx).
func New(cfg config.Config) *slog.Logger {
	return slog.New(logctx.Handler{Handler: slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})})
}

// FxLogger encaminha os eventos de ciclo de vida do Fx pelo logger da aplicação.
func FxLogger(l *slog.Logger) fxevent.Logger {
	return &fxevent.SlogLogger{Logger: l.With("component", "fx")}
}
