//go:build integration

package app_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gbrpscal/wallet/internal/adapters/postgres"
	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/wagering"
	"github.com/gbrpscal/wallet/internal/domain/wallet"
)

func TestOpenWallet(t *testing.T) {
	h := newHarness()
	ctx := context.Background()

	player := newPlayerID(t)
	w, err := h.svc.OpenWallet(ctx, app.OpenWalletInput{PlayerID: player, Amount: "1000.00", Currency: "BRL"}, traceFor("open-1"))
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "1000.00" || w.Version() != 1 {
		t.Fatalf("wallet = %v v%d", w.Balance(), w.Version())
	}
	if n := ledgerCount(t, w.ID().String()); n != 1 {
		t.Fatalf("ledger entries = %d, want 1 (opening credit)", n)
	}
	if n := countRows(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND kind = 'OPENING' AND status = 'PROCESSED'`, w.ID().String()); n != 1 {
		t.Fatalf("opening transactions = %d", n)
	}
	if n := countRows(t, `SELECT count(*) FROM outbox_events o JOIN wager_transactions t ON o.aggregate_id IN (t.id, t.wallet_id) WHERE t.wallet_id = $1 AND t.kind = 'OPENING'`, w.ID().String()); n != 2 {
		t.Fatalf("opening events = %d, want WagerTransactionProcessed and WalletBalanceChanged", n)
	}

	_, err = h.svc.OpenWallet(ctx, app.OpenWalletInput{PlayerID: player, Amount: "5.00", Currency: "BRL"}, traceFor("open-2"))
	if !errors.Is(err, domain.ErrWalletAlreadyExists) {
		t.Errorf("second wallet: %v, want WALLET_ALREADY_EXISTS", err)
	}

	zero, err := h.svc.OpenWallet(ctx, app.OpenWalletInput{PlayerID: newPlayerID(t), Amount: "0.00", Currency: "BRL"}, traceFor("open-3"))
	if err != nil {
		t.Fatal(err)
	}
	if ledgerCount(t, zero.ID().String()) != 0 || countRows(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id = $1`, zero.ID().String()) != 0 {
		t.Error("zero opening must not create OPENING nor ledger entry")
	}

	for name, in := range map[string]app.OpenWalletInput{
		"bad player":   {PlayerID: "player-1", Amount: "1.00", Currency: "BRL"},
		"negative":     {PlayerID: newPlayerID(t), Amount: "-1.00", Currency: "BRL"},
		"bad currency": {PlayerID: newPlayerID(t), Amount: "1.00", Currency: "JPY"},
	} {
		if _, err := h.svc.OpenWallet(ctx, in, traceFor("bad")); err == nil {
			t.Errorf("%s accepted", name)
		} else if k, _ := domain.KindOf(err); k != domain.KindValidation {
			t.Errorf("%s: kind = %v", name, k)
		}
	}
}

// TestReplayReturnsOriginalResult: o replay devolve o saldo observado no
// processamento original, mesmo depois de a carteira ter mudado.
func TestReplayReturnsOriginalResult(t *testing.T) {
	h := newHarness()
	walletID := h.open(t, "1000.00")
	bet := h.request(t, walletID, "transaction-123", "BET", "25.00", "")

	first := h.submit(t, bet)
	if first.Replay || first.Transaction.Status() != wagering.Processed || resultBalance(t, first) != "975.00" {
		t.Fatalf("first = replay %v, %s, %s", first.Replay, first.Transaction.Status(), resultBalance(t, first))
	}
	h.submit(t, h.request(t, walletID, "transaction-124", "BET", "100.00", ""))

	again := h.submit(t, bet)
	if !again.Replay || again.Transaction.ID() != first.Transaction.ID() || resultBalance(t, again) != "975.00" {
		t.Fatalf("replay = %v, id %s, balance %s; want the original 975.00", again.Replay, again.Transaction.ID(), resultBalance(t, again))
	}
	if h.balance(t, walletID) != "875.00" || ledgerCount(t, walletID) != 3 {
		t.Fatalf("balance %s, entries %d", h.balance(t, walletID), ledgerCount(t, walletID))
	}
	h.assertReconciled(t, walletID)
}

