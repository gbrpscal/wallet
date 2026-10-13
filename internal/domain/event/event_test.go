package event_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/event"
	"github.com/gbrpscal/wallet/internal/domain/money"
	"github.com/gbrpscal/wallet/internal/domain/wallet"
)

func TestWalletBalanceChangedEnvelope(t *testing.T) {
	walletID := domain.MustParseID[domain.WalletID]("0192f291-27dd-7d3f-8071-5f8685deef37")
	txID := domain.MustParseID[domain.TransactionID]("0192f298-345e-7e38-af88-e43f851a819d")
	entryID, _ := domain.NewID[domain.LedgerEntryID]()
	amount, _ := money.Parse("25.00", "BRL")
	before, _ := money.Parse("1000.00", "BRL")
	after, _ := money.Parse("975.00", "BRL")
	at := time.Date(2026, 9, 8, 9, 0, 0, 0, time.FixedZone("BRT", -3*3600))

	entry, err := wallet.NewLedgerEntry(wallet.LedgerEntrySnapshot{
		ID: entryID, WalletID: walletID, TransactionID: txID, Direction: wallet.Debit,
		Amount: amount, BalanceBefore: before, BalanceAfter: after, WalletVersion: 2, CreatedAt: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := event.NewWalletBalanceChanged(event.Trace{CorrelationID: "corr-1", CausationID: "msg-123"}, at, entry)
	if err != nil {
		t.Fatal(err)
	}
	if e.Type() != event.TypeWalletBalanceChanged || e.Version() != 1 || e.AggregateID() != walletID.String() {
		t.Errorf("header = %s v%d %s", e.Type(), e.Version(), e.AggregateID())
	}

	raw, err := event.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"eventId":       e.ID().String(),
		"eventType":     "WalletBalanceChanged",
		"aggregateId":   walletID.String(),
		"correlationId": "corr-1",
		"causationId":   "msg-123",
		"occurredAt":    "2026-09-08T12:00:00.000000Z",
		"version":       float64(1),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	data := got["data"].(map[string]any)
	if data["direction"] != "DEBIT" || data["walletVersion"] != float64(2) || data["transactionId"] != txID.String() {
		t.Errorf("data = %v", data)
	}
	for field, amount := range map[string]string{"money": "25.00", "balanceBefore": "1000.00", "balanceAfter": "975.00"} {
		m := data[field].(map[string]any)
		if m["amount"] != amount || m["currency"] != "BRL" {
			t.Errorf("%s = %v, want amount as decimal string", field, m)
		}
	}

	again, _ := event.Marshal(e)
	if string(again) != string(raw) {
		t.Error("marshaling the same event must be deterministic (same eventId and payload)")
	}
}

func TestEventRequiresCorrelation(t *testing.T) {
	_, err := event.NewWagerTransactionProcessed(event.Trace{}, time.Now(), event.WagerTransactionProcessedData{})
	if err == nil {
		t.Error("event without correlation id must be rejected")
	}
}

func TestCausationIsOptional(t *testing.T) {
	txID, _ := domain.NewID[domain.TransactionID]()
	walletID, _ := domain.NewID[domain.WalletID]()
	playerID, _ := domain.NewID[domain.PlayerID]()
	m, _ := money.Parse("0.00", "BRL")
	bal, _ := money.Parse("1.00", "BRL")
	e, err := event.NewWagerTransactionProcessed(event.Trace{CorrelationID: "c"}, time.Now(), event.WagerTransactionProcessedData{
		TransactionRef: event.TransactionRef{TransactionID: txID, WalletID: walletID, PlayerID: playerID, Kind: "LOSS", Money: m},
		Balance:        bal,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := event.Marshal(e)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	if _, ok := got["causationId"]; ok {
		t.Error("empty causationId must be omitted")
	}
	data := got["data"].(map[string]any)
	if _, ok := data["providerId"]; ok {
		t.Error("empty external metadata must be omitted")
	}
}
