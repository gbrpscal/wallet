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
		fx.Annotate(NewOIDCAuthenticator, fx.As(new(Authenticator))),
		NewAPI,
		NewHandler,
	),
	fx.Invoke(StartServer),
)

// NewHandler registra todas as rotas: health checks públicos e as rotas
// autenticadas da API, envolvidas pelos middlewares de correlação, access log
// e recuperação de panic.
func NewHandler(h *HealthHandler, api *API, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", routed(h.Live))
	mux.HandleFunc("GET /health/ready", routed(h.Ready))
	api.routes(mux)
	return withCorrelation(accessLog(log, recoverer(log, mux)))
}

// StartServer abre o listener durante o start, para que uma porta ocupada
// faça a inicialização falhar, e drena as requisições em andamento no stop,
// dentro do prazo de parada do Fx.
func StartServer(lc fx.Lifecycle, cfg config.Config, handler http.Handler, health *HealthHandler, log *slog.Logger) {
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
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
