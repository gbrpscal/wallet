package wagering_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/event"
	"github.com/gbrpscal/wallet/internal/domain/money"
	"github.com/gbrpscal/wallet/internal/domain/wagering"
	"github.com/gbrpscal/wallet/internal/domain/wallet"
)

var (
	processedOnly = []string{event.TypeWagerTransactionProcessed}
	processedMove = []string{event.TypeWagerTransactionProcessed, event.TypeWalletBalanceChanged}
	rejectedOnly  = []string{event.TypeWagerTransactionRejected}
	pendingOnly   = []string{event.TypeWagerTransactionPendingReference}
)

func assertRejected(t *testing.T, out wagering.Outcome, w *wallet.Wallet, code domain.Code, wantBalance string, wantVersion int64) {
	t.Helper()
	tx := out.Transaction
	if tx.Status() != wagering.Rejected || tx.FailureCode() != code {
		t.Fatalf("status = %s %s, want REJECTED %s", tx.Status(), tx.FailureCode(), code)
	}
	if out.Entry != nil {
		t.Error("rejection must not produce a ledger entry")
	}
	if !slices.Equal(eventTypes(out), rejectedOnly) {
		t.Errorf("events = %v", eventTypes(out))
	}
	if w.Balance().Amount() != wantBalance || w.Version() != wantVersion {
		t.Errorf("wallet changed on rejection: %v v%d", w.Balance(), w.Version())
	}
	if observed, ok := tx.ResultBalance(); !ok || observed.Amount() != wantBalance {
		t.Errorf("observed balance = %v %v", observed, ok)
	}
}

func process(t *testing.T, tx *wagering.WagerTransaction, w *wallet.Wallet, ref wagering.Reference) wagering.Outcome {
	t.Helper()
	out, err := wagering.Process(tx, w, ref, trace, t0.Add(time.Second))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	return out
}

// processedRef builds a PROCESSED transaction to be used as a reference.
func processedRef(t *testing.T, w *wallet.Wallet, opts ...func(*wagering.RequestInput)) *wagering.WagerTransaction {
	t.Helper()
	tx := pending(t, opts...)
	processed(t, tx, w, wagering.Reference{})
	return tx
}

func TestBetDebits(t *testing.T) {
	w := openWallet(t, "1000.00")
	out := processed(t, pending(t), w, wagering.Reference{})

	if w.Balance().Amount() != "975.00" || w.Version() != 2 {
		t.Errorf("wallet = %v v%d", w.Balance(), w.Version())
	}
	if out.Entry == nil || out.Entry.Direction() != wallet.Debit || out.Entry.BalanceAfter().Amount() != "975.00" {
		t.Errorf("entry = %+v", out.Entry)
	}
	if b, _ := out.Transaction.ResultBalance(); b.Amount() != "975.00" {
		t.Errorf("result balance = %v", b)
	}
	if !slices.Equal(eventTypes(out), processedMove) {
		t.Errorf("events = %v", eventTypes(out))
	}
}

// TestTwoBetsOf80OnWalletWith100 is the domain view of the mandatory
// scenario: one bet is processed, the other rejected for insufficient funds.
func TestTwoBetsOf80OnWalletWith100(t *testing.T) {
	w := openWallet(t, "100.00")
	first := processed(t, pending(t, op("BET", "bet-1", "80.00", "")), w, wagering.Reference{})
	second := process(t, pending(t, op("BET", "bet-2", "80.00", "")), w, wagering.Reference{})

	if first.Entry == nil {
		t.Fatal("first bet must debit")
	}
	assertRejected(t, second, w, domain.CodeInsufficientFunds, "20.00", 2)
}

func TestLossHasNoMovement(t *testing.T) {
	w := openWallet(t, "1000.00")
	out := processed(t, pending(t, op("LOSS", "loss-1", "0.00", "")), w, wagering.Reference{})

	if out.Entry != nil {
		t.Error("LOSS must not produce a ledger entry")
	}
	if w.Version() != 1 || w.Balance().Amount() != "1000.00" {
		t.Errorf("LOSS changed the wallet: %v v%d", w.Balance(), w.Version())
	}
	if !slices.Equal(eventTypes(out), processedOnly) {
		t.Errorf("LOSS events = %v, want only WagerTransactionProcessed", eventTypes(out))
	}
}

