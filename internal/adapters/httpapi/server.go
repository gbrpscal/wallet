// Package httpapi expõe a API HTTP usando net/http.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"go.uber.org/fx"

	"github.com/gbrpscal/wallet/internal/platform/config"
)

// Module fornece o roteador, os handlers de health e o servidor HTTP.
var Module = fx.Module("httpapi",
	fx.Provide(
		NewHealthHandler,
		NewMux,
	),
	fx.Invoke(StartServer),
)

// NewMux registra todas as rotas.
func NewMux(h *HealthHandler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", h.Live)
	mux.HandleFunc("GET /health/ready", h.Ready)
	return mux
}

// StartServer abre o listener durante o start, para que uma porta ocupada
// faça a inicialização falhar, e drena as requisições em andamento no stop,
// dentro do prazo de parada do Fx.
func StartServer(lc fx.Lifecycle, cfg config.Config, mux *http.ServeMux, health *HealthHandler, log *slog.Logger) {
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           mux,
		ReadTimeout:       cfg.HTTPReadTimeout,
		ReadHeaderTimeout: cfg.HTTPHeaderTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", srv.Addr)
			if err != nil {
				return fmt.Errorf("listen %s: %w", srv.Addr, err)
			}
			log.Info("http server listening", "addr", ln.Addr().String())
			go func() {
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Error("http server stopped unexpectedly", "error", err)
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			health.MarkShuttingDown()
			log.Info("http server shutting down")
			return srv.Shutdown(ctx)
		},
	})
}
