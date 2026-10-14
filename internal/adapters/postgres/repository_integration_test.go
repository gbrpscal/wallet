//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/gbrpscal/wallet/internal/adapters/postgres"
	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/event"
	"github.com/gbrpscal/wallet/internal/domain/money"
	"github.com/gbrpscal/wallet/internal/domain/wagering"
	"github.com/gbrpscal/wallet/internal/domain/wallet"
	"github.com/gbrpscal/wallet/internal/platform/metrics"
	"github.com/gbrpscal/wallet/internal/platform/metrics/metricstest"
)

func TestOpeningRoundTrip(t *testing.T) {
	ctx := context.Background()
	out := openWallet(t, "1000.00")

	w := getWallet(t, out.Wallet.ID())
	if w.Balance().Amount() != "1000.00" || w.Version() != 1 || w.PlayerID() != out.Wallet.PlayerID() ||
		!w.CreatedAt().Equal(out.Wallet.CreatedAt()) {
		t.Fatalf("wallet = %v v%d", w.Balance(), w.Version())
	}
	entries := ledgerOf(t, w.ID())
	if len(entries) != 1 || entries[0] != *out.Entry {
		t.Fatalf("ledger = %+v, want the opening entry", entries)
	}

	var opening *wagering.WagerTransaction
	_ = uow().Do(ctx, func(ctx context.Context, tx app.Tx) error {
		var err error
		opening, err = tx.Transactions().Get(ctx, out.Opening.ID())
		return err
	})
	if opening == nil || opening.Snapshot() != out.Opening.Snapshot() {
		t.Fatalf("opening transaction did not round trip")
	}

	// Payload da outbox é exatamente o snapshot gerado.
	for _, e := range out.Events {
		want, _ := event.Marshal(e)
		var got string
		if err := appPool.QueryRow(ctx, `SELECT payload::text FROM outbox_events WHERE event_id = $1`, e.ID().String()).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("outbox payload changed:\n got %s\nwant %s", got, want)
		}
	}
}

func TestZeroOpeningHasNoLedgerNorEvents(t *testing.T) {
	ctx := context.Background()
	out := openWallet(t, "0.00")
	if n := len(ledgerOf(t, out.Wallet.ID())); n != 0 {
		t.Fatalf("ledger has %d entries", n)
	}
	var events int
	_ = appPool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, out.Wallet.ID().String()).Scan(&events)
	if events != 0 {
		t.Fatalf("zero opening produced %d events", events)
	}
}

func TestOneWalletPerPlayerAndCurrency(t *testing.T) {
	ctx := context.Background()
	first := openWallet(t, "10.00")

	again, err := wagering.OpenWallet(first.Wallet.PlayerID(), brl(t, "5.00"), trace, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	err = uow().Do(ctx, func(ctx context.Context, tx app.Tx) error { return persistOpening(ctx, tx, again) })
	if !errors.Is(err, domain.ErrWalletAlreadyExists) {
		t.Fatalf("second wallet: error = %v, want WALLET_ALREADY_EXISTS", err)
	}

	usd, _ := money.Parse("5.00", "USD")
	other, _ := wagering.OpenWallet(first.Wallet.PlayerID(), usd, trace, time.Now())
	if err := uow().Do(ctx, func(ctx context.Context, tx app.Tx) error { return persistOpening(ctx, tx, other) }); err != nil {
		t.Fatalf("wallet in another currency rejected: %v", err)
	}
}

func TestBetRoundTripAndLookups(t *testing.T) {
	ctx := context.Background()
	out := openWallet(t, "1000.00")
	req := operation(t, out.Wallet, "provider-a", "bet-rt", "BET", "25.00", "")
	bet := mustProcess(t, req)

	w := getWallet(t, out.Wallet.ID())
	if w.Balance().Amount() != "975.00" || w.Version() != 2 {
		t.Fatalf("wallet = %v v%d", w.Balance(), w.Version())
	}
	_ = uow().Do(ctx, func(ctx context.Context, tx app.Tx) error {
		byKey, err := tx.Transactions().FindByIdempotencyKey(ctx, "provider-a", "provider-a:bet-rt")
		if err != nil || byKey.Snapshot() != bet.Transaction.Snapshot() {
			t.Errorf("FindByIdempotencyKey = %v", err)
		}
		byExt, err := tx.Transactions().FindByExternalID(ctx, "provider-a", "bet-rt")
		if err != nil || byExt.ID() != bet.Transaction.ID() {
			t.Errorf("FindByExternalID = %v", err)
		}
		// Isolamento por provedor: outro provedor não enxerga a operação.
		if _, err := tx.Transactions().FindByIdempotencyKey(ctx, "provider-b", "provider-a:bet-rt"); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("cross-provider lookup = %v, want not found", err)
		}
		if _, err := tx.Transactions().FindByExternalID(ctx, "provider-b", "bet-rt"); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("cross-provider external lookup = %v, want not found", err)
		}
		return nil
	})
	if b, _ := bet.Transaction.ResultBalance(); b.Amount() != "975.00" {
		t.Errorf("result balance = %v", b)
	}
	assertLedgerMatchesBalance(t, w.ID())
}