func TestWin(t *testing.T) {
	t.Run("without reference", func(t *testing.T) {
		w := openWallet(t, "100.00")
		out := processed(t, pending(t, op("WIN", "win-1", "50.00", "")), w, wagering.Reference{})
		if w.Balance().Amount() != "150.00" || out.Entry.Direction() != wallet.Credit {
			t.Errorf("wallet = %v", w.Balance())
		}
	})
	t.Run("unknown optional reference is not awaited", func(t *testing.T) {
		w := openWallet(t, "100.00")
		tx := pending(t, op("WIN", "win-1", "50.00", "bet-404"))
		processed(t, tx, w, wagering.Reference{})
		if _, ok := tx.ReferenceTransactionID(); ok {
			t.Error("unknown reference must stay unresolved")
		}
	})
	t.Run("resolved reference", func(t *testing.T) {
		w := openWallet(t, "100.00")
		bet := processedRef(t, w, op("BET", "bet-1", "10.00", ""))
		tx := pending(t, op("WIN", "win-1", "50.00", "bet-1"))
		processed(t, tx, w, wagering.Reference{Transaction: bet})
		if id, _ := tx.ReferenceTransactionID(); id != bet.ID() {
			t.Error("WIN must record the resolved bet")
		}
	})
	t.Run("reference of another round", func(t *testing.T) {
		w := openWallet(t, "100.00")
		bet := processedRef(t, w, op("BET", "bet-1", "10.00", ""), func(in *wagering.RequestInput) { in.RoundID = "round-1" })
		out := process(t, pending(t, op("WIN", "win-1", "50.00", "bet-1")), w, wagering.Reference{Transaction: bet})
		assertRejected(t, out, w, domain.CodeReferenceMismatch, "90.00", 2)
	})
	t.Run("reference that is not a bet", func(t *testing.T) {
		w := openWallet(t, "100.00")
		win := processedRef(t, w, op("WIN", "win-0", "10.00", ""))
		out := process(t, pending(t, op("WIN", "win-1", "50.00", "win-0")), w, wagering.Reference{Transaction: win})
		assertRejected(t, out, w, domain.CodeReferenceMismatch, "110.00", 2)
	})
	t.Run("reference not processed", func(t *testing.T) {
		w := openWallet(t, "100.00")
		bet := pending(t, op("BET", "bet-1", "10.00", ""))
		out := process(t, pending(t, op("WIN", "win-1", "50.00", "bet-1")), w, wagering.Reference{Transaction: bet})
		assertRejected(t, out, w, domain.CodeReferenceNotProcessed, "100.00", 1)
	})
}

func TestRefund(t *testing.T) {
	w := openWallet(t, "100.00")
	bet := processedRef(t, w, op("BET", "bet-1", "30.00", ""))

	tx := pending(t, op("REFUND", "refund-1", "30.00", "bet-1"))
	out := processed(t, tx, w, wagering.Reference{Transaction: bet})
	if w.Balance().Amount() != "100.00" || w.Version() != 3 || out.Entry.Direction() != wallet.Credit {
		t.Errorf("wallet = %v v%d", w.Balance(), w.Version())
	}
	if id, _ := tx.ReferenceTransactionID(); id != bet.ID() {
		t.Error("refund must record the resolved bet")
	}
}

func TestRollbackDirections(t *testing.T) {
	cases := []struct {
		name     string
		original func(*wagering.RequestInput)
		dir      wallet.Direction
		balance  string
	}{
		{"of bet credits", op("BET", "orig", "30.00", ""), wallet.Credit, "100.00"},
		{"of win debits", op("WIN", "orig", "30.00", ""), wallet.Debit, "100.00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := openWallet(t, "100.00")
			orig := processedRef(t, w, c.original)
			out := processed(t, pending(t, op("ROLLBACK", "rb-1", "30.00", "orig")), w, wagering.Reference{Transaction: orig})
			if out.Entry.Direction() != c.dir || w.Balance().Amount() != c.balance {
				t.Errorf("entry %s, balance %v", out.Entry.Direction(), w.Balance())
			}
		})
	}
	t.Run("of refund debits", func(t *testing.T) {
		w := openWallet(t, "100.00")
		bet := processedRef(t, w, op("BET", "bet-1", "30.00", ""))
		refund := pending(t, op("REFUND", "refund-1", "30.00", "bet-1"))
		processed(t, refund, w, wagering.Reference{Transaction: bet})
		out := processed(t, pending(t, op("ROLLBACK", "rb-1", "30.00", "refund-1")), w, wagering.Reference{Transaction: refund})
		if out.Entry.Direction() != wallet.Debit || w.Balance().Amount() != "70.00" {
			t.Errorf("entry %s, balance %v", out.Entry.Direction(), w.Balance())
		}
	})
}

