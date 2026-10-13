package wagering

import (
	"errors"
	"time"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/event"
	"github.com/gbrpscal/wallet/internal/domain/money"
	"github.com/gbrpscal/wallet/internal/domain/wallet"
)

// Reference is what the caller found for the transaction's reference, loaded
// in the same SQL transaction: by (providerId, referenceExternalTransactionId).
type Reference struct {
	// Transaction is nil when no transaction with that external id exists.
	Transaction *WagerTransaction
	// Reversed is true when a PROCESSED reversal already points to it.
	Reversed bool
}

// Outcome is everything one processing step produced. The caller persists the
// transaction, the wallet, the entry and the events in a single commit.
type Outcome struct {
	Transaction *WagerTransaction
	// Entry is nil when the balance did not change.
	Entry  *wallet.LedgerEntry
	Events []event.Event
	// Waiting is true when the transaction stays PENDING_REFERENCE with no
	// change at all; there is nothing to persist except retry bookkeeping.
	Waiting bool
}

// Process applies the rules of the transaction's kind to the wallet.
// Business rejections are not errors: they are returned as an Outcome with
// the transaction REJECTED. A returned error means nothing may be persisted.
func Process(tx *WagerTransaction, w *wallet.Wallet, ref Reference, trace event.Trace, now time.Time) (Outcome, error) {
	if tx == nil || w == nil {
		return Outcome{}, domain.Errorf(domain.ErrInconsistentState, "process requires transaction and wallet")
	}
	if tx.status != Pending && tx.status != PendingReference {
		return Outcome{}, domain.Errorf(domain.ErrInvalidTransition, "transaction %s is %s", tx.id, tx.status)
	}
	if tx.kind == Opening {
		return Outcome{}, domain.Errorf(domain.ErrInconsistentState, "opening is completed by OpenWallet")
	}
	if w.ID() != tx.walletID {
		return Outcome{}, domain.Errorf(domain.ErrInconsistentState, "wallet %s does not match transaction wallet %s", w.ID(), tx.walletID)
	}
	if err := trace.Validate(); err != nil {
		return Outcome{}, err
	}

	p := processor{tx: tx, w: w, trace: trace, now: now}
	if w.PlayerID() != tx.playerID {
		return p.reject(domain.CodeWalletPlayerMismatch)
	}
	if w.Currency() != tx.money.Currency() {
		return p.reject(domain.CodeCurrencyMismatch)
	}

	switch tx.kind {
	case Bet:
		return p.move(wallet.Debit, domain.TransactionID{}, domain.CodeInsufficientFunds)
	case Loss:
		return p.processed(nil, domain.TransactionID{})
	case Win:
		return p.win(ref)
	case Refund, Rollback:
		return p.reversal(ref)
	default:
		return Outcome{}, domain.Errorf(domain.ErrInconsistentState, "unknown kind %q", tx.kind)
	}
}

// Expire finalizes a transaction whose reference never became available
// within the retry policy. w may be nil when the wallet is not loaded; the
// observed balance is then not recorded.
func Expire(tx *WagerTransaction, w *wallet.Wallet, trace event.Trace, now time.Time) (Outcome, error) {
	if tx == nil {
		return Outcome{}, domain.Errorf(domain.ErrInconsistentState, "expire requires a transaction")
	}
	if tx.status != PendingReference {
		return Outcome{}, domain.Errorf(domain.ErrInvalidTransition, "only %s transactions expire, %s is %s", PendingReference, tx.id, tx.status)
	}
	if err := trace.Validate(); err != nil {
		return Outcome{}, err
	}
	p := processor{tx: tx, w: w, trace: trace, now: now}
	return p.reject(domain.CodeReferenceNotFound)
}

type processor struct {
	tx    *WagerTransaction
	w     *wallet.Wallet
	trace event.Trace
	now   time.Time
}

func (p processor) win(ref Reference) (Outcome, error) {
	if ref.Transaction == nil {
		// The WIN credit does not depend on the bet: an unknown optional
		// reference stays recorded and is not resolved.
		return p.move(wallet.Credit, domain.TransactionID{}, "")
	}
	r := ref.Transaction
	if r.kind != Bet || !p.sameScope(r) {
		return p.reject(domain.CodeReferenceMismatch)
	}
	if r.status != Processed {
		return p.reject(domain.CodeReferenceNotProcessed)
	}
	return p.move(wallet.Credit, r.id, "")
}

// reversal implements REFUND and ROLLBACK. Static checks come first so a
// request that can never succeed is rejected without waiting.
func (p processor) reversal(ref Reference) (Outcome, error) {
	r := ref.Transaction
	if r == nil {
		return p.wait()
	}
	if !reversible(p.tx.kind, r.kind) {
		return p.reject(domain.CodeReferenceNotReversible)
	}
	if !p.sameScope(r) {
		return p.reject(domain.CodeReferenceMismatch)
	}
	if !r.money.Equal(p.tx.money) {
		return p.reject(domain.CodeReferenceAmount)
	}
	switch r.status {
	case Pending, PendingReference:
		return p.wait()
	case Rejected, Failed:
		return p.reject(domain.CodeReferenceNotProcessed)
	}
	if ref.Reversed {
		return p.reject(domain.CodeAlreadyReversed)
	}
	// The reversal moves money in the opposite direction of the original.
	if r.kind == Bet {
		return p.move(wallet.Credit, r.id, "")
	}
	return p.move(wallet.Debit, r.id, domain.CodeReversalInsufficient)
}

// reversible: REFUND undoes a BET; ROLLBACK undoes a BET, WIN or REFUND.
func reversible(kind, target Kind) bool {
	switch kind {
	case Refund:
		return target == Bet
	case Rollback:
		return target == Bet || target == Win || target == Refund
	default:
		return false
	}
}

