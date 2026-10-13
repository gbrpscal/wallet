//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gbrpscal/wallet/internal/adapters/postgres"
	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/event"
	"github.com/gbrpscal/wallet/internal/domain/money"
	"github.com/gbrpscal/wallet/internal/domain/wagering"
	"github.com/gbrpscal/wallet/internal/domain/wallet"
)

var trace = event.Trace{CorrelationID: "integration-test"}

func uow() *postgres.UnitOfWork {
	return postgres.NewUnitOfWork(appPool, 2*time.Second)
}

func brl(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func newPlayer(t *testing.T) domain.PlayerID {
	t.Helper()
	id, err := domain.NewID[domain.PlayerID]()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// openWallet abre e persiste uma carteira como o caso de uso fará: carteira,
// OPENING, lançamento e eventos no mesmo commit.
func openWallet(t *testing.T, initial string) wagering.OpenOutcome {
	t.Helper()
	out, err := wagering.OpenWallet(newPlayer(t), brl(t, initial), trace, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	err = uow().Do(context.Background(), func(ctx context.Context, tx app.Tx) error {
		return persistOpening(ctx, tx, out)
	})
	if err != nil {
		t.Fatalf("persist opening: %v", err)
	}
	return out
}

func persistOpening(ctx context.Context, tx app.Tx, out wagering.OpenOutcome) error {
	if err := tx.Wallets().Insert(ctx, out.Wallet); err != nil {
		return err
	}
	if out.Opening != nil {
		if err := tx.Transactions().Insert(ctx, out.Opening, app.TransactionMeta{Trace: trace}); err != nil {
			return err
		}
		if err := tx.Ledger().Append(ctx, *out.Entry); err != nil {
			return err
		}
	}
	return tx.Outbox().Append(ctx, out.Events...)
}

// operation monta uma requisição externa para a carteira.
func operation(t *testing.T, w *wallet.Wallet, provider, extID, kind, amount, ref string) wagering.Request {
	t.Helper()
	req, err := wagering.NewRequest(wagering.RequestInput{
		ProviderID:                     provider,
		ExternalTransactionID:          extID,
		IdempotencyKey:                 provider + ":" + extID,
		PlayerID:                       w.PlayerID().String(),
		WalletID:                       w.ID().String(),
		RoundID:                        "round-1",
		GameID:                         "game-1",
		Kind:                           kind,
		Amount:                         amount,
		Currency:                       w.Currency().String(),
		ReferenceExternalTransactionID: ref,
	})
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// processNew executa uma operação nova sob o lock da carteira e persiste o
// Outcome no mesmo commit: transação, lançamento, saldo e eventos.
func processNew(ctx context.Context, tx app.Tx, req wagering.Request) (wagering.Outcome, error) {
	w, err := tx.Wallets().Lock(ctx, req.WalletID())
	if err != nil {
		return wagering.Outcome{}, err
	}
	expected := w.Version()

	var ref wagering.Reference
	if extRef := req.ReferenceExternalTransactionID(); extRef != "" {
		r, err := tx.Transactions().FindByExternalID(ctx, req.ProviderID(), extRef)
		switch {
		case err == nil:
			ref.Transaction = r
			if ref.Reversed, err = tx.Transactions().HasProcessedReversal(ctx, r.ID()); err != nil {
				return wagering.Outcome{}, err
			}
		case !errors.Is(err, app.ErrNotFound):
			return wagering.Outcome{}, err
		}
	}

	wtx, err := wagering.NewExternal(req, time.Now())
	if err != nil {
		return wagering.Outcome{}, err
	}
	out, err := wagering.Process(wtx, w, ref, trace, time.Now())
	if err != nil {
		return wagering.Outcome{}, err
	}
	meta := app.TransactionMeta{Trace: trace}
	if out.Transaction.Status() == wagering.PendingReference {
		sched := app.DefaultReferencePolicy.Initial(out.Transaction.CreatedAt())
		meta.Schedule = &sched
	}
	if err := tx.Transactions().Insert(ctx, out.Transaction, meta); err != nil {
		return wagering.Outcome{}, err
	}
	if out.Entry != nil {
		if err := tx.Ledger().Append(ctx, *out.Entry); err != nil {
			return wagering.Outcome{}, err
		}
		if err := tx.Wallets().Save(ctx, w, expected); err != nil {
			return wagering.Outcome{}, err
		}
	}
	return out, tx.Outbox().Append(ctx, out.Events...)
}

func mustProcess(t *testing.T, req wagering.Request) wagering.Outcome {
	t.Helper()
	var out wagering.Outcome
	err := uow().Do(context.Background(), func(ctx context.Context, tx app.Tx) error {
		var err error
		out, err = processNew(ctx, tx, req)
		return err
	})
	if err != nil {
		t.Fatalf("process %s %s: %v", req.Kind(), req.ExternalTransactionID(), err)
	}
	return out
}

func getWallet(t *testing.T, id domain.WalletID) *wallet.Wallet {
	t.Helper()
	var w *wallet.Wallet
	err := uow().Do(context.Background(), func(ctx context.Context, tx app.Tx) error {
		var err error
		w, err = tx.Wallets().Get(ctx, id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func ledgerOf(t *testing.T, id domain.WalletID) []wallet.LedgerEntry {
	t.Helper()
	var entries []wallet.LedgerEntry
	err := uow().Do(context.Background(), func(ctx context.Context, tx app.Tx) error {
		var err error
		entries, err = tx.Ledger().List(ctx, id, 0, 1000)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// assertLedgerMatchesBalance confere o saldo armazenado contra créditos −
// débitos do ledger.
func assertLedgerMatchesBalance(t *testing.T, id domain.WalletID) {
	t.Helper()
	w := getWallet(t, id)
	sum, _ := money.Zero(w.Currency())
	for _, e := range ledgerOf(t, id) {
		signed, err := e.SignedAmount()
		if err != nil {
			t.Fatal(err)
		}
		if sum, err = sum.Add(signed); err != nil {
			t.Fatal(err)
		}
	}
	if !sum.Equal(w.Balance()) {
		t.Fatalf("stored balance %v differs from ledger sum %v", w.Balance(), sum)
	}
}
