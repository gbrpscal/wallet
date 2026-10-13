// Command wallet executa a API HTTP e os workers em segundo plano.
package main

import (
	"fmt"
	"os"

	"go.uber.org/fx"

	"github.com/gbrpscal/wallet/internal/adapters/httpapi"
	"github.com/gbrpscal/wallet/internal/adapters/postgres"
	"github.com/gbrpscal/wallet/internal/adapters/sqs"
	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/platform/config"
	"github.com/gbrpscal/wallet/internal/platform/logging"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fx.New(
		fx.Supply(cfg),
		fx.StopTimeout(cfg.ShutdownTimeout),
		fx.WithLogger(logging.FxLogger),
		logging.Module,
		postgres.Module,
		sqs.Module,
		app.Module,
		httpapi.Module,
	).Run()
}
