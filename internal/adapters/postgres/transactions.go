package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/money"
	"github.com/gbrpscal/wallet/internal/domain/wagering"
)

type transactionRepo struct {
	q pgx.Tx
}

const transactionColumns = `
	id::text, kind, wallet_id::text, player_id::text, amount_minor, currency, status,
	provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
	reference_external_transaction_id, reference_transaction_id::text, failure_code,
	result_balance_minor, created_at, updated_at`

func (r transactionRepo) Insert(ctx context.Context, t *wagering.WagerTransaction, meta app.TransactionMeta) error {
	s := t.Snapshot()
	var attempts int
	var next, expires *time.Time
	if sc := meta.Schedule; sc != nil {
		attempts, next, expires = sc.Attempts, &sc.NextAttemptAt, &sc.ExpiresAt
	}
	_, err := r.q.Exec(ctx, `
		INSERT INTO wager_transactions (
			id, kind, wallet_id, player_id, amount_minor, currency, status,
			provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
			reference_external_transaction_id, reference_transaction_id, failure_code,
			result_balance_minor, created_at, updated_at,
			correlation_id, causation_id, reference_attempts, next_reference_attempt_at, reference_expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19,
			$20, $21, $22, $23, $24)`,
		s.ID.String(), string(s.Kind), s.WalletID.String(), s.PlayerID.String(),
		s.Money.MinorUnits(), s.Money.Currency().String(), string(s.Status),
		nullString(s.ProviderID), nullString(s.ExternalTransactionID), nullString(s.IdempotencyKey),
		nullString(string(s.PayloadHash)), nullString(s.RoundID), nullString(s.GameID),
		nullString(s.ReferenceExternalTransactionID), nullID(s.ReferenceTransactionID),
		nullString(string(s.FailureCode)), nullMinor(s.ResultBalance), s.CreatedAt, s.UpdatedAt,
		meta.Trace.CorrelationID, nullString(meta.Trace.CausationID), attempts, next, expires)
	return translate(err)
}

func (r transactionRepo) Save(ctx context.Context, t *wagering.WagerTransaction, previous wagering.Status, schedule *app.ReferenceSchedule) error {
	s := t.Snapshot()
	sql := `
		UPDATE wager_transactions
		SET status = $2, failure_code = $3, result_balance_minor = $4,
		    reference_transaction_id = $5, updated_at = $6
		WHERE id = $1 AND status = $7`
	args := []any{s.ID.String(), string(s.Status), nullString(string(s.FailureCode)), nullMinor(s.ResultBalance),
		nullID(s.ReferenceTransactionID), s.UpdatedAt, string(previous)}
	if schedule != nil {
		sql = `
		UPDATE wager_transactions
		SET status = $2, failure_code = $3, result_balance_minor = $4,
		    reference_transaction_id = $5, updated_at = $6,
		    reference_attempts = $8, next_reference_attempt_at = $9, reference_expires_at = $10
		WHERE id = $1 AND status = $7`
		args = append(args, schedule.Attempts, schedule.NextAttemptAt, schedule.ExpiresAt)
	}
	tag, err := r.q.Exec(ctx, sql, args...)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() != 1 {
		return app.ErrConcurrentUpdate
	}
	return nil
}

func (r transactionRepo) LockPending(ctx context.Context, id domain.TransactionID) (*wagering.WagerTransaction, app.PendingInfo, error) {
	var info app.PendingInfo
	var causation *string
	var next, expires *time.Time
	row := r.q.QueryRow(ctx, `
		SELECT `+transactionColumns+`, correlation_id, causation_id,
		       reference_attempts, next_reference_attempt_at, reference_expires_at
		FROM wager_transactions WHERE id = $1 FOR UPDATE`, id.String())
	t, err := scanTransactionWith(row, &info.Trace.CorrelationID, &causation, &info.Schedule.Attempts, &next, &expires)
	if err != nil {
		return nil, app.PendingInfo{}, err
	}
	info.Trace.CausationID = deref(causation)
	if next != nil {
		info.Schedule.NextAttemptAt = domain.Timestamp(*next)
	}
	if expires != nil {
		info.Schedule.ExpiresAt = domain.Timestamp(*expires)
	}
	return t, info, nil
}

