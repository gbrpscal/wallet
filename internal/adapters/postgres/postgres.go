// Package postgres fornece o pool de conexões PostgreSQL e os repositórios.
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/platform/config"
	"github.com/gbrpscal/wallet/internal/platform/health"
)

// Module fornece o pool de conexões, a UnitOfWork e a checagem de readiness.
var Module = fx.Module("postgres",
	fx.Provide(
		NewPool,
		fx.Annotate(newUnitOfWork, fx.As(new(app.UnitOfWork))),
		health.AsChecker(NewReadinessCheck),
	),
)

func newUnitOfWork(pool *pgxpool.Pool, cfg config.Config) *UnitOfWork {
	return NewUnitOfWork(pool, cfg.DBLockTimeout)
}

// NewPool cria o pool. As conexões são abertas sob demanda, então um banco
// temporariamente indisponível na partida não impede o processo de subir; a
// readiness é que reporta a indisponibilidade. O pool é fechado no stop, depois
// que todos os componentes que dependem dele pararam.
func NewPool(lc fx.Lifecycle, cfg config.Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	poolCfg.MaxConns = int32(cfg.DBMaxConns)
	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	lc.Append(fx.StopHook(pool.Close))
	return pool, nil
}

// NewReadinessCheck faz ping no banco.
func NewReadinessCheck(pool *pgxpool.Pool) health.CheckerFunc {
	return health.CheckerFunc{CheckName: "postgres", Fn: pool.Ping}
}
