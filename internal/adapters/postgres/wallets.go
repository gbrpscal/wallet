package postgres

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/money"
	"github.com/gbrpscal/wallet/internal/domain/wallet"
)

type walletRepo struct {
	q pgx.Tx
}

const walletColumns = `id::text, player_id::text, currency, balance_minor, version, created_at, updated_at`

func (r walletRepo) Insert(ctx context.Context, w *wallet.Wallet) error {
	_, err := r.q.Exec(ctx, `
		INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		w.ID().String(), w.PlayerID().String(), w.Currency().String(),
		w.Balance().MinorUnits(), w.Version(), w.CreatedAt(), w.UpdatedAt())
	return translate(err)
}

func (r walletRepo) Lock(ctx context.Context, id domain.WalletID) (*wallet.Wallet, error) {
	return scanWallet(r.q.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1 FOR UPDATE`, id.String()))
}

func (r walletRepo) Get(ctx context.Context, id domain.WalletID) (*wallet.Wallet, error) {
	return scanWallet(r.q.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1`, id.String()))
}

func (r walletRepo) Save(ctx context.Context, w *wallet.Wallet, expectedVersion int64) error {
	tag, err := r.q.Exec(ctx, `
		UPDATE wallets
		SET balance_minor = $2, version = $3, updated_at = $4
		WHERE id = $1 AND version = $5`,
		w.ID().String(), w.Balance().MinorUnits(), w.Version(), w.UpdatedAt(), expectedVersion)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() != 1 {
		return app.ErrConcurrentUpdate
	}
	return nil
}

func scanWallet(row pgx.Row) (*wallet.Wallet, error) {
	var (
		id, player, currency string
		balance, version     int64
		createdAt, updatedAt time.Time
	)
	if err := row.Scan(&id, &player, &currency, &balance, &version, &createdAt, &updatedAt); err != nil {
		return nil, translate(err)
	}
	walletID, err := domain.ParseID[domain.WalletID](id)
	if err != nil {
		return nil, inconsistent("wallet id", err)
	}
	playerID, err := domain.ParseID[domain.PlayerID](player)
	if err != nil {
		return nil, inconsistent("wallet player", err)
	}
	cur, bal, err := moneyOf(balance, currency)
	if err != nil {
		return nil, err
	}
	return wallet.Rehydrate(wallet.Snapshot{
		ID: walletID, PlayerID: playerID, Currency: cur, Balance: bal,
		Version: version, CreatedAt: domain.Timestamp(createdAt), UpdatedAt: domain.Timestamp(updatedAt),
	})
}

// moneyOf reconstrói um valor persistido (unidades mínimas + moeda).
func moneyOf(minor int64, currency string) (money.Currency, money.Money, error) {
	cur, err := money.ParseCurrency(strings.TrimSpace(currency))
	if err != nil {
		return money.Currency{}, money.Money{}, inconsistent("currency", err)
	}
	m, err := money.FromMinor(minor, cur)
	if err != nil {
		return money.Currency{}, money.Money{}, inconsistent("amount", err)
	}
	return cur, m, nil
}

func inconsistent(what string, err error) error {
	return domain.Errorf(domain.ErrInconsistentState, "persisted %s is invalid: %v", what, err)
}
