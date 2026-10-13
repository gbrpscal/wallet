package wallet

import (
	"time"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/money"
)

// Direction of a ledger entry from the wallet's point of view.
type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

// ParseDirection validates a persisted direction.
func ParseDirection(s string) (Direction, error) {
	switch d := Direction(s); d {
	case Debit, Credit:
		return d, nil
	default:
		return "", domain.Errorf(domain.ErrInconsistentState, "unknown ledger direction %q", s)
	}
}

// LedgerEntry is an immutable record of one balance change. It can only be
// built with balanceAfter = balanceBefore ± amount.
type LedgerEntry struct {
	id            domain.LedgerEntryID
	walletID      domain.WalletID
	transactionID domain.TransactionID
	direction     Direction
	amount        money.Money
	balanceBefore money.Money
	balanceAfter  money.Money
	createdAt     time.Time
}

// LedgerEntrySnapshot carries every field of an entry, for reconstruction
// from persistence.
type LedgerEntrySnapshot struct {
	ID            domain.LedgerEntryID
	WalletID      domain.WalletID
	TransactionID domain.TransactionID
	Direction     Direction
	Amount        money.Money
	BalanceBefore money.Money
	BalanceAfter  money.Money
	CreatedAt     time.Time
}

// NewLedgerEntry validates and builds an entry. Any inconsistency is an
// invariant violation, never a client error.
func NewLedgerEntry(s LedgerEntrySnapshot) (LedgerEntry, error) {
	if s.ID.IsZero() || s.WalletID.IsZero() || s.TransactionID.IsZero() {
		return LedgerEntry{}, domain.Errorf(domain.ErrInconsistentState, "ledger entry requires id, wallet and transaction")
	}
	if s.Direction != Debit && s.Direction != Credit {
		return LedgerEntry{}, domain.Errorf(domain.ErrInconsistentState, "ledger entry has invalid direction %q", s.Direction)
	}
	if s.CreatedAt.IsZero() {
		return LedgerEntry{}, domain.Errorf(domain.ErrInconsistentState, "ledger entry requires creation time")
	}
	if !s.Amount.IsValid() || !s.Amount.IsPositive() {
		return LedgerEntry{}, domain.Errorf(domain.ErrInconsistentState, "ledger entry amount must be positive")
	}
	if !s.BalanceBefore.IsValid() || s.BalanceBefore.IsNegative() || !s.BalanceAfter.IsValid() || s.BalanceAfter.IsNegative() {
		return LedgerEntry{}, domain.Errorf(domain.ErrInconsistentState, "ledger entry balances must be non-negative")
	}
	var expected money.Money
	var err error
	if s.Direction == Credit {
		expected, err = s.BalanceBefore.Add(s.Amount)
	} else {
		expected, err = s.BalanceBefore.Sub(s.Amount)
	}
	if err != nil {
		return LedgerEntry{}, domain.Errorf(domain.ErrInconsistentState, "ledger entry arithmetic: %v", err)
	}
	if !expected.Equal(s.BalanceAfter) {
		return LedgerEntry{}, domain.Errorf(domain.ErrInconsistentState,
			"ledger entry balanceAfter %s != balanceBefore %s %s %s", s.BalanceAfter, s.BalanceBefore, s.Direction, s.Amount)
	}
	return LedgerEntry{
		id:            s.ID,
		walletID:      s.WalletID,
		transactionID: s.TransactionID,
		direction:     s.Direction,
		amount:        s.Amount,
		balanceBefore: s.BalanceBefore,
		balanceAfter:  s.BalanceAfter,
		createdAt:     s.CreatedAt,
	}, nil
}

func (e LedgerEntry) ID() domain.LedgerEntryID            { return e.id }
func (e LedgerEntry) WalletID() domain.WalletID           { return e.walletID }
func (e LedgerEntry) TransactionID() domain.TransactionID { return e.transactionID }
func (e LedgerEntry) Direction() Direction                { return e.direction }
func (e LedgerEntry) Amount() money.Money                 { return e.amount }
func (e LedgerEntry) BalanceBefore() money.Money          { return e.balanceBefore }
func (e LedgerEntry) BalanceAfter() money.Money           { return e.balanceAfter }
func (e LedgerEntry) CreatedAt() time.Time                { return e.createdAt }

// SignedAmount returns +amount for credits and -amount for debits, used by
// reconciliation.
func (e LedgerEntry) SignedAmount() (money.Money, error) {
	if e.direction == Credit {
		return e.amount, nil
	}
	return e.amount.Neg()
}