func TestIdempotencyConflicts(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	walletID := h.open(t, "1000.00")
	bet := h.request(t, walletID, "conflict-1", "BET", "25.00", "")
	h.submit(t, bet)

	changed := bet
	changed.Amount = "30.00"
	if _, err := h.svc.SubmitOperation(ctx, changed, traceFor("x")); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Errorf("same key, different payload: %v", err)
	}
	otherKey := bet
	otherKey.IdempotencyKey = "another-key"
	if _, err := h.svc.SubmitOperation(ctx, otherKey, traceFor("x")); !errors.Is(err, domain.ErrExternalTxConflict) {
		t.Errorf("same operation, another key: %v", err)
	}
	if h.balance(t, walletID) != "975.00" || ledgerCount(t, walletID) != 2 {
		t.Fatalf("conflicts changed the wallet: %s", h.balance(t, walletID))
	}
}

// TestKeyNotConsumedByCorrectableErrors: erro de entrada e carteira
// inexistente não persistem nada; a mesma chave continua utilizável.
func TestKeyNotConsumedByCorrectableErrors(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	walletID := h.open(t, "100.00")
	valid := h.request(t, walletID, "retry-key", "BET", "10.00", "")

	unknown := valid
	unknown.WalletID = "0192f291-27dd-7d3f-8071-000000000000"
	if _, err := h.svc.SubmitOperation(ctx, unknown, traceFor("x")); !errors.Is(err, domain.ErrWalletNotFound) {
		t.Fatalf("unknown wallet: %v, want WALLET_NOT_FOUND", err)
	}
	zero := valid
	zero.Amount = "0.00"
	if _, err := h.svc.SubmitOperation(ctx, zero, traceFor("x")); !errors.Is(err, domain.ErrAmountMustBePositive) {
		t.Fatalf("zero bet: %v", err)
	}
	opening := valid
	opening.Kind = "OPENING"
	if _, err := h.svc.SubmitOperation(ctx, opening, traceFor("x")); !errors.Is(err, domain.ErrKindNotAllowed) {
		t.Fatalf("external OPENING: %v", err)
	}
	if n := countRows(t, `SELECT count(*) FROM wager_transactions WHERE idempotency_key = 'provider-a:retry-key'`); n != 0 {
		t.Fatalf("rejected input persisted %d rows", n)
	}

	res := h.submit(t, valid)
	if res.Replay || res.Transaction.Status() != wagering.Processed {
		t.Fatalf("valid request after errors = %+v", res)
	}
}

func TestRejectionIsPersistedAndReplayed(t *testing.T) {
	h := newHarness()
	walletID := h.open(t, "100.00")
	bet := h.request(t, walletID, "too-big", "BET", "200.00", "")

	first := h.submit(t, bet)
	if first.Transaction.Status() != wagering.Rejected || first.Transaction.FailureCode() != domain.CodeInsufficientFunds {
		t.Fatalf("first = %s %s", first.Transaction.Status(), first.Transaction.FailureCode())
	}
	again := h.submit(t, bet)
	if !again.Replay || again.Transaction.ID() != first.Transaction.ID() || again.Transaction.FailureCode() != domain.CodeInsufficientFunds {
		t.Fatalf("replay of rejection = %+v", again)
	}
	if h.balance(t, walletID) != "100.00" || ledgerCount(t, walletID) != 1 {
		t.Fatal("rejection moved money")
	}
}

func TestLossHasNoMovement(t *testing.T) {
	h := newHarness()
	walletID := h.open(t, "100.00")
	res := h.submit(t, h.request(t, walletID, "loss-1", "LOSS", "0.00", ""))
	if res.Transaction.Status() != wagering.Processed {
		t.Fatalf("status = %s", res.Transaction.Status())
	}
	w, _ := h.svc.GetWallet(context.Background(), domain.MustParseID[domain.WalletID](walletID))
	if w.Version() != 1 || ledgerCount(t, walletID) != 1 {
		t.Fatalf("LOSS changed version (%d) or ledger", w.Version())
	}
	types := countRows(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, res.Transaction.ID().String())
	balanceEvents := countRows(t, `SELECT count(*) FROM outbox_events WHERE event_type = 'WalletBalanceChanged' AND payload::jsonb->'data'->>'transactionId' = $1`, res.Transaction.ID().String())
	if types != 1 || balanceEvents != 0 {
		t.Fatalf("LOSS events: %d processed, %d balance changes", types, balanceEvents)
	}
}