func TestIdempotencyConstraints(t *testing.T) {
	ctx := context.Background()
	out := openWallet(t, "100.00")
	mustProcess(t, operation(t, out.Wallet, "provider-a", "idem-1", "BET", "1.00", ""))

	reg := prometheus.NewRegistry()
	counted := postgres.NewUnitOfWork(appPool, 2*time.Second, metrics.New(reg))
	insert := func(req wagering.Request) error {
		return counted.Do(ctx, func(ctx context.Context, tx app.Tx) error {
			wtx, err := wagering.NewExternal(req, time.Now())
			if err != nil {
				return err
			}
			return tx.Transactions().Insert(ctx, wtx, app.TransactionMeta{Trace: trace})
		})
	}
	sameKey := operation(t, out.Wallet, "provider-a", "idem-1", "BET", "2.00", "")
	var dup *app.DuplicateError
	if err := insert(sameKey); !errors.As(err, &dup) || dup.Constraint != "wager_transactions_provider_key_uq" {
		t.Errorf("same provider and key: %v", err)
	}

	otherKey, _ := wagering.NewRequest(wagering.RequestInput{
		ProviderID: "provider-a", ExternalTransactionID: "idem-1", IdempotencyKey: "another-key",
		PlayerID: out.Wallet.PlayerID().String(), WalletID: out.Wallet.ID().String(),
		RoundID: "round-1", GameID: "game-1", Kind: "BET", Amount: "1.00", Currency: "BRL",
	})
	if err := insert(otherKey); !errors.As(err, &dup) || dup.Constraint != "wager_transactions_provider_external_uq" {
		t.Errorf("same operation with another key: %v", err)
	}
	if n := metricstest.Value(t, reg, "wallet_concurrency_conflicts_total", map[string]string{"kind": "unique"}); n != 2 {
		t.Errorf("unique conflicts = %v, want 2", n)
	}

	// Outro provedor pode usar a mesma chave e o mesmo id externo.
	otherProvider, _ := wagering.NewRequest(wagering.RequestInput{
		ProviderID: "provider-b", ExternalTransactionID: "idem-1", IdempotencyKey: "provider-a:idem-1",
		PlayerID: out.Wallet.PlayerID().String(), WalletID: out.Wallet.ID().String(),
		RoundID: "round-1", GameID: "game-1", Kind: "BET", Amount: "1.00", Currency: "BRL",
	})
	if err := insert(otherProvider); err != nil {
		t.Errorf("other provider with the same key rejected: %v", err)
	}
}

// TestOneSuccessfulReversalPerReference: duas reversões processadas da mesma
// BET são barradas pelo índice único, mesmo que o código deixe passar.
func TestOneSuccessfulReversalPerReference(t *testing.T) {
	ctx := context.Background()
	out := openWallet(t, "100.00")
	bet := mustProcess(t, operation(t, out.Wallet, "provider-a", "rev-bet", "BET", "30.00", ""))
	mustProcess(t, operation(t, out.Wallet, "provider-a", "rev-refund", "REFUND", "30.00", "rev-bet"))

	// Simula um caso de uso com bug, que ignora a reversão já existente.
	err := uow().Do(ctx, func(ctx context.Context, tx app.Tx) error {
		w, err := tx.Wallets().Lock(ctx, out.Wallet.ID())
		if err != nil {
			return err
		}
		expected := w.Version()
		wtx, _ := wagering.NewExternal(operation(t, out.Wallet, "provider-a", "rev-rollback", "ROLLBACK", "30.00", "rev-bet"), time.Now())
		res, err := wagering.Process(wtx, w, wagering.Reference{Transaction: bet.Transaction, Reversed: false}, trace, time.Now())
		if err != nil {
			return err
		}
		if err := tx.Transactions().Insert(ctx, res.Transaction, app.TransactionMeta{Trace: trace}); err != nil {
			return err
		}
		if err := tx.Ledger().Append(ctx, *res.Entry); err != nil {
			return err
		}
		return tx.Wallets().Save(ctx, w, expected)
	})
	var dup *app.DuplicateError
	if !errors.As(err, &dup) || dup.Constraint != "wager_transactions_one_reversal_uq" {
		t.Fatalf("second reversal: error = %v, want one_reversal unique violation", err)
	}
	if w := getWallet(t, out.Wallet.ID()); w.Balance().Amount() != "100.00" {
		t.Fatalf("balance = %v, the bet was refunded twice", w.Balance())
	}
	assertLedgerMatchesBalance(t, out.Wallet.ID())
}

