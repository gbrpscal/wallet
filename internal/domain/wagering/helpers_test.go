package wagering_test

import (
	"testing"
	"time"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/event"
	"github.com/gbrpscal/wallet/internal/domain/money"
	"github.com/gbrpscal/wallet/internal/domain/wagering"
	"github.com/gbrpscal/wallet/internal/domain/wallet"
)

const (
	playerStr = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"
	walletStr = "0192f291-27dd-7d3f-8071-5f8685deef37"
)

var (
	t0       = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	playerID = domain.MustParseID[domain.PlayerID](playerStr)
	walletID = domain.MustParseID[domain.WalletID](walletStr)
	trace    = event.Trace{CorrelationID: "corr-1"}
)

// input devolve a BET de exemplo do enunciado; opts a ajustam.
func input(opts ...func(*wagering.RequestInput)) wagering.RequestInput {
	in := wagering.RequestInput{
		ProviderID:            "provider-a",
		ExternalTransactionID: "transaction-123",
		IdempotencyKey:        "provider-a:transaction-123",
		PlayerID:              playerStr,
		WalletID:              walletStr,
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Kind:                  "BET",
		Amount:                "25.00",
		Currency:              "BRL",
	}
	for _, o := range opts {
		o(&in)
	}
	return in
}

func op(kind, extID, amount, ref string) func(*wagering.RequestInput) {
	return func(in *wagering.RequestInput) {
		in.Kind = kind
		in.ExternalTransactionID = extID
		in.IdempotencyKey = "provider-a:" + extID
		in.Amount = amount
		in.ReferenceExternalTransactionID = ref
	}
}

func request(t *testing.T, opts ...func(*wagering.RequestInput)) wagering.Request {
	t.Helper()
	r, err := wagering.NewRequest(input(opts...))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return r
}

func pending(t *testing.T, opts ...func(*wagering.RequestInput)) *wagering.WagerTransaction {
	t.Helper()
	tx, err := wagering.NewExternal(request(t, opts...), t0)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func brl(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func openWallet(t *testing.T, initial string) *wallet.Wallet {
	t.Helper()
	opening, _ := domain.NewID[domain.TransactionID]()
	w, _, err := wallet.Open(walletID, playerID, brl(t, initial), opening, t0)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// processed executa tx sobre w e exige que termine PROCESSED.
func processed(t *testing.T, tx *wagering.WagerTransaction, w *wallet.Wallet, ref wagering.Reference) wagering.Outcome {
	t.Helper()
	out, err := wagering.Process(tx, w, ref, trace, t0.Add(time.Second))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if tx.Status() != wagering.Processed {
		t.Fatalf("status = %s (%s), want PROCESSED", tx.Status(), tx.FailureCode())
	}
	return out
}

func eventTypes(out wagering.Outcome) []string {
	types := make([]string, 0, len(out.Events))
	for _, e := range out.Events {
		types = append(types, e.Type())
	}
	return types
}