// TestFiftyIdenticalBetsInParallel: a mesma aposta 50 vezes em paralelo gera
// um único débito; as demais são replays do mesmo resultado.
func TestFiftyIdenticalBetsInParallel(t *testing.T) {
	h := newHarness()
	walletID := h.open(t, "1000.00")
	bet := h.request(t, walletID, "same-bet", "BET", "25.00", "")

	const n = 50
	start := make(chan struct{})
	results := make(chan app.OperationResult, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := h.svc.SubmitOperation(context.Background(), bet, traceFor("same-bet"))
			if err != nil {
				errs <- err
				return
			}
			results <- res
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("parallel submit failed: %v", err)
	}

	ids := map[domain.TransactionID]bool{}
	fresh := 0
	for r := range results {
		ids[r.Transaction.ID()] = true
		if !r.Replay {
			fresh++
		}
		if resultBalance(t, r) != "975.00" {
			t.Errorf("result balance = %s", resultBalance(t, r))
		}
	}
	if len(ids) != 1 || fresh != 1 {
		t.Fatalf("distinct transactions = %d, non-replays = %d; want 1 and 1", len(ids), fresh)
	}
	if h.balance(t, walletID) != "975.00" || ledgerCount(t, walletID) != 2 {
		t.Fatalf("balance %s, entries %d", h.balance(t, walletID), ledgerCount(t, walletID))
	}
	h.assertReconciled(t, walletID)
}

