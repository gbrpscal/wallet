package wagering_test

import (
	"errors"
	"testing"
	"time"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/money"
	"github.com/gbrpscal/wallet/internal/domain/wagering"
)

func TestStateMachine(t *testing.T) {
	all := []wagering.Status{wagering.Pending, wagering.PendingReference, wagering.Processed, wagering.Rejected, wagering.Failed}
	allowed := map[[2]wagering.Status]bool{
		{wagering.Pending, wagering.Processed}:          true,
		{wagering.Pending, wagering.Rejected}:           true,
		{wagering.Pending, wagering.Failed}:             true,
		{wagering.Pending, wagering.PendingReference}:   true,
		{wagering.PendingReference, wagering.Processed}: true,
		{wagering.PendingReference, wagering.Rejected}:  true,
		{wagering.PendingReference, wagering.Failed}:    true,
	}
	for _, from := range all {
		for _, to := range all {
			if got := wagering.CanTransition(from, to); got != allowed[[2]wagering.Status{from, to}] {
				t.Errorf("CanTransition(%s, %s) = %v", from, to, got)
			}
		}
	}
	for _, s := range []wagering.Status{wagering.Processed, wagering.Rejected, wagering.Failed} {
		if !s.IsTerminal() {
			t.Errorf("%s must be terminal", s)
		}
	}
}

func TestNewExternalStartsPending(t *testing.T) {
	tx := pending(t)
	if tx.Status() != wagering.Pending || tx.IsInternal() || tx.Kind() != wagering.Bet {
		t.Errorf("unexpected transaction %+v", tx.Snapshot())
	}
	if tx.ID().IsZero() || tx.PayloadHash() == "" || tx.ProviderID() != "provider-a" {
		t.Error("external metadata missing")
	}
	if _, ok := tx.ResultBalance(); ok {
		t.Error("pending transaction must not have a result balance")
	}
}

func TestTerminalTransactionsRejectTransitions(t *testing.T) {
	now := t0.Add(time.Second)
	tx := pending(t)
	if err := tx.MarkProcessed(brl(t, "975.00"), domain.TransactionID{}, now); err != nil {
		t.Fatal(err)
	}
	checks := map[string]error{
		"processed again": tx.MarkProcessed(brl(t, "950.00"), domain.TransactionID{}, now),
		"rejected":        tx.MarkRejected(domain.CodeInsufficientFunds, money.Money{}, now),
		"failed":          tx.MarkFailed(domain.CodeInconsistentState, now),
	}
	for name, err := range checks {
		if !errors.Is(err, domain.ErrInvalidTransition) {
			t.Errorf("%s: error = %v, want INVALID_STATE_TRANSITION", name, err)
		}
	}
	if b, _ := tx.ResultBalance(); b.Amount() != "975.00" {
		t.Errorf("terminal result changed to %v", b)
	}
}

func TestOnlyReversalsWaitForReference(t *testing.T) {
	if err := pending(t).MarkPendingReference(t0); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("BET waiting: %v", err)
	}
	if err := pending(t, op("WIN", "w-1", "10.00", "bet-1")).MarkPendingReference(t0); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("WIN waiting: %v", err)
	}
	refund := pending(t, op("REFUND", "r-1", "25.00", "bet-1"))
	if err := refund.MarkPendingReference(t0); err != nil {
		t.Fatal(err)
	}
	if err := refund.MarkPendingReference(t0); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("PENDING_REFERENCE -> PENDING_REFERENCE: %v", err)
	}
}

func TestMarkValidatesCodes(t *testing.T) {
	tx := pending(t)
	if err := tx.MarkRejected(domain.CodeInvalidAmount, money.Money{}, t0); err == nil {
		t.Error("a validation code must not be persisted as a rejection")
	}
	if err := tx.MarkRejected(domain.CodeInconsistentState, money.Money{}, t0); err == nil {
		t.Error("an invariant code must not be persisted as a rejection")
	}
	if err := tx.MarkFailed(domain.CodeInsufficientFunds, t0); err == nil {
		t.Error("a rejection code must not be persisted as a permanent failure")
	}
	if tx.Status() != wagering.Pending {
		t.Error("invalid marks must not change the status")
	}
	if err := tx.MarkFailed(domain.CodeInconsistentState, t0); err != nil || tx.Status() != wagering.Failed {
		t.Errorf("MarkFailed: %v", err)
	}
}

