//go:build integration

package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/gbrpscal/wallet/internal/worker"
)

// TestReferenceWorkerResolvesPendingReversals: um REFUND chega antes da BET;
// com dois loops do worker rodando (como em duas instâncias), a pendência é
// concluída uma única vez quando a BET chega.
func TestReferenceWorkerResolvesPendingReversals(t *testing.T) {
	e := newEnv(t)
	w := e.openWallet(t, "100.00")
	other := e.openWallet(t, "100.00")
	refund := e.submit(t, w, "early-refund", "REFUND", "30.00", "late-bet")
	otherRefund := e.submit(t, other, "early-refund-2", "REFUND", "10.00", "late-bet-2")

	loops := []*worker.Loop{
		worker.NewReferenceLoop(e.svc, 100*time.Millisecond, 50, 8, quiet),
		worker.NewReferenceLoop(e.svc, 100*time.Millisecond, 50, 8, quiet),
	}
	for _, l := range loops {
		if err := l.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, l := range loops {
			_ = l.Stop(ctx)
		}
	}()

	e.submit(t, w, "late-bet", "BET", "30.00", "")
	e.submit(t, other, "late-bet-2", "BET", "10.00", "")

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if e.count(t, `SELECT count(*) FROM wager_transactions WHERE id IN ($1, $2) AND status = 'PROCESSED'`,
			refund.Transaction.ID().String(), otherRefund.Transaction.ID().String()) == 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, id := range []string{refund.Transaction.ID().String(), otherRefund.Transaction.ID().String()} {
		if n := e.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id = $1`, id); n != 1 {
			t.Fatalf("reversal %s has %d ledger entries, want 1", id, n)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM wager_transactions WHERE status = 'PENDING_REFERENCE'`); n != 0 {
		t.Fatalf("%d reversals still pending", n)
	}
}

// TestReferenceWorkerExpiresMissingReferences: sem a referência, o worker
// esgota as tentativas e rejeita com REFERENCE_NOT_FOUND.
func TestReferenceWorkerExpiresMissingReferences(t *testing.T) {
	e := newEnv(t)
	w := e.openWallet(t, "100.00")
	res := e.submit(t, w, "orphan", "ROLLBACK", "5.00", "never")

	loop := worker.NewReferenceLoop(e.svc, 50*time.Millisecond, 50, 8, quiet)
	if err := loop.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = loop.Stop(ctx)
	}()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if e.count(t, `SELECT count(*) FROM wager_transactions WHERE id = $1 AND status = 'REJECTED' AND failure_code = 'REFERENCE_NOT_FOUND'`,
			res.Transaction.ID().String()) == 1 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("pending reversal did not expire")
}