// TestTwoBetsOf80OnWalletWith100: uma processada, outra rejeitada por saldo,
// saldo final 20.00, um único débito; reenvios não mudam o resultado.
func TestTwoBetsOf80OnWalletWith100(t *testing.T) {
	h := newHarness()
	walletID := h.open(t, "100.00")
	bets := []wagering.RequestInput{
		h.request(t, walletID, "b80-a", "BET", "80.00", ""),
		h.request(t, walletID, "b80-b", "BET", "80.00", ""),
	}
	run := func() map[wagering.Status]int {
		statuses := map[wagering.Status]int{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, b := range bets {
			wg.Add(1)
			go func(b wagering.RequestInput) {
				defer wg.Done()
				res, err := h.svc.SubmitOperation(context.Background(), b, traceFor(b.ExternalTransactionID))
				if err != nil {
					t.Errorf("submit: %v", err)
					return
				}
				mu.Lock()
				statuses[res.Transaction.Status()]++
				mu.Unlock()
				if res.Transaction.Status() == wagering.Rejected && res.Transaction.FailureCode() != domain.CodeInsufficientFunds {
					t.Errorf("code = %s", res.Transaction.FailureCode())
				}
			}(b)
		}
		wg.Wait()
		return statuses
	}
	for round := 0; round < 3; round++ {
		if s := run(); s[wagering.Processed] != 1 || s[wagering.Rejected] != 1 {
			t.Fatalf("round %d statuses = %v", round, s)
		}
	}
	debits := countRows(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, walletID)
	if h.balance(t, walletID) != "20.00" || debits != 1 {
		t.Fatalf("balance %s, debits %d", h.balance(t, walletID), debits)
	}
	h.assertReconciled(t, walletID)
}

func TestIndependentWalletsInParallel(t *testing.T) {
	h := newHarness()
	const wallets, bets = 5, 10
	ids := make([]string, wallets)
	for i := range ids {
		ids[i] = h.open(t, "100.00")
	}
	var wg sync.WaitGroup
	for i, id := range ids {
		for j := 0; j < bets; j++ {
			wg.Add(1)
			go func(i, j int, id string) {
				defer wg.Done()
				in := h.request(t, id, fmt.Sprintf("multi-%d-%d", i, j), "BET", "1.00", "")
				if _, err := h.svc.SubmitOperation(context.Background(), in, traceFor("multi")); err != nil {
					t.Errorf("submit: %v", err)
				}
			}(i, j, id)
		}
	}
	wg.Wait()
	for _, id := range ids {
		if h.balance(t, id) != "90.00" {
			t.Errorf("wallet %s balance = %s", id, h.balance(t, id))
		}
		h.assertReconciled(t, id)
	}
}

// TestReversalBeforeReferenceResolvedByWorker: REFUND antes da BET fica em
// PENDING_REFERENCE e é concluído pelo worker quando a BET chega.
func TestReversalBeforeReferenceResolvedByWorker(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	walletID := h.open(t, "100.00")

	refund := h.request(t, walletID, "early-refund", "REFUND", "30.00", "late-bet")
	pending := h.submit(t, refund)
	if pending.Transaction.Status() != wagering.PendingReference {
		t.Fatalf("status = %s", pending.Transaction.Status())
	}
	id := pending.Transaction.ID()
	if n := countRows(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'WagerTransactionPendingReference'`, id.String()); n != 1 {
		t.Fatalf("pending events = %d", n)
	}
	if replay := h.submit(t, refund); !replay.Replay || replay.Transaction.Status() != wagering.PendingReference {
		t.Fatalf("replay while pending = %+v", replay)
	}

	// Ainda não venceu: nada a fazer, nem listando nem tentando resolver.
	if due, _ := h.svc.DuePendingReferences(ctx, 100); containsTx(due, id) {
		t.Fatal("pending reference due before its schedule")
	}
	if st, err := h.svc.ResolvePendingReference(ctx, id); err != nil || st != app.ResolveSkipped {
		t.Fatalf("resolve before schedule = %s, %v, want skipped", st, err)
	}
	h.clock.Advance(10 * time.Second)
	if due, _ := h.svc.DuePendingReferences(ctx, 100); !containsTx(due, id) {
		t.Fatal("pending reference not due after its schedule")
	}
	if st, err := h.svc.ResolvePendingReference(ctx, id); err != nil || st != app.ResolveWaiting {
		t.Fatalf("resolve without reference = %s, %v", st, err)
	}

	h.submit(t, h.request(t, walletID, "late-bet", "BET", "30.00", ""))
	h.clock.Advance(10 * time.Second)
	if st, err := h.svc.ResolvePendingReference(ctx, id); err != nil || st != app.ResolveCompleted {
		t.Fatalf("resolve with reference = %s, %v", st, err)
	}
	if st, _ := h.svc.ResolvePendingReference(ctx, id); st != app.ResolveSkipped {
		t.Fatalf("second resolve = %s, want skipped", st)
	}

	done := h.submit(t, refund)
	if !done.Replay || done.Transaction.Status() != wagering.Processed || resultBalance(t, done) != "100.00" {
		t.Fatalf("refund after resolution = %s", done.Transaction.Status())
	}
	if h.balance(t, walletID) != "100.00" {
		t.Fatalf("balance = %s", h.balance(t, walletID))
	}
	// Os eventos do worker carregam o correlationId da requisição original.
	var corr string
	_ = appPool.QueryRow(ctx, `SELECT payload::jsonb->>'correlationId' FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'WagerTransactionProcessed'`, id.String()).Scan(&corr)
	if corr != "corr-early-refund" {
		t.Fatalf("worker event correlationId = %q, want the original request's", corr)
	}
	h.assertReconciled(t, walletID)
}

func containsTx(due []app.PendingReference, id domain.TransactionID) bool {
	for _, d := range due {
		if d.TransactionID == id {
			return true
		}
	}
	return false
}

// TestPendingReferenceExpiresAfterMaxAttempts: esgotadas as tentativas, a
// reversão é rejeitada com REFERENCE_NOT_FOUND e evento de rejeição.
func TestPendingReferenceExpiresAfterMaxAttempts(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	walletID := h.open(t, "100.00")
	res := h.submit(t, h.request(t, walletID, "orphan-rollback", "ROLLBACK", "10.00", "never-arrives"))
	id := res.Transaction.ID()

	var statuses []app.ResolveStatus
	for i := 0; i < testPolicy.MaxAttempts; i++ {
		h.clock.Advance(10 * time.Second)
		st, err := h.svc.ResolvePendingReference(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, st)
	}
	want := []app.ResolveStatus{app.ResolveWaiting, app.ResolveWaiting, app.ResolveExpired}
	if fmt.Sprint(statuses) != fmt.Sprint(want) {
		t.Fatalf("statuses = %v, want %v", statuses, want)
	}
	tx, _ := h.svc.GetTransaction(ctx, id)
	if tx.Status() != wagering.Rejected || tx.FailureCode() != domain.CodeReferenceNotFound {
		t.Fatalf("expired = %s %s", tx.Status(), tx.FailureCode())
	}
	if n := countRows(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'WagerTransactionRejected'`, id.String()); n != 1 {
		t.Fatalf("rejection events = %d", n)
	}
	if !strings.Contains(h.logs.String(), "pending reference expired") {
		t.Error("expiration not logged")
	}
}

func TestPendingReferenceExpiresAfterTTL(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	walletID := h.open(t, "100.00")
	res := h.submit(t, h.request(t, walletID, "ttl-refund", "REFUND", "10.00", "missing"))

	h.clock.Advance(testPolicy.TTL + time.Second)
	if st, err := h.svc.ResolvePendingReference(ctx, res.Transaction.ID()); err != nil || st != app.ResolveExpired {
		t.Fatalf("resolve after TTL = %s, %v", st, err)
	}
}

// TestConcurrentResolversProcessOnce: várias chamadas simultâneas para a
// mesma pendência resultam em uma única conclusão.
func TestConcurrentResolversProcessOnce(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	walletID := h.open(t, "100.00")
	res := h.submit(t, h.request(t, walletID, "race-refund", "REFUND", "20.00", "race-bet"))
	h.submit(t, h.request(t, walletID, "race-bet", "BET", "20.00", ""))
	h.clock.Advance(10 * time.Second)

	const n = 8
	var wg sync.WaitGroup
	statuses := make(chan app.ResolveStatus, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := h.svc.ResolvePendingReference(ctx, res.Transaction.ID())
			if err != nil {
				t.Errorf("resolve: %v", err)
			}
			statuses <- st
		}()
	}
	wg.Wait()
	close(statuses)
	completed := 0
	for st := range statuses {
		if st == app.ResolveCompleted {
			completed++
		}
	}
	credits := countRows(t, `SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id = $1`, res.Transaction.ID().String())
	if completed != 1 || credits != 1 || h.balance(t, walletID) != "100.00" {
		t.Fatalf("completed %d, credits %d, balance %s", completed, credits, h.balance(t, walletID))
	}
}

