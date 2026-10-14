//go:build integration

package app_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gbrpscal/wallet/internal/adapters/postgres"
	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/event"
	"github.com/gbrpscal/wallet/internal/domain/money"
	"github.com/gbrpscal/wallet/internal/domain/wagering"
	"github.com/gbrpscal/wallet/internal/platform/logctx"
	"github.com/gbrpscal/wallet/internal/testutil/pgtest"
)

var (
	ownerPool *pgxpool.Pool
	appPool   *pgxpool.Pool
)

func TestMain(m *testing.M) {
	db, err := pgtest.New(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ownerPool, appPool = db.Owner, db.App
	code := m.Run()
	db.Close()
	os.Exit(code)
}

// fakeClock é um relógio controlado pelos testes.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// testPolicy: 3 tentativas ou 30 minutos, esperas de 1s a 4s.
var testPolicy = app.ReferencePolicy{MaxAttempts: 3, TTL: 30 * time.Minute, BaseDelay: time.Second, MaxDelay: 4 * time.Second}

type harness struct {
	svc   *app.Service
	clock *fakeClock
	logs  *syncBuffer
}

// syncBuffer coleta os logs para asserções.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newHarness() *harness {
	return newHarnessWith(app.NopMetrics{})
}

// newHarnessWith usa as métricas informadas no serviço e na unidade de
// trabalho; os logs passam pelo mesmo handler de contexto da aplicação.
func newHarnessWith(m app.Metrics) *harness {
	clock := &fakeClock{now: time.Now().UTC()}
	logs := &syncBuffer{}
	log := slog.New(logctx.Handler{Handler: slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})})
	svc := app.NewService(postgres.NewUnitOfWork(appPool, 5*time.Second, m), clock,
		app.RetryPolicy{Retries: 5, BaseDelay: 5 * time.Millisecond}, testPolicy, m, log)
	return &harness{svc: svc, clock: clock, logs: logs}
}

func traceFor(name string) event.Trace {
	return event.Trace{CorrelationID: "corr-" + name, CausationID: "cause-" + name}
}

func newPlayerID(t *testing.T) string {
	t.Helper()
	id, err := domain.NewID[domain.PlayerID]()
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

func (h *harness) open(t *testing.T, amount string) string {
	t.Helper()
	w, err := h.svc.OpenWallet(context.Background(),
		app.OpenWalletInput{PlayerID: newPlayerID(t), Amount: amount, Currency: "BRL"}, traceFor("open"))
	if err != nil {
		t.Fatalf("open wallet: %v", err)
	}
	return w.ID().String()
}

// request monta a operação de exemplo do enunciado para a carteira.
func (h *harness) request(t *testing.T, walletID, extID, kind, amount, ref string) wagering.RequestInput {
	t.Helper()
	w, err := h.svc.GetWallet(context.Background(), domain.MustParseID[domain.WalletID](walletID))
	if err != nil {
		t.Fatal(err)
	}
	return wagering.RequestInput{
		ProviderID: "provider-a", ExternalTransactionID: extID, IdempotencyKey: "provider-a:" + extID,
		PlayerID: w.PlayerID().String(), WalletID: walletID, RoundID: "round-987", GameID: "fortune-chimp",
		Kind: kind, Amount: amount, Currency: "BRL", ReferenceExternalTransactionID: ref,
	}
}

func (h *harness) submit(t *testing.T, in wagering.RequestInput) app.OperationResult {
	t.Helper()
	res, err := h.svc.SubmitOperation(context.Background(), in, traceFor(in.ExternalTransactionID))
	if err != nil {
		t.Fatalf("submit %s %s: %v", in.Kind, in.ExternalTransactionID, err)
	}
	return res
}

func (h *harness) balance(t *testing.T, walletID string) string {
	t.Helper()
	w, err := h.svc.GetWallet(context.Background(), domain.MustParseID[domain.WalletID](walletID))
	if err != nil {
		t.Fatal(err)
	}
	return w.Balance().Amount()
}

func resultBalance(t *testing.T, res app.OperationResult) string {
	t.Helper()
	b, ok := res.Transaction.ResultBalance()
	if !ok {
		t.Fatalf("transaction %s has no result balance", res.Transaction.ID())
	}
	return b.Amount()
}

// assertReconciled confere o saldo armazenado contra créditos − débitos do ledger.
func (h *harness) assertReconciled(t *testing.T, walletID string) {
	t.Helper()
	r, err := h.svc.Reconcile(context.Background(), domain.MustParseID[domain.WalletID](walletID))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Consistent {
		t.Fatalf("wallet %s diverges: stored %v, calculated %v", walletID, r.Stored, r.Calculated)
	}
}

func countRows(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := appPool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func ledgerCount(t *testing.T, walletID string) int {
	return countRows(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, walletID)
}

func brlMoney(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}
