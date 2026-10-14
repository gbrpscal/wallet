//go:build integration

package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/gbrpscal/wallet/internal/adapters/postgres"
	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/platform/metrics"
	"github.com/gbrpscal/wallet/internal/platform/metrics/metricstest"
)

// newMetricsHarness devolve um harness com métricas reais em um registry
// exclusivo do teste.
func newMetricsHarness() (*harness, *prometheus.Registry) {
	reg := prometheus.NewRegistry()
	return newHarnessWith(metrics.New(reg)), reg
}

func op(channel, kind, status, code string) map[string]string {
	return map[string]string{"channel": channel, "kind": kind, "status": status, "failure_code": code}
}

func duration(channel, kind, outcome string) map[string]string {
	return map[string]string{"channel": channel, "kind": kind, "outcome": outcome}
}

func assertMetric(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string, want float64) {
	t.Helper()
	if got := metricstest.Value(t, reg, name, labels); got != want {
		t.Errorf("%s%v = %v, want %v", name, labels, got, want)
	}
}

// TestMetricsFiftyIdenticalBets: 50 apostas iguais em paralelo resultam em
// 1 operação concluída e 49 duplicatas; as 50 entram no histograma.
func TestMetricsFiftyIdenticalBets(t *testing.T) {
	h, reg := newMetricsHarness()
	walletID := h.open(t, "1000.00")
	bet := h.request(t, walletID, "metrics-same-bet", "BET", "25.00", "")

	const n = 50
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := h.svc.SubmitOperation(context.Background(), bet, traceFor("metrics-same-bet")); err != nil {
				t.Errorf("submit: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	assertMetric(t, reg, "wallet_operations_total", op("http", "OPENING", "PROCESSED", ""), 1)
	assertMetric(t, reg, "wallet_operations_total", op("http", "BET", "PROCESSED", ""), 1)
	assertMetric(t, reg, "wallet_duplicates_total", map[string]string{"channel": "http", "reason": "idempotent_replay"}, n-1)
	assertMetric(t, reg, "wallet_operation_duration_seconds", duration("http", "BET", "PROCESSED"), n)
	assertMetric(t, reg, "wallet_operation_duration_seconds", duration("http", "OPENING", "PROCESSED"), 1)
}

// TestMetricsRejectionAndErrors: rejeição conta como operação com o código;
// erro de validação entra só no histograma; replay da rejeição é duplicata.
func TestMetricsRejectionAndErrors(t *testing.T) {
	h, reg := newMetricsHarness()
	walletID := h.open(t, "100.00")

	bet := h.request(t, walletID, "metrics-too-big", "BET", "150.00", "")
	h.submit(t, bet)
	h.submit(t, bet)

	invalid := h.request(t, walletID, "metrics-invalid", "BET", "0.00", "")
	if _, err := h.svc.SubmitOperation(context.Background(), invalid, traceFor("invalid")); err == nil {
		t.Fatal("zero amount accepted")
	}
	unknown := h.request(t, walletID, "metrics-unknown", "JACKPOT", "1.00", "")
	if _, err := h.svc.SubmitOperation(context.Background(), unknown, traceFor("unknown")); err == nil {
		t.Fatal("unknown kind accepted")
	}

	assertMetric(t, reg, "wallet_operations_total", op("http", "BET", "REJECTED", "INSUFFICIENT_FUNDS"), 1)
	assertMetric(t, reg, "wallet_duplicates_total", map[string]string{"channel": "http", "reason": "idempotent_replay"}, 1)
	assertMetric(t, reg, "wallet_operation_duration_seconds", duration("http", "BET", "REJECTED"), 2)
	assertMetric(t, reg, "wallet_operation_duration_seconds", duration("http", "BET", "AMOUNT_MUST_BE_POSITIVE"), 1)
	assertMetric(t, reg, "wallet_operation_duration_seconds", duration("http", "unknown", "KIND_NOT_ALLOWED"), 1)
	for _, code := range []string{"AMOUNT_MUST_BE_POSITIVE", "KIND_NOT_ALLOWED"} {
		assertMetric(t, reg, "wallet_operations_total", op("http", "BET", "REJECTED", code), 0)
	}
}

// TestMetricsPendingReferenceExpiration: cada tentativa do worker é contada;
// a expiração conta a operação concluída no canal worker.
func TestMetricsPendingReferenceExpiration(t *testing.T) {
	h, reg := newMetricsHarness()
	walletID := h.open(t, "100.00")
	res := h.submit(t, h.request(t, walletID, "metrics-orphan", "ROLLBACK", "10.00", "never-arrives"))

	for i := 0; i < testPolicy.MaxAttempts; i++ {
		h.clock.Advance(10 * time.Second)
		if _, err := h.svc.ResolvePendingReference(context.Background(), res.Transaction.ID()); err != nil {
			t.Fatal(err)
		}
	}
	// Pendência já encerrada: ignorada, sem métrica.
	if _, err := h.svc.ResolvePendingReference(context.Background(), res.Transaction.ID()); err != nil {
		t.Fatal(err)
	}

	assertMetric(t, reg, "wallet_operations_total", op("http", "ROLLBACK", "PENDING_REFERENCE", ""), 1)
	assertMetric(t, reg, "wallet_pending_reference_attempts_total", map[string]string{"result": "waiting"}, 2)
	assertMetric(t, reg, "wallet_pending_reference_attempts_total", map[string]string{"result": "expired"}, 1)
	assertMetric(t, reg, "wallet_operations_total", op("worker", "ROLLBACK", "REJECTED", "REFERENCE_NOT_FOUND"), 1)
	assertMetric(t, reg, "wallet_operation_duration_seconds", duration("worker", "ROLLBACK", "PENDING_REFERENCE"), 2)
	assertMetric(t, reg, "wallet_operation_duration_seconds", duration("worker", "ROLLBACK", "REJECTED"), 1)
}

// TestMetricsReconciliation: a reconciliação conta só o resultado.
func TestMetricsReconciliation(t *testing.T) {
	h, reg := newMetricsHarness()
	walletID := h.open(t, "10.00")
	if _, err := h.svc.Reconcile(context.Background(), domain.MustParseID[domain.WalletID](walletID)); err != nil {
		t.Fatal(err)
	}
	assertMetric(t, reg, "wallet_reconciliations_total", map[string]string{"result": "consistent"}, 1)
	assertMetric(t, reg, "wallet_reconciliations_total", map[string]string{"result": "divergent"}, 0)
}

// moneyPattern encontra valores monetários ("25.00") e em centavos
// ("2500") vindos das operações do teste.
var moneyPattern = regexp.MustCompile(`"[^"]*":\s*"?(1000\.00|975\.00|25\.00|100000|97500|2500)"?`)

// TestLogsCarryIdsButNoMoney: os fluxos principais registram ids pelo
// contexto e nunca valores; o único log com valores é o de divergência.
func TestLogsCarryIdsButNoMoney(t *testing.T) {
	h, _ := newMetricsHarness()
	ctx := context.Background()
	walletID := h.open(t, "1000.00")
	bet := h.request(t, walletID, "logs-bet", "BET", "25.00", "")
	h.submit(t, bet)
	h.submit(t, bet)
	h.submit(t, h.request(t, walletID, "logs-refund", "REFUND", "25.00", "logs-missing"))
	pending := h.request(t, walletID, "logs-rollback", "ROLLBACK", "25.00", "logs-missing-2")
	res := h.submit(t, pending)
	for i := 0; i < testPolicy.MaxAttempts; i++ {
		h.clock.Advance(10 * time.Second)
		if _, err := h.svc.ResolvePendingReference(ctx, res.Transaction.ID()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.svc.Reconcile(ctx, domain.MustParseID[domain.WalletID](walletID)); err != nil {
		t.Fatal(err)
	}

	out := h.logs.String()
	if strings.TrimSpace(out) == "" {
		t.Fatal("no logs produced; the scan would prove nothing")
	}
	if m := moneyPattern.FindString(out); m != "" {
		t.Errorf("log carries a monetary value: %s\n%s", m, out)
	}
	if !strings.Contains(out, `"transactionId":"`+res.Transaction.ID().String()+`"`) {
		t.Errorf("expiration log lacks transactionId from context:\n%s", out)
	}
}

// TestMetricsMessageChannel: reentrega da mesma mensagem conta como duplicata
// da inbox; outra mensagem com a mesma chave, como replay idempotente.
func TestMetricsMessageChannel(t *testing.T) {
	h, reg := newMetricsHarness()
	walletID := h.open(t, "100.00")
	in := h.request(t, walletID, "metrics-sqs-bet", "BET", "10.00", "")
	handle := func(messageID string) app.MessageResult {
		t.Helper()
		msg := app.InboxMessage{ConsumerName: "metrics-consumer", MessageID: messageID, PayloadHash: "v1:metrics-" + walletID}
		res, err := h.svc.HandleMessage(context.Background(), msg, in, traceFor(messageID))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	handle("metrics-msg-1-" + walletID)
	if !handle("metrics-msg-1-" + walletID).Duplicate {
		t.Fatal("redelivery not detected by the inbox")
	}
	handle("metrics-msg-2-" + walletID)

	assertMetric(t, reg, "wallet_operations_total", op("sqs", "BET", "PROCESSED", ""), 1)
	assertMetric(t, reg, "wallet_duplicates_total", map[string]string{"channel": "sqs", "reason": "inbox_redelivery"}, 1)
	assertMetric(t, reg, "wallet_duplicates_total", map[string]string{"channel": "sqs", "reason": "idempotent_replay"}, 1)
	assertMetric(t, reg, "wallet_operation_duration_seconds", duration("sqs", "BET", "inbox_redelivery"), 1)
	assertMetric(t, reg, "wallet_operation_duration_seconds", duration("sqs", "BET", "PROCESSED"), 2)
}

// flakyUnitOfWork falha as primeiras chamadas de Do com o erro informado e
// depois delega à unidade de trabalho real.
type flakyUnitOfWork struct {
	app.UnitOfWork
	mu       sync.Mutex
	failures []error
}

func (f *flakyUnitOfWork) Do(ctx context.Context, fn func(ctx context.Context, tx app.Tx) error) error {
	f.mu.Lock()
	if len(f.failures) > 0 {
		err := f.failures[0]
		f.failures = f.failures[1:]
		f.mu.Unlock()
		return err
	}
	f.mu.Unlock()
	return f.UnitOfWork.Do(ctx, fn)
}

// TestMetricsUseCaseRetries: cada nova tentativa do caso de uso é contada
// pelo motivo da falha anterior.
func TestMetricsUseCaseRetries(t *testing.T) {
	h := newHarness()
	walletID := h.open(t, "100.00")
	bet := h.request(t, walletID, "metrics-retry", "BET", "10.00", "")

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	uow := &flakyUnitOfWork{UnitOfWork: postgres.NewUnitOfWork(appPool, 5*time.Second, m),
		failures: []error{app.ErrConcurrentUpdate, &app.TransientError{Err: errors.New("connection reset")}}}
	svc := app.NewService(uow, h.clock, app.RetryPolicy{Retries: 3, BaseDelay: time.Millisecond}, testPolicy, m,
		slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if _, err := svc.SubmitOperation(context.Background(), bet, traceFor("metrics-retry")); err != nil {
		t.Fatal(err)
	}

	assertMetric(t, reg, "wallet_retries_total", map[string]string{"component": "usecase", "reason": "stale_version"}, 1)
	assertMetric(t, reg, "wallet_retries_total", map[string]string{"component": "usecase", "reason": "transient"}, 1)
	assertMetric(t, reg, "wallet_operations_total", op("http", "BET", "PROCESSED", ""), 1)
}
