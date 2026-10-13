// Package wallet holds the Wallet aggregate root and its ledger entries.
package wallet

import (
	"time"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/money"
)

// InitialVersion is the version of a newly opened wallet.
const InitialVersion int64 = 1

// Wallet is the financial aggregate root. Its balance only changes through
// Debit and Credit, each of which returns the matching ledger entry; the
// caller must persist both in the same SQL transaction.
type Wallet struct {
	id        domain.WalletID
	playerID  domain.PlayerID
	currency  money.Currency
	balance   money.Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

// Snapshot carries every field of a wallet, for reconstruction from
// persistence.
type Snapshot struct {
	ID        domain.WalletID
	PlayerID  domain.PlayerID
	Currency  money.Currency
	Balance   money.Money
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Open creates a wallet with version 1. A positive initial balance produces
// the opening credit entry, linked to the internal OPENING transaction; a zero
// initial balance produces no entry and openingTx may be zero.
func Open(id domain.WalletID, playerID domain.PlayerID, initial money.Money, openingTx domain.TransactionID, now time.Time) (*Wallet, *LedgerEntry, error) {
	if id.IsZero() || playerID.IsZero() {
		return nil, nil, domain.Errorf(domain.ErrInvalidRequest, "wallet requires id and player")
	}
	if !initial.IsValid() {
		return nil, nil, domain.Errorf(domain.ErrInvalidAmount, "initial balance is uninitialized")
	}
	if initial.IsNegative() {
		return nil, nil, domain.Errorf(domain.ErrInvalidAmount, "initial balance must not be negative")
	}
	if now.IsZero() {
		return nil, nil, domain.Errorf(domain.ErrInconsistentState, "wallet requires creation time")
	}
	now = domain.Timestamp(now)
	w := &Wallet{
		id:        id,
		playerID:  playerID,
		currency:  initial.Currency(),
		balance:   initial,
		version:   InitialVersion,
		createdAt: now,
		updatedAt: now,
	}
	if initial.IsZero() {
		return w, nil, nil
	}
	if openingTx.IsZero() {
		return nil, nil, domain.Errorf(domain.ErrInconsistentState, "positive initial balance requires an opening transaction")
	}
	zero, _ := money.Zero(initial.Currency())
	entry, err := newEntry(id, openingTx, Credit, initial, zero, initial, now)
	if err != nil {
		return nil, nil, err
	}
	return w, &entry, nil
}

// Rehydrate rebuilds a persisted wallet without applying any movement.
// Persisted data that violates an invariant is reported as INCONSISTENT_STATE.
func Rehydrate(s Snapshot) (*Wallet, error) {
	switch {
	case s.ID.IsZero() || s.PlayerID.IsZero():
		return nil, domain.Errorf(domain.ErrInconsistentState, "wallet requires id and player")
	case s.Currency.IsZero() || !s.Balance.IsValid() || s.Balance.Currency() != s.Currency:
		return nil, domain.Errorf(domain.ErrInconsistentState, "wallet %s balance currency mismatch", s.ID)
	case s.Balance.IsNegative():
		return nil, domain.Errorf(domain.ErrInconsistentState, "wallet %s has negative balance", s.ID)
	case s.Version < InitialVersion:
		return nil, domain.Errorf(domain.ErrInconsistentState, "wallet %s has invalid version %d", s.ID, s.Version)
	case s.CreatedAt.IsZero() || s.UpdatedAt.Before(s.CreatedAt):
		return nil, domain.Errorf(domain.ErrInconsistentState, "wallet %s has invalid timestamps", s.ID)
	}
	return &Wallet{
		id:        s.ID,
		playerID:  s.PlayerID,
		currency:  s.Currency,
		balance:   s.Balance,
		version:   s.Version,
		createdAt: s.CreatedAt,
		updatedAt: s.UpdatedAt,
	}, nil
}

// Debit subtracts amount, keeping the balance non-negative. On insufficient
// funds it returns ErrInsufficientFunds and leaves the wallet unchanged; the
// caller decides which rejection code applies (bet or reversal).
func (w *Wallet) Debit(txID domain.TransactionID, amount money.Money, now time.Time) (LedgerEntry, error) {
	if err := w.checkMovement(txID, amount, now); err != nil {
		return LedgerEntry{}, err
	}
	cmp, err := w.balance.Cmp(amount)
	if err != nil {
		return LedgerEntry{}, err
	}
	if cmp < 0 {
		return LedgerEntry{}, domain.Errorf(domain.ErrInsufficientFunds, "balance %s is lower than %s", w.balance, amount)
	}
	after, err := w.balance.Sub(amount)
	if err != nil {
		return LedgerEntry{}, err
	}
	return w.apply(txID, Debit, amount, after, now)
}

// Credit adds amount.
func (w *Wallet) Credit(txID domain.TransactionID, amount money.Money, now time.Time) (LedgerEntry, error) {
	if err := w.checkMovement(txID, amount, now); err != nil {
		return LedgerEntry{}, err
	}
	after, err := w.balance.Add(amount)
	if err != nil {
		return LedgerEntry{}, err
	}
	return w.apply(txID, Credit, amount, after, now)
}

func (w *Wallet) checkMovement(txID domain.TransactionID, amount money.Money, now time.Time) error {
	if txID.IsZero() {
		return domain.Errorf(domain.ErrInconsistentState, "movement requires a transaction id")
	}
	if now.IsZero() {
		return domain.Errorf(domain.ErrInconsistentState, "movement requires a timestamp")
	}
	if !amount.IsValid() {
		return domain.Errorf(domain.ErrInvalidAmount, "movement amount is uninitialized")
	}
	if amount.Currency() != w.currency {
		return domain.Errorf(domain.ErrCurrencyMismatch, "movement in %s on %s wallet", amount.Currency(), w.currency)
	}
	if !amount.IsPositive() {
		return domain.Errorf(domain.ErrAmountMustBePositive, "movement amount must be positive")
	}
	return nil
}

// apply builds the entry first and only then mutates the wallet, so a failure
// leaves the aggregate untouched. The version increments only here, i.e. only
// when the balance changes.
func (w *Wallet) apply(txID domain.TransactionID, dir Direction, amount, after money.Money, now time.Time) (LedgerEntry, error) {
	now = domain.Timestamp(now)
	entry, err := newEntry(w.id, txID, dir, amount, w.balance, after, now)
	if err != nil {
		return LedgerEntry{}, err
	}
	w.balance = after
	w.version++
	if now.After(w.updatedAt) {
		w.updatedAt = now
	}
	return entry, nil
}

func newEntry(walletID domain.WalletID, txID domain.TransactionID, dir Direction, amount, before, after money.Money, now time.Time) (LedgerEntry, error) {
	entryID, err := domain.NewID[domain.LedgerEntryID]()
	if err != nil {
		return LedgerEntry{}, err
	}
	return NewLedgerEntry(LedgerEntrySnapshot{
		ID:            entryID,
		WalletID:      walletID,
		TransactionID: txID,
		Direction:     dir,
		Amount:        amount,
		BalanceBefore: before,
		BalanceAfter:  after,
		CreatedAt:     now,
	})
}

func (w *Wallet) ID() domain.WalletID       { return w.id }
func (w *Wallet) PlayerID() domain.PlayerID { return w.playerID }
func (w *Wallet) Currency() money.Currency  { return w.currency }
func (w *Wallet) Balance() money.Money      { return w.balance }
func (w *Wallet) Version() int64            { return w.version }
func (w *Wallet) CreatedAt() time.Time      { return w.createdAt }
func (w *Wallet) UpdatedAt() time.Time      { return w.updatedAt }
