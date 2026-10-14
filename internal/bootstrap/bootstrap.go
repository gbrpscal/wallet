// Package bootstrap é a raiz de composição: reúne a configuração e os módulos
// Fx da aplicação, para o main e para os testes da composição.
package bootstrap

import (
	"go.uber.org/fx"

	"github.com/gbrpscal/wallet/internal/adapters/httpapi"
	"github.com/gbrpscal/wallet/internal/adapters/postgres"
	"github.com/gbrpscal/wallet/internal/adapters/sqs"
	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/platform/config"
	"github.com/gbrpscal/wallet/internal/platform/logging"
	"github.com/gbrpscal/wallet/internal/platform/metrics"
	"github.com/gbrpscal/wallet/internal/worker"
)

// Options monta a aplicação completa a partir de uma configuração já
// validada. A ordem dos módulos define a ordem de início; o Fx para na ordem
// inversa, então as dependências (pool, clientes) fecham depois de quem as usa.
func Options(cfg config.Config) fx.Option {
	return fx.Options(
		fx.Supply(cfg),
		fx.StopTimeout(cfg.ShutdownTimeout),
		fx.WithLogger(logging.FxLogger),
		logging.Module,
		metrics.Module,
		postgres.Module,
		sqs.Module,
		app.Module,
		httpapi.Module,
		worker.Module,
	)
}
