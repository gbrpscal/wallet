package postgres

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
)

// SQLSTATEs de falhas temporárias: nada foi confirmado e a operação pode ser
// repetida. Classes inteiras 08 (conexão) e 53 (recursos) também contam.
var transientCodes = map[string]bool{
	"40001": true, // serialization_failure
	"40P01": true, // deadlock_detected
	"55P03": true, // lock_not_available (lock_timeout)
	"57014": true, // query_canceled (statement_timeout)
	"57P01": true, // admin_shutdown
	"57P02": true, // crash_shutdown
	"57P03": true, // cannot_connect_now
}

// conflictKinds classifica os SQLSTATEs que são conflitos de concorrência.
var conflictKinds = map[string]string{
	"40001": app.ConflictSerialization,
	"40P01": app.ConflictDeadlock,
	"55P03": app.ConflictLockTimeout,
}

// constraintErrors traduz constraints de unicidade com significado de negócio.
var constraintErrors = map[string]*domain.Error{
	"wallets_player_currency_key": domain.ErrWalletAlreadyExists,
}

// translate classifica um erro do pgx para os erros da camada de aplicação.
func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return app.ErrNotFound
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &app.TransientError{Err: err}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case transientCodes[pgErr.Code] || strings.HasPrefix(pgErr.Code, "08") || strings.HasPrefix(pgErr.Code, "53"):
			return &app.TransientError{Err: err, Conflict: conflictKinds[pgErr.Code]}
		case pgErr.Code == "23505":
			if de, ok := constraintErrors[pgErr.ConstraintName]; ok {
				return domain.Errorf(de, "%s", pgErr.Detail)
			}
			return &app.DuplicateError{Constraint: pgErr.ConstraintName}
		case strings.HasPrefix(pgErr.Code, "23"):
			// CHECK, FK e triggers de integridade: o código tentou gravar algo
			// que viola uma invariante.
			return domain.Errorf(domain.ErrInconsistentState, "%s (%s)", pgErr.Message, pgErr.Code)
		}
		return err
	}
	var connectErr *pgconn.ConnectError
	var netErr net.Error
	if errors.As(err, &connectErr) || errors.As(err, &netErr) || pgconn.SafeToRetry(err) {
		return &app.TransientError{Err: err}
	}
	return err
}