func TestUnitOfWorkRollsBackEverything(t *testing.T) {
	ctx := context.Background()
	out, _ := wagering.OpenWallet(newPlayer(t), brl(t, "100.00"), trace, time.Now())
	boom := errors.New("failure after writes")
	err := uow().Do(ctx, func(ctx context.Context, tx app.Tx) error {
		if err := persistOpening(ctx, tx, out); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v", err)
	}
	for table, col := range map[string]string{
		"wallets": "id", "wager_transactions": "wallet_id", "wallet_ledger_entries": "wallet_id",
	} {
		var n int
		_ = appPool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE `+col+` = $1`, out.Wallet.ID().String()).Scan(&n)
		if n != 0 {
			t.Errorf("%s has %d rows after rollback", table, n)
		}
	}
	var events int
	_ = appPool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE event_id = $1`, out.Events[0].ID().String()).Scan(&events)
	if events != 0 {
		t.Error("outbox event survived rollback")
	}
}

// TestSaveDetectsStaleVersion: a checagem de versão no UPDATE é a defesa
// contra uma escrita feita sem o lock.
func TestSaveDetectsStaleVersion(t *testing.T) {
	ctx := context.Background()
	out := openWallet(t, "100.00")
	reg := prometheus.NewRegistry()
	counted := postgres.NewUnitOfWork(appPool, 2*time.Second, metrics.New(reg))
	err := counted.Do(ctx, func(ctx context.Context, tx app.Tx) error {
		w, err := tx.Wallets().Get(ctx, out.Wallet.ID())
		if err != nil {
			return err
		}
		txID, _ := domain.NewID[domain.TransactionID]()
		if _, err := w.Credit(txID, brl(t, "1.00"), time.Now()); err != nil {
			return err
		}
		return tx.Wallets().Save(ctx, w, 99)
	})
	if !errors.Is(err, app.ErrConcurrentUpdate) || !app.IsRetryable(err) {
		t.Fatalf("stale save: error = %v, want ErrConcurrentUpdate", err)
	}
	if n := metricstest.Value(t, reg, "wallet_concurrency_conflicts_total", map[string]string{"kind": "stale_version"}); n != 1 {
		t.Errorf("stale_version conflicts = %v, want 1", n)
	}
}

// TestConcurrentDebitsDoNotLoseUpdates: muitas apostas simultâneas na mesma
// carteira, cada uma em sua transação; nenhuma atualização é perdida.
func TestConcurrentDebitsDoNotLoseUpdates(t *testing.T) {
	out := openWallet(t, "100.00")
	const n = 40

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := operation(t, out.Wallet, "provider-a", fmt.Sprintf("conc-%d", i), "BET", "1.00", "")
			errs <- uow().Do(context.Background(), func(ctx context.Context, tx app.Tx) error {
				_, err := processNew(ctx, tx, req)
				return err
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent bet failed: %v", err)
		}
	}

	w := getWallet(t, out.Wallet.ID())
	if w.Balance().Amount() != "60.00" || w.Version() != n+1 {
		t.Fatalf("wallet = %v v%d, want 60.00 v%d", w.Balance(), w.Version(), n+1)
	}
	entries := ledgerOf(t, w.ID())
	if len(entries) != n+1 {
		t.Fatalf("ledger has %d entries, want %d", len(entries), n+1)
	}
	for i, e := range entries {
		if e.WalletVersion() != int64(i+1) {
			t.Fatalf("entry %d has version %d", i, e.WalletVersion())
		}
	}
	assertLedgerMatchesBalance(t, w.ID())
}

// TestTwoConcurrentBetsOf80On100: uma processada, outra rejeitada por saldo
// insuficiente, saldo final 20.00 e um único débito.
func TestTwoConcurrentBetsOf80On100(t *testing.T) {
	out := openWallet(t, "100.00")
	start := make(chan struct{})
	results := make(chan wagering.Outcome, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"b80-1", "b80-2"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			req := operation(t, out.Wallet, "provider-a", id, "BET", "80.00", "")
			<-start
			var res wagering.Outcome
			err := uow().Do(context.Background(), func(ctx context.Context, tx app.Tx) error {
				var err error
				res, err = processNew(ctx, tx, req)
				return err
			})
			if err != nil {
				t.Errorf("bet %s: %v", id, err)
			}
			results <- res
		}(id)
	}
	close(start)
	wg.Wait()
	close(results)

	statuses := map[wagering.Status]int{}
	for r := range results {
		statuses[r.Transaction.Status()]++
		if r.Transaction.Status() == wagering.Rejected && r.Transaction.FailureCode() != domain.CodeInsufficientFunds {
			t.Errorf("rejection code = %s", r.Transaction.FailureCode())
		}
	}
	if statuses[wagering.Processed] != 1 || statuses[wagering.Rejected] != 1 {
		t.Fatalf("statuses = %v", statuses)
	}
	w := getWallet(t, out.Wallet.ID())
	if w.Balance().Amount() != "20.00" {
		t.Fatalf("balance = %v", w.Balance())
	}
	debits := 0
	for _, e := range ledgerOf(t, w.ID()) {
		if e.Direction() == wallet.Debit {
			debits++
		}
	}
	if debits != 1 {
		t.Fatalf("ledger has %d debits, want 1", debits)
	}
	assertLedgerMatchesBalance(t, w.ID())
}