func TestRefundAndRollbackOfTheSameBet(t *testing.T) {
	h := newHarness()
	walletID := h.open(t, "100.00")
	h.submit(t, h.request(t, walletID, "rr-bet", "BET", "40.00", ""))
	refund := h.submit(t, h.request(t, walletID, "rr-refund", "REFUND", "40.00", "rr-bet"))
	rollback := h.submit(t, h.request(t, walletID, "rr-rollback", "ROLLBACK", "40.00", "rr-bet"))

	if refund.Transaction.Status() != wagering.Processed {
		t.Fatalf("refund = %s", refund.Transaction.Status())
	}
	if rollback.Transaction.Status() != wagering.Rejected || rollback.Transaction.FailureCode() != domain.CodeAlreadyReversed {
		t.Fatalf("rollback = %s %s, want ALREADY_REVERSED", rollback.Transaction.Status(), rollback.Transaction.FailureCode())
	}
	if h.balance(t, walletID) != "100.00" {
		t.Fatalf("balance = %s, the bet was returned twice", h.balance(t, walletID))
	}
	h.assertReconciled(t, walletID)
}

func TestRollbackOfWinWithoutFunds(t *testing.T) {
	h := newHarness()
	walletID := h.open(t, "0.00")
	h.submit(t, h.request(t, walletID, "rw-win", "WIN", "50.00", ""))
	h.submit(t, h.request(t, walletID, "rw-bet", "BET", "45.00", ""))
	res := h.submit(t, h.request(t, walletID, "rw-rollback", "ROLLBACK", "50.00", "rw-win"))
	if res.Transaction.Status() != wagering.Rejected || res.Transaction.FailureCode() != domain.CodeReversalInsufficient {
		t.Fatalf("rollback = %s %s", res.Transaction.Status(), res.Transaction.FailureCode())
	}
	if h.balance(t, walletID) != "5.00" {
		t.Fatalf("balance = %s", h.balance(t, walletID))
	}
}

