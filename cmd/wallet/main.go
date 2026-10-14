// Command wallet executa a API HTTP e os workers em segundo plano.
package main

import (
	"fmt"
	"os"

	"go.uber.org/fx"

	"github.com/gbrpscal/wallet/internal/bootstrap"
	"github.com/gbrpscal/wallet/internal/platform/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fx.New(bootstrap.Options(cfg)).Run()
}