// TestIndependentWalletsDoNotBlock: com a carteira A travada por uma
// transação aberta, a carteira B continua sendo processada.
func TestIndependentWalletsDoNotBlock(t *testing.T) {
	a := openWallet(t, "100.00")
	b := openWallet(t, "100.00")

	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- uow().Do(context.Background(), func(ctx context.Context, tx app.Tx) error {
			if _, err := tx.Wallets().Lock(ctx, a.Wallet.ID()); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	started := time.Now()
	mustProcess(t, operation(t, b.Wallet, "provider-a", "indep-b", "BET", "10.00", ""))
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("wallet B waited %v for wallet A's lock", elapsed)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestLockTimeoutIsTransient: esperar demais pelo lock de uma carteira
// resulta em erro transitório, sem nada confirmado.
func TestLockTimeoutIsTransient(t *testing.T) {
	a := openWallet(t, "100.00")
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- uow().Do(context.Background(), func(ctx context.Context, tx app.Tx) error {
			if _, err := tx.Wallets().Lock(ctx, a.Wallet.ID()); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	defer func() { close(release); <-done }()

	reg := prometheus.NewRegistry()
	short := postgres.NewUnitOfWork(appPool, 200*time.Millisecond, metrics.New(reg))
	err := short.Do(context.Background(), func(ctx context.Context, tx app.Tx) error {
		_, err := processNew(ctx, tx, operation(t, a.Wallet, "provider-a", "timeout-bet", "BET", "10.00", ""))
		return err
	})
	if !errors.Is(err, app.ErrTransient) || !app.IsRetryable(err) {
		t.Fatalf("error = %v, want transient", err)
	}
	if n := metricstest.Value(t, reg, "wallet_concurrency_conflicts_total", map[string]string{"kind": "lock_timeout"}); n != 1 {
		t.Errorf("lock_timeout conflicts = %v, want 1", n)
	}
}

func TestLedgerPaginationIsStable(t *testing.T) {
	ctx := context.Background()
	out := openWallet(t, "100.00")
	for i := 0; i < 5; i++ {
		mustProcess(t, operation(t, out.Wallet, "provider-a", fmt.Sprintf("page-%d", i), "BET", "1.00", ""))
	}
	var versions []int64
	var after int64
	for {
		var page []wallet.LedgerEntry
		_ = uow().Do(ctx, func(ctx context.Context, tx app.Tx) error {
			var err error
			page, err = tx.Ledger().List(ctx, out.Wallet.ID(), after, 2)
			return err
		})
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			versions = append(versions, e.WalletVersion())
		}
		after = page[len(page)-1].WalletVersion()
	}
	if fmt.Sprint(versions) != "[1 2 3 4 5 6]" {
		t.Fatalf("versions = %v", versions)
	}
}

func TestInboxDeduplicates(t *testing.T) {
	ctx := context.Background()
	msg := app.InboxMessage{ConsumerName: "wager-consumer", MessageID: fmt.Sprintf("msg-%d", time.Now().UnixNano()),
		PayloadHash: "v1:abc", ReceivedAt: domain.Timestamp(time.Now())}

	register := func(m app.InboxMessage) (app.InboxMessage, bool) {
		var got app.InboxMessage
		var inserted bool
		err := uow().Do(ctx, func(ctx context.Context, tx app.Tx) error {
			var err error
			got, inserted, err = tx.Inbox().Register(ctx, m)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return got, inserted
	}
	if _, inserted := register(msg); !inserted {
		t.Fatal("first registration not inserted")
	}
	redelivery := msg
	redelivery.PayloadHash = "v1:different"
	existing, inserted := register(redelivery)
	if inserted || existing.PayloadHash != "v1:abc" {
		t.Fatalf("redelivery = %+v inserted=%v, want the original record", existing, inserted)
	}

	complete := func() error {
		return uow().Do(ctx, func(ctx context.Context, tx app.Tx) error {
			return tx.Inbox().MarkCompleted(ctx, msg.ConsumerName, msg.MessageID, time.Now())
		})
	}
	if err := complete(); err != nil {
		t.Fatal(err)
	}
	if err := complete(); !errors.Is(err, app.ErrConcurrentUpdate) {
		t.Fatalf("second completion = %v", err)
	}
}