func TestReconciliation(t *testing.T) {
	h, reg := newMetricsHarness()
	ctx := context.Background()
	walletID := h.open(t, "1000.00")
	h.submit(t, h.request(t, walletID, "rec-bet", "BET", "25.00", ""))
	id := domain.MustParseID[domain.WalletID](walletID)

	r, err := h.svc.Reconcile(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Consistent || r.Stored.Amount() != "975.00" || r.Calculated.Amount() != "975.00" ||
		r.Difference.Amount() != "0.00" || r.CheckedEntries != 2 {
		t.Fatalf("reconciliation = %+v", r)
	}

	// Simula corrupção: o dono do schema desliga as proteções e altera o saldo.
	corrupt := func(sql string) {
		if _, err := ownerPool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	corrupt(`ALTER TABLE wallets DISABLE TRIGGER USER`)
	corrupt(`UPDATE wallets SET balance_minor = 99000 WHERE id = '` + walletID + `'`)
	corrupt(`ALTER TABLE wallets ENABLE TRIGGER USER`)

	r, err = h.svc.Reconcile(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.Consistent || r.Difference.Amount() != "15.00" || r.Calculated.Amount() != "975.00" {
		t.Fatalf("divergence not reported: %+v", r)
	}
	if !strings.Contains(h.logs.String(), "reconciliation divergence") {
		t.Error("divergence not logged")
	}
	assertMetric(t, reg, "wallet_reconciliations_total", map[string]string{"result": "consistent"}, 1)
	assertMetric(t, reg, "wallet_reconciliations_total", map[string]string{"result": "divergent"}, 1)
	w, _ := h.svc.GetWallet(ctx, id)
	if w.Balance().Amount() != "990.00" {
		t.Fatal("reconciliation must not change the balance")
	}

	if _, err := h.svc.Reconcile(ctx, domain.MustParseID[domain.WalletID]("0192f291-27dd-7d3f-8071-000000000001")); !errors.Is(err, domain.ErrWalletNotFound) {
		t.Errorf("unknown wallet: %v", err)
	}
}

func TestLedgerPagination(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	walletID := h.open(t, "100.00")
	for i := 0; i < 4; i++ {
		h.submit(t, h.request(t, walletID, fmt.Sprintf("page-%d", i), "BET", "1.00", ""))
	}
	id := domain.MustParseID[domain.WalletID](walletID)

	var versions []int64
	cursor := ""
	pages := 0
	for {
		page, err := h.svc.ListLedger(ctx, id, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, e := range page.Entries {
			versions = append(versions, e.WalletVersion())
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if fmt.Sprint(versions) != "[1 2 3 4 5]" || pages != 3 {
		t.Fatalf("versions = %v in %d pages", versions, pages)
	}

	if page, err := h.svc.ListLedger(ctx, id, "", 0); err != nil || len(page.Entries) != 5 || page.NextCursor != "" {
		t.Errorf("default page = %d entries, %v", len(page.Entries), err)
	}
	for _, limit := range []int{-1, app.MaxPageSize + 1} {
		if _, err := h.svc.ListLedger(ctx, id, "", limit); !errors.Is(err, domain.ErrInvalidRequest) {
			t.Errorf("limit %d: %v", limit, err)
		}
	}
	if _, err := h.svc.ListLedger(ctx, id, "garbage!", 10); !errors.Is(err, domain.ErrInvalidRequest) {
		t.Errorf("bad cursor: %v", err)
	}
	if _, err := h.svc.ListLedger(ctx, domain.MustParseID[domain.WalletID]("0192f291-27dd-7d3f-8071-000000000002"), "", 10); !errors.Is(err, domain.ErrWalletNotFound) {
		t.Errorf("unknown wallet: %v", err)
	}
}

func TestTransactionLookupsAreProviderScoped(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	walletID := h.open(t, "100.00")
	res := h.submit(t, h.request(t, walletID, "scoped-1", "BET", "1.00", ""))

	if got, err := h.svc.GetTransactionByExternalID(ctx, "provider-a", "scoped-1"); err != nil || got.ID() != res.Transaction.ID() {
		t.Fatalf("own provider lookup: %v", err)
	}
	if _, err := h.svc.GetTransactionByExternalID(ctx, "provider-b", "scoped-1"); !errors.Is(err, domain.ErrTransactionNotFound) {
		t.Fatalf("other provider lookup: %v, want not found", err)
	}
	if got, err := h.svc.GetTransaction(ctx, res.Transaction.ID()); err != nil || got.ProviderID() != "provider-a" {
		t.Fatalf("lookup by id: %v", err)
	}
}

// TestSameKeyFromAnotherProviderIsIndependent: isolamento entre provedores
// também no replay.
func TestSameKeyFromAnotherProviderIsIndependent(t *testing.T) {
	h := newHarness()
	walletID := h.open(t, "100.00")
	a := h.request(t, walletID, "shared-ext", "BET", "10.00", "")
	b := a
	b.ProviderID = "provider-b"
	b.IdempotencyKey = a.IdempotencyKey

	ra := h.submit(t, a)
	rb := h.submit(t, b)
	if rb.Replay || ra.Transaction.ID() == rb.Transaction.ID() {
		t.Fatal("provider-b replayed provider-a's operation")
	}
	if h.balance(t, walletID) != "80.00" {
		t.Fatalf("balance = %s", h.balance(t, walletID))
	}
}

// inconsistentWalletUoW delega à unidade de trabalho real, mas a leitura com
// lock de uma carteira específica devolve dado persistido inconsistente (como
// uma linha corrompida que o domínio recusa reidratar).
type inconsistentWalletUoW struct {
	app.UnitOfWork
	walletID domain.WalletID
}

func (u inconsistentWalletUoW) Do(ctx context.Context, fn func(ctx context.Context, tx app.Tx) error) error {
	return u.UnitOfWork.Do(ctx, func(ctx context.Context, tx app.Tx) error {
		return fn(ctx, inconsistentTx{Tx: tx, walletID: u.walletID})
	})
}

type inconsistentTx struct {
	app.Tx
	walletID domain.WalletID
}

func (t inconsistentTx) Wallets() app.WalletRepository {
	return inconsistentWallets{WalletRepository: t.Tx.Wallets(), walletID: t.walletID}
}

type inconsistentWallets struct {
	app.WalletRepository
	walletID domain.WalletID
}

func (w inconsistentWallets) Lock(ctx context.Context, id domain.WalletID) (*wallet.Wallet, error) {
	if id == w.walletID {
		return nil, domain.Errorf(domain.ErrInconsistentState, "wallet %s has invalid timestamps", id)
	}
	return w.WalletRepository.Lock(ctx, id)
}

// TestPendingReferenceWithInconsistentDataFails: uma pendência cuja resolução
// encontra dado persistido inconsistente é registrada como FAILED
// (INCONSISTENT_STATE), sem movimentação nem evento, e não é retentada.
func TestPendingReferenceWithInconsistentDataFails(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	walletID := h.open(t, "100.00")
	pending := h.submit(t, h.request(t, walletID, "broken-refund", "REFUND", "10.00", "broken-bet"))
	h.submit(t, h.request(t, walletID, "broken-bet", "BET", "10.00", ""))
	entriesBefore := ledgerCount(t, walletID)
	h.clock.Advance(10 * time.Second)

	broken := app.NewService(inconsistentWalletUoW{UnitOfWork: postgres.NewUnitOfWork(appPool, 5*time.Second, app.NopMetrics{}),
		walletID: domain.MustParseID[domain.WalletID](walletID)}, h.clock, app.RetryPolicy{Retries: 1, BaseDelay: time.Millisecond},
		testPolicy, app.NopMetrics{}, slog.New(slog.NewJSONHandler(h.logs, nil)))
	st, err := broken.ResolvePendingReference(ctx, pending.Transaction.ID())
	if err != nil || st != app.ResolveFailed {
		t.Fatalf("resolve = %s, %v; want failed", st, err)
	}

	tx, err := h.svc.GetTransaction(ctx, pending.Transaction.ID())
	if err != nil {
		t.Fatal(err)
	}
	if tx.Status() != wagering.Failed || tx.FailureCode() != domain.CodeInconsistentState {
		t.Fatalf("transaction = %s %s, want FAILED INCONSISTENT_STATE", tx.Status(), tx.FailureCode())
	}
	if n := ledgerCount(t, walletID); n != entriesBefore {
		t.Fatalf("ledger entries %d -> %d; a failed transaction must not move money", entriesBefore, n)
	}
	if n := countRows(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type <> 'WagerTransactionPendingReference'`,
		pending.Transaction.ID().String()); n != 0 {
		t.Fatalf("%d events for a failed transaction", n)
	}
	due, err := h.svc.DuePendingReferences(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.TransactionID == pending.Transaction.ID() {
			t.Fatal("failed transaction is still scheduled for retries")
		}
	}
	if !strings.Contains(h.logs.String(), "pending reference failed permanently") {
		t.Error("permanent failure not logged")
	}
	// Uma nova tentativa encontra a transação terminal e não faz nada.
	if st, err := h.svc.ResolvePendingReference(ctx, pending.Transaction.ID()); err != nil || st != app.ResolveSkipped {
		t.Fatalf("resolve after failure = %s, %v; want skipped", st, err)
	}
}