func (r transactionRepo) DuePendingReferences(ctx context.Context, now time.Time, limit int) ([]app.PendingReference, error) {
	rows, err := r.q.Query(ctx, `
		SELECT id::text, wallet_id::text FROM wager_transactions
		WHERE status = 'PENDING_REFERENCE' AND next_reference_attempt_at <= $1
		ORDER BY next_reference_attempt_at
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	var due []app.PendingReference
	for rows.Next() {
		var id, walletID string
		if err := rows.Scan(&id, &walletID); err != nil {
			return nil, translate(err)
		}
		var p app.PendingReference
		if p.TransactionID, err = domain.ParseID[domain.TransactionID](id); err != nil {
			return nil, inconsistent("pending transaction id", err)
		}
		if p.WalletID, err = domain.ParseID[domain.WalletID](walletID); err != nil {
			return nil, inconsistent("pending wallet id", err)
		}
		due = append(due, p)
	}
	return due, translate(rows.Err())
}

func (r transactionRepo) Get(ctx context.Context, id domain.TransactionID) (*wagering.WagerTransaction, error) {
	return scanTransaction(r.q.QueryRow(ctx, `SELECT `+transactionColumns+` FROM wager_transactions WHERE id = $1`, id.String()))
}

func (r transactionRepo) FindByIdempotencyKey(ctx context.Context, providerID, key string) (*wagering.WagerTransaction, error) {
	return scanTransaction(r.q.QueryRow(ctx, `
		SELECT `+transactionColumns+` FROM wager_transactions
		WHERE provider_id = $1 AND idempotency_key = $2 AND kind <> 'OPENING'`, providerID, key))
}

func (r transactionRepo) FindByExternalID(ctx context.Context, providerID, externalID string) (*wagering.WagerTransaction, error) {
	return scanTransaction(r.q.QueryRow(ctx, `
		SELECT `+transactionColumns+` FROM wager_transactions
		WHERE provider_id = $1 AND external_transaction_id = $2 AND kind <> 'OPENING'`, providerID, externalID))
}

func (r transactionRepo) HasProcessedReversal(ctx context.Context, id domain.TransactionID) (bool, error) {
	var exists bool
	err := r.q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM wager_transactions
			WHERE reference_transaction_id = $1 AND status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK'))`,
		id.String()).Scan(&exists)
	return exists, translate(err)
}

func scanTransaction(row pgx.Row) (*wagering.WagerTransaction, error) {
	return scanTransactionWith(row)
}

// scanTransactionWith lê as colunas de transactionColumns seguidas de extra.
func scanTransactionWith(row pgx.Row, extra ...any) (*wagering.WagerTransaction, error) {
	var (
		id, kind, walletID, playerID, currency, status string
		amount                                         int64
		providerID, externalID, key, hash              *string
		roundID, gameID, refExternal, refID, failure   *string
		resultBalance                                  *int64
		createdAt, updatedAt                           time.Time
	)
	dest := []any{&id, &kind, &walletID, &playerID, &amount, &currency, &status,
		&providerID, &externalID, &key, &hash, &roundID, &gameID,
		&refExternal, &refID, &failure, &resultBalance, &createdAt, &updatedAt}
	err := row.Scan(append(dest, extra...)...)
	if err != nil {
		return nil, translate(err)
	}

	s := wagering.Snapshot{
		Kind:                           wagering.Kind(kind),
		Status:                         wagering.Status(status),
		ProviderID:                     deref(providerID),
		ExternalTransactionID:          deref(externalID),
		IdempotencyKey:                 deref(key),
		PayloadHash:                    wagering.PayloadHash(deref(hash)),
		RoundID:                        deref(roundID),
		GameID:                         deref(gameID),
		ReferenceExternalTransactionID: deref(refExternal),
		FailureCode:                    domain.Code(deref(failure)),
		CreatedAt:                      domain.Timestamp(createdAt),
		UpdatedAt:                      domain.Timestamp(updatedAt),
	}
	if s.ID, err = domain.ParseID[domain.TransactionID](id); err != nil {
		return nil, inconsistent("transaction id", err)
	}
	if s.WalletID, err = domain.ParseID[domain.WalletID](walletID); err != nil {
		return nil, inconsistent("transaction wallet", err)
	}
	if s.PlayerID, err = domain.ParseID[domain.PlayerID](playerID); err != nil {
		return nil, inconsistent("transaction player", err)
	}
	if refID != nil {
		if s.ReferenceTransactionID, err = domain.ParseID[domain.TransactionID](*refID); err != nil {
			return nil, inconsistent("transaction reference", err)
		}
	}
	var cur money.Currency
	if cur, s.Money, err = moneyOf(amount, currency); err != nil {
		return nil, err
	}
	if resultBalance != nil {
		if s.ResultBalance, err = money.FromMinor(*resultBalance, cur); err != nil {
			return nil, inconsistent("result balance", err)
		}
	}
	return wagering.Rehydrate(s)
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nullID(id domain.TransactionID) *string {
	if id.IsZero() {
		return nil
	}
	s := id.String()
	return &s
}

func nullMinor(m money.Money) *int64 {
	if !m.IsValid() {
		return nil
	}
	v := m.MinorUnits()
	return &v
}
