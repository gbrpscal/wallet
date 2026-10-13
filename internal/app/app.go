// Package app contém os casos de uso e as portas de que eles dependem.
package app

import (
	"go.uber.org/fx"

	"github.com/gbrpscal/wallet/internal/platform/config"
)

// Module fornece os casos de uso.
var Module = fx.Module("app",
	fx.Provide(
		fx.Annotate(func() SystemClock { return SystemClock{} }, fx.As(new(Clock))),
		func() RetryPolicy { return DefaultRetryPolicy },
		NewReferencePolicy,
		NewService,
	),
)

// NewReferencePolicy monta a política de espera por referência a partir da configuração.
func NewReferencePolicy(cfg config.Config) ReferencePolicy {
	return ReferencePolicy{
		MaxAttempts: cfg.ReferenceMaxAttempts,
		TTL:         cfg.ReferenceTTL,
		BaseDelay:   cfg.ReferenceBaseDelay,
		MaxDelay:    cfg.ReferenceMaxDelay,
	}
}