func TestReversalWithoutFundsUsesDistinctCode(t *testing.T) {
	w := openWallet(t, "0.00")
	win := processedRef(t, w, op("WIN", "win-1", "50.00", ""))
	processed(t, pending(t, op("BET", "bet-1", "40.00", "")), w, wagering.Reference{})

	out := process(t, pending(t, op("ROLLBACK", "rb-1", "50.00", "win-1")), w, wagering.Reference{Transaction: win})
	assertRejected(t, out, w, domain.CodeReversalInsufficient, "10.00", 3)
	if out.Transaction.FailureCode() == domain.CodeInsufficientFunds {
		t.Error("reversal must not reuse the bet insufficient funds code")
	}
}

func TestReversalRejections(t *testing.T) {
	cases := []struct {
		name     string
		original func(*wagering.RequestInput)
		reversal func(*wagering.RequestInput)
		reversed bool
		code     domain.Code
	}{
		{"refund of win", op("WIN", "orig", "30.00", ""), op("REFUND", "rev", "30.00", "orig"), false, domain.CodeReferenceNotReversible},
		{"rollback of loss", op("LOSS", "orig", "0.00", ""), op("ROLLBACK", "rev", "30.00", "orig"), false, domain.CodeReferenceNotReversible},
		{"different amount", op("BET", "orig", "30.00", ""), op("REFUND", "rev", "29.99", "orig"), false, domain.CodeReferenceAmount},
		{"already reversed", op("BET", "orig", "30.00", ""), op("ROLLBACK", "rev", "30.00", "orig"), true, domain.CodeAlreadyReversed},
		{"other round", op("BET", "orig", "30.00", ""), func(in *wagering.RequestInput) {
			op("REFUND", "rev", "30.00", "orig")(in)
			in.RoundID = "round-other"
		}, false, domain.CodeReferenceMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := openWallet(t, "100.00")
			orig := processedRef(t, w, c.original)
			balance, version := w.Balance().Amount(), w.Version()
			out := process(t, pending(t, c.reversal), w, wagering.Reference{Transaction: orig, Reversed: c.reversed})
			assertRejected(t, out, w, c.code, balance, version)
		})
	}
}

func TestReversalOfRejectedReference(t *testing.T) {
	w := openWallet(t, "10.00")
	bet := pending(t, op("BET", "bet-1", "30.00", ""))
	assertRejected(t, process(t, bet, w, wagering.Reference{}), w, domain.CodeInsufficientFunds, "10.00", 1)

	out := process(t, pending(t, op("REFUND", "refund-1", "30.00", "bet-1")), w, wagering.Reference{Transaction: bet})
	assertRejected(t, out, w, domain.CodeReferenceNotProcessed, "10.00", 1)
}

// TestReversalBeforeReference covers a REFUND arriving before its BET: it
// waits, stays waiting while the reference is missing or pending, and
// completes once the reference is processed.
func TestReversalBeforeReference(t *testing.T) {
	w := openWallet(t, "100.00")
	refund := pending(t, op("REFUND", "refund-1", "30.00", "bet-1"))

	out := process(t, refund, w, wagering.Reference{})
	if refund.Status() != wagering.PendingReference || !slices.Equal(eventTypes(out), pendingOnly) || out.Waiting {
		t.Fatalf("first attempt: %s %v waiting=%v", refund.Status(), eventTypes(out), out.Waiting)
	}

	again := process(t, refund, w, wagering.Reference{})
	if !again.Waiting || len(again.Events) != 0 || refund.Status() != wagering.PendingReference {
		t.Fatalf("retry while missing must be a no-op: %+v", again)
	}

	bet := pending(t, op("BET", "bet-1", "30.00", ""))
	stillPending := process(t, refund, w, wagering.Reference{Transaction: bet})
	if !stillPending.Waiting {
		t.Fatal("a pending reference must keep the reversal waiting")
	}

	processed(t, bet, w, wagering.Reference{})
	done := processed(t, refund, w, wagering.Reference{Transaction: bet})
	if w.Balance().Amount() != "100.00" || !slices.Equal(eventTypes(done), processedMove) {
		t.Errorf("balance %v, events %v", w.Balance(), eventTypes(done))
	}
}

