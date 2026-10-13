// Package postgres provides the PostgreSQL connection pool and repositories.
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/gbrpscal/wallet/internal/platform/config"
	"github.com/gbrpscal/wallet/internal/platform/health"
)

// Module provides the connection pool and its readiness check.
var Module = fx.Module("postgres",
	fx.Provide(
		NewPool,
		health.AsChecker(NewReadinessCheck),
	),
)

// NewPool creates the pool. Connections are opened lazily, so a database that
// is temporarily unavailable at startup does not prevent the process from
// starting; readiness reports it instead. The pool is closed on stop, after
// every component that depends on it has stopped.
func NewPool(lc fx.Lifecycle, cfg config.Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	lc.Append(fx.StopHook(pool.Close))
	return pool, nil
}

// NewReadinessCheck pings the database.
func NewReadinessCheck(pool *pgxpool.Pool) health.CheckerFunc {
	return health.CheckerFunc{CheckName: "postgres", Fn: pool.Ping}
}
