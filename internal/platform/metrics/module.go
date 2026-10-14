package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/fx"

	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/platform/config"
)

// Module fornece app.Metrics e, se habilitado, serve /metrics em METRICS_ADDR
// (porta separada, não publicada no host). Desabilitado, as métricas viram
// app.NopMetrics.
var Module = fx.Module("metrics",
	fx.Provide(NewFromConfig),
	fx.Invoke(StartServer),
)

// Registry agrupa o registry e as métricas para o servidor.
type Registry struct {
	reg *prometheus.Registry
}

// NewFromConfig cria o registry com as métricas de negócio e os coletores de
// runtime do Go e do processo.
func NewFromConfig(cfg config.Config) (app.Metrics, *Registry) {
	if !cfg.MetricsEnabled {
		return app.NopMetrics{}, nil
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return New(reg), &Registry{reg: reg}
}

// StartServer serve /metrics na porta própria; para depois de drenar as
// requisições em andamento.
func StartServer(lc fx.Lifecycle, cfg config.Config, r *Registry, log *slog.Logger) {
	if r == nil {
		log.Info("metrics disabled")
		return
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{Registry: r.reg}))
	srv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", srv.Addr)
			if err != nil {
				return fmt.Errorf("listen %s: %w", srv.Addr, err)
			}
			log.Info("metrics server listening", "addr", ln.Addr().String())
			go func() {
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Error("metrics server stopped unexpectedly", "error", err)
				}
			}()
			return nil
		},
		OnStop: srv.Shutdown,
	})
}
