package metrics_test

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/platform/metrics"
	"github.com/gbrpscal/wallet/internal/platform/metrics/metricstest"
)

// TestCatalog: cada método da porta alimenta a métrica do catálogo aprovado.
func TestCatalog(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	m.OperationCompleted(app.ChannelHTTP, "BET", "REJECTED", "INSUFFICIENT_FUNDS")
	m.OperationDuration(app.ChannelSQS, "WIN", "PROCESSED", 15*time.Millisecond)
	m.Duplicate(app.ChannelHTTP, app.DuplicateIdempotentReplay)
	m.Retry(app.RetryComponentUseCase, app.RetryReasonStaleVersion)
	m.DeadLetter("wager-transactions-dlq.fifo", "INVALID_MESSAGE")
	m.ConcurrencyConflict(app.ConflictLockTimeout)
	m.OutboxPublished(2 * time.Second)
	m.OutboxBacklog(3, 90*time.Second)
	m.DeadLetterDepth("wager-transactions-dlq.fifo", 4)
	m.Reconciliation(false)
	m.PendingReferenceAttempt("expired")

	cases := []struct {
		name   string
		labels map[string]string
		want   int
	}{
		{"wallet_operations_total", map[string]string{"channel": "http", "kind": "BET", "status": "REJECTED", "failure_code": "INSUFFICIENT_FUNDS"}, 1},
		{"wallet_operation_duration_seconds", map[string]string{"channel": "sqs", "kind": "WIN", "outcome": "PROCESSED"}, 1},
		{"wallet_duplicates_total", map[string]string{"channel": "http", "reason": "idempotent_replay"}, 1},
		{"wallet_retries_total", map[string]string{"component": "usecase", "reason": "stale_version"}, 1},
		{"wallet_dead_letters_total", map[string]string{"queue": "wager-transactions-dlq.fifo", "error_code": "INVALID_MESSAGE"}, 1},
		{"wallet_concurrency_conflicts_total", map[string]string{"kind": "lock_timeout"}, 1},
		{"wallet_outbox_publish_delay_seconds", map[string]string{}, 1},
		{"wallet_outbox_pending_events", map[string]string{}, 3},
		{"wallet_outbox_oldest_pending_age_seconds", map[string]string{}, 90},
		{"wallet_dead_letter_queue_messages", map[string]string{"queue": "wager-transactions-dlq.fifo"}, 4},
		{"wallet_reconciliations_total", map[string]string{"result": "divergent"}, 1},
		{"wallet_pending_reference_attempts_total", map[string]string{"result": "expired"}, 1},
	}
	for _, c := range cases {
		if got := metricstest.Value(t, reg, c.name, c.labels); int(got) != c.want {
			t.Errorf("%s%v = %v, want %d", c.name, c.labels, got, c.want)
		}
	}
}