// sameScope checks the fields an operation and its reference must share.
func (p processor) sameScope(r *WagerTransaction) bool {
	return r.providerID == p.tx.providerID &&
		r.playerID == p.tx.playerID &&
		r.walletID == p.tx.walletID &&
		r.money.Currency() == p.tx.money.Currency() &&
		r.roundID == p.tx.roundID
}

// move debits or credits the wallet. insufficient is the rejection code used
// when a debit would make the balance negative.
func (p processor) move(dir wallet.Direction, ref domain.TransactionID, insufficient domain.Code) (Outcome, error) {
	var entry wallet.LedgerEntry
	var err error
	if dir == wallet.Debit {
		entry, err = p.w.Debit(p.tx.id, p.tx.money, p.now)
	} else {
		entry, err = p.w.Credit(p.tx.id, p.tx.money, p.now)
	}
	if err != nil {
		if errors.Is(err, domain.ErrInsufficientFunds) && insufficient != "" {
			return p.reject(insufficient)
		}
		return Outcome{}, err
	}
	return p.processed(&entry, ref)
}

func (p processor) processed(entry *wallet.LedgerEntry, ref domain.TransactionID) (Outcome, error) {
	if err := p.tx.MarkProcessed(p.w.Balance(), ref, p.now); err != nil {
		return Outcome{}, err
	}
	data := event.WagerTransactionProcessedData{TransactionRef: transactionRef(p.tx), Balance: p.w.Balance()}
	if !ref.IsZero() {
		data.ReferenceTransactionID = ref.String()
	}
	processed, err := event.NewWagerTransactionProcessed(p.trace, p.now, data)
	if err != nil {
		return Outcome{}, err
	}
	events := []event.Event{processed}
	if entry != nil {
		changed, err := event.NewWalletBalanceChanged(p.trace, p.now, *entry, p.w.Version())
		if err != nil {
			return Outcome{}, err
		}
		events = append(events, changed)
	}
	return Outcome{Transaction: p.tx, Entry: entry, Events: events}, nil
}

func (p processor) reject(code domain.Code) (Outcome, error) {
	// The observed balance is only recorded when it is expressed in the
	// operation's currency (not the case for CURRENCY_MISMATCH).
	var observed money.Money
	if p.w != nil && p.w.Currency() == p.tx.money.Currency() {
		observed = p.w.Balance()
	}
	if err := p.tx.MarkRejected(code, observed, p.now); err != nil {
		return Outcome{}, err
	}
	rejected, err := event.NewWagerTransactionRejected(p.trace, p.now, event.WagerTransactionRejectedData{
		TransactionRef:                 transactionRef(p.tx),
		ReferenceExternalTransactionID: p.tx.referenceExternalID,
		FailureCode:                    code,
	})
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Transaction: p.tx, Events: []event.Event{rejected}}, nil
}

// wait moves PENDING to PENDING_REFERENCE with its event; a transaction
// already waiting stays as it is, with nothing to persist.
func (p processor) wait() (Outcome, error) {
	if p.tx.status == PendingReference {
		return Outcome{Transaction: p.tx, Waiting: true}, nil
	}
	if err := p.tx.MarkPendingReference(p.now); err != nil {
		return Outcome{}, err
	}
	pending, err := event.NewWagerTransactionPendingReference(p.trace, p.now, event.WagerTransactionPendingReferenceData{
		TransactionRef:                 transactionRef(p.tx),
		ReferenceExternalTransactionID: p.tx.referenceExternalID,
	})
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Transaction: p.tx, Events: []event.Event{pending}}, nil
}

func transactionRef(t *WagerTransaction) event.TransactionRef {
	return event.TransactionRef{
		TransactionID:         t.id,
		WalletID:              t.walletID,
		PlayerID:              t.playerID,
		Kind:                  string(t.kind),
		Money:                 t.money,
		ProviderID:            t.providerID,
		ExternalTransactionID: t.externalID,
		RoundID:               t.roundID,
		GameID:                t.gameID,
	}
}

// OpenOutcome is everything opening a wallet produced, committed together.
type OpenOutcome struct {
	Wallet *wallet.Wallet
	// Opening, Entry and Events are empty for a zero initial balance.
	Opening *WagerTransaction
	Entry   *wallet.LedgerEntry
	Events  []event.Event
}

// OpenWallet creates a wallet. A positive initial balance also produces the
// internal OPENING transaction (PROCESSED), its credit entry and the
// WagerTransactionProcessed and WalletBalanceChanged events. A zero initial
// balance produces none of them.
func OpenWallet(playerID domain.PlayerID, initial money.Money, trace event.Trace, now time.Time) (OpenOutcome, error) {
	if err := trace.Validate(); err != nil {
		return OpenOutcome{}, err
	}
	walletID, err := domain.NewID[domain.WalletID]()
	if err != nil {
		return OpenOutcome{}, err
	}
	var openingID domain.TransactionID
	if initial.IsValid() && initial.IsPositive() {
		if openingID, err = domain.NewID[domain.TransactionID](); err != nil {
			return OpenOutcome{}, err
		}
	}
	w, entry, err := wallet.Open(walletID, playerID, initial, openingID, now)
	if err != nil {
		return OpenOutcome{}, err
	}
	if entry == nil {
		return OpenOutcome{Wallet: w}, nil
	}

	opening, err := newOpening(openingID, walletID, playerID, initial, now)
	if err != nil {
		return OpenOutcome{}, err
	}
	p := processor{tx: opening, w: w, trace: trace, now: now}
	out, err := p.processed(entry, domain.TransactionID{})
	if err != nil {
		return OpenOutcome{}, err
	}
	return OpenOutcome{Wallet: w, Opening: opening, Entry: entry, Events: out.Events}, nil
}
