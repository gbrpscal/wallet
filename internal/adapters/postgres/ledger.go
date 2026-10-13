package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/money"
	"github.com/gbrpscal/wallet/internal/domain/wallet"
)

type ledgerRepo struct {
	q pgx.Tx
}

func (r ledgerRepo) Append(ctx context.Context, e wallet.LedgerEntry) error {
	_, err := r.q.Exec(ctx, `
		INSERT INTO wallet_ledger_entries (
			id, wallet_id, transaction_id, direction, amount_minor, currency,
			balance_before_minor, balance_after_minor, wallet_version, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		e.ID().String(), e.WalletID().String(), e.TransactionID().String(), string(e.Direction()),
		e.Amount().MinorUnits(), e.Amount().Currency().String(),
		e.BalanceBefore().MinorUnits(), e.BalanceAfter().MinorUnits(), e.WalletVersion(), e.CreatedAt())
	return translate(err)
}

func (r ledgerRepo) List(ctx context.Context, walletID domain.WalletID, afterVersion int64, limit int) ([]wallet.LedgerEntry, error) {
	rows, err := r.q.Query(ctx, `
		SELECT id::text, wallet_id::text, transaction_id::text, direction, amount_minor, currency,
		       balance_before_minor, balance_after_minor, wallet_version, created_at
		FROM wallet_ledger_entries
		WHERE wallet_id = $1 AND wallet_version > $2
		ORDER BY wallet_version
		LIMIT $3`, walletID.String(), afterVersion, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var entries []wallet.LedgerEntry
	for rows.Next() {
		var (
			id, wid, txID, direction, currency string
			amount, before, after, version     int64
			createdAt                          time.Time
		)
		if err := rows.Scan(&id, &wid, &txID, &direction, &amount, &currency, &before, &after, &version, &createdAt); err != nil {
			return nil, translate(err)
		}
		entry, err := ledgerEntryOf(id, wid, txID, direction, currency, amount, before, after, version, createdAt)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, translate(rows.Err())
}

func ledgerEntryOf(id, walletID, txID, direction, currency string, amount, before, after, version int64, createdAt time.Time) (wallet.LedgerEntry, error) {
	s := wallet.LedgerEntrySnapshot{WalletVersion: version, CreatedAt: domain.Timestamp(createdAt)}
	var err error
	if s.ID, err = domain.ParseID[domain.LedgerEntryID](id); err != nil {
		return wallet.LedgerEntry{}, inconsistent("ledger id", err)
	}
	if s.WalletID, err = domain.ParseID[domain.WalletID](walletID); err != nil {
		return wallet.LedgerEntry{}, inconsistent("ledger wallet", err)
	}
	if s.TransactionID, err = domain.ParseID[domain.TransactionID](txID); err != nil {
		return wallet.LedgerEntry{}, inconsistent("ledger transaction", err)
	}
	if s.Direction, err = wallet.ParseDirection(direction); err != nil {
		return wallet.LedgerEntry{}, err
	}
	var cur money.Currency
	if cur, s.Amount, err = moneyOf(amount, currency); err != nil {
		return wallet.LedgerEntry{}, err
	}
	if s.BalanceBefore, err = money.FromMinor(before, cur); err != nil {
		return wallet.LedgerEntry{}, inconsistent("ledger balance", err)
	}
	if s.BalanceAfter, err = money.FromMinor(after, cur); err != nil {
		return wallet.LedgerEntry{}, inconsistent("ledger balance", err)
	}
	return wallet.NewLedgerEntry(s)
}