func TestExpire(t *testing.T) {
	w := openWallet(t, "100.00")
	refund := pending(t, op("ROLLBACK", "rb-1", "30.00", "bet-404"))
	if _, err := wagering.Expire(refund, w, trace, t0); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("PENDING must not expire: %v", err)
	}
	process(t, refund, w, wagering.Reference{})

	out, err := wagering.Expire(refund, w, trace, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	assertRejected(t, out, w, domain.CodeReferenceNotFound, "100.00", 1)
}

func TestScopeChecksBeforeKindRules(t *testing.T) {
	t.Run("wallet of another player", func(t *testing.T) {
		w := openWallet(t, "100.00")
		tx := pending(t, func(in *wagering.RequestInput) { in.PlayerID = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a2" })
		assertRejected(t, process(t, tx, w, wagering.Reference{}), w, domain.CodeWalletPlayerMismatch, "100.00", 1)
	})
	t.Run("currency differs from wallet", func(t *testing.T) {
		w := openWallet(t, "100.00")
		tx := pending(t, func(in *wagering.RequestInput) { in.Currency = "USD" })
		out := process(t, tx, w, wagering.Reference{})
		if out.Transaction.FailureCode() != domain.CodeCurrencyMismatch {
			t.Errorf("code = %s", out.Transaction.FailureCode())
		}
		if _, ok := out.Transaction.ResultBalance(); ok {
			t.Error("observed balance in another currency must not be recorded")
		}
	})
}

func TestProcessGuards(t *testing.T) {
	w := openWallet(t, "100.00")
	tx := pending(t)
	processed(t, tx, w, wagering.Reference{})
	if _, err := wagering.Process(tx, w, wagering.Reference{}, trace, t0); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("terminal transaction reprocessed: %v", err)
	}
	other := openWalletWithID(t, "100.00")
	if _, err := wagering.Process(pending(t), other, wagering.Reference{}, trace, t0); !errors.Is(err, domain.ErrInconsistentState) {
		t.Errorf("wrong wallet accepted: %v", err)
	}
	if _, err := wagering.Process(pending(t), w, wagering.Reference{}, event.Trace{}, t0); !errors.Is(err, domain.ErrInvalidRequest) {
		t.Errorf("missing correlation id accepted: %v", err)
	}
}

func openWalletWithID(t *testing.T, initial string) *wallet.Wallet {
	t.Helper()
	id, _ := domain.NewID[domain.WalletID]()
	opening, _ := domain.NewID[domain.TransactionID]()
	w, _, err := wallet.Open(id, playerID, brl(t, initial), opening, t0)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestOpenWalletPositive(t *testing.T) {
	out, err := wagering.OpenWallet(playerID, brl(t, "1000.00"), trace, t0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Wallet.Version() != 1 || out.Wallet.Balance().Amount() != "1000.00" {
		t.Errorf("wallet = %v v%d", out.Wallet.Balance(), out.Wallet.Version())
	}
	op := out.Opening
	if op == nil || op.Kind() != wagering.Opening || op.Status() != wagering.Processed || !op.IsInternal() {
		t.Fatalf("opening = %+v", op)
	}
	if op.ProviderID() != "" || op.ExternalTransactionID() != "" || op.IdempotencyKey() != "" ||
		op.PayloadHash() != "" || op.RoundID() != "" || op.GameID() != "" {
		t.Error("opening must not carry external metadata")
	}
	if out.Entry == nil || out.Entry.TransactionID() != op.ID() || out.Entry.Direction() != wallet.Credit {
		t.Errorf("entry = %+v", out.Entry)
	}
	types := make([]string, 0)
	for _, e := range out.Events {
		types = append(types, e.Type())
	}
	if !slices.Equal(types, processedMove) {
		t.Errorf("events = %v", types)
	}
	if _, err := wagering.Rehydrate(op.Snapshot()); err != nil {
		t.Errorf("opening must rehydrate: %v", err)
	}
}

func TestOpenWalletZero(t *testing.T) {
	out, err := wagering.OpenWallet(playerID, brl(t, "0.00"), trace, t0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Opening != nil || out.Entry != nil || len(out.Events) != 0 {
		t.Errorf("zero opening produced %+v", out)
	}
	if out.Wallet.Version() != 1 {
		t.Errorf("version = %d", out.Wallet.Version())
	}
	if _, err := wagering.OpenWallet(playerID, money.Money{}, trace, t0); err == nil {
		t.Error("uninitialized initial balance accepted")
	}
}