func TestMarkProcessedReferenceRules(t *testing.T) {
	ref, _ := domain.NewID[domain.TransactionID]()
	if err := pending(t).MarkProcessed(brl(t, "1.00"), ref, t0); err == nil {
		t.Error("BET must not carry a resolved reference")
	}
	if err := pending(t, op("REFUND", "r-1", "25.00", "b")).MarkProcessed(brl(t, "1.00"), domain.TransactionID{}, t0); err == nil {
		t.Error("REFUND must carry a resolved reference")
	}
	usd, _ := money.Parse("1.00", "USD")
	if err := pending(t).MarkProcessed(usd, domain.TransactionID{}, t0); err == nil {
		t.Error("result balance in another currency must be rejected")
	}
}

func TestCheckReplay(t *testing.T) {
	tx := pending(t)

	if err := tx.CheckReplay(request(t)); err != nil {
		t.Errorf("same key and payload must be a replay: %v", err)
	}
	diffPayload := request(t, func(in *wagering.RequestInput) { in.Amount = "30.00" })
	if err := tx.CheckReplay(diffPayload); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Errorf("same key, different payload: %v", err)
	}
	otherKey := request(t, func(in *wagering.RequestInput) { in.IdempotencyKey = "other-key" })
	if err := tx.CheckReplay(otherKey); !errors.Is(err, domain.ErrExternalTxConflict) {
		t.Errorf("same operation under another key: %v", err)
	}
	otherProvider := request(t, func(in *wagering.RequestInput) { in.ProviderID = "provider-b" })
	if err := tx.CheckReplay(otherProvider); !errors.Is(err, domain.ErrInconsistentState) {
		t.Errorf("cross-provider lookup must be refused: %v", err)
	}
}

func processedSnapshot(t *testing.T) wagering.Snapshot {
	tx := pending(t)
	if err := tx.MarkProcessed(brl(t, "975.00"), domain.TransactionID{}, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return tx.Snapshot()
}

func TestRehydrateRoundTrip(t *testing.T) {
	s := processedSnapshot(t)
	tx, err := wagering.Rehydrate(s)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Snapshot() != s {
		t.Error("rehydration must reproduce the snapshot exactly")
	}
	if err := tx.MarkRejected(domain.CodeInsufficientFunds, money.Money{}, t0); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Error("rehydrated terminal transaction must stay terminal")
	}
}

func TestRehydrateRejectsInconsistentState(t *testing.T) {
	ref, _ := domain.NewID[domain.TransactionID]()
	mutations := map[string]func(*wagering.Snapshot){
		"unknown kind":          func(s *wagering.Snapshot) { s.Kind = "JACKPOT" },
		"unknown status":        func(s *wagering.Snapshot) { s.Status = "DONE" },
		"missing provider":      func(s *wagering.Snapshot) { s.ProviderID = "" },
		"missing hash":          func(s *wagering.Snapshot) { s.PayloadHash = "" },
		"processed no balance":  func(s *wagering.Snapshot) { s.ResultBalance = money.Money{} },
		"processed with code":   func(s *wagering.Snapshot) { s.FailureCode = domain.CodeInsufficientFunds },
		"rejected without code": func(s *wagering.Snapshot) { s.Status = wagering.Rejected },
		"failed with rejection": func(s *wagering.Snapshot) { s.Status = wagering.Failed; s.FailureCode = domain.CodeInsufficientFunds },
		"bet with reference":    func(s *wagering.Snapshot) { s.ReferenceTransactionID = ref },
		"pending with result":   func(s *wagering.Snapshot) { s.Status = wagering.Pending },
		"bet waiting":           func(s *wagering.Snapshot) { s.Status = wagering.PendingReference; s.ResultBalance = money.Money{} },
		"opening with provider": func(s *wagering.Snapshot) { s.Kind = wagering.Opening },
		"zero amount bet":       func(s *wagering.Snapshot) { s.Money = brl(t, "0.00") },
		"timestamps":            func(s *wagering.Snapshot) { s.UpdatedAt = s.CreatedAt.Add(-time.Second) },
	}
	for name, mutate := range mutations {
		s := processedSnapshot(t)
		mutate(&s)
		if _, err := wagering.Rehydrate(s); !errors.Is(err, domain.ErrInconsistentState) {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}
