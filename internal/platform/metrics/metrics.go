// Package metrics implementa a porta app.Metrics com Prometheus e expõe
// /metrics numa porta separada, acessível só pela rede interna.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/gbrpscal/wallet/internal/app"
)

// Prometheus implementa app.Metrics. Os labels têm só valores fixos (canal,
// tipo, status, código), sem o provedor, para manter a cardinalidade baixa.
// Nenhuma métrica carrega valor monetário: elas contam e medem tempo.
type Prometheus struct {
	operations        *prometheus.CounterVec
	duration          *prometheus.HistogramVec
	duplicates        *prometheus.CounterVec
	retries           *prometheus.CounterVec
	deadLetters       *prometheus.CounterVec
	conflicts         *prometheus.CounterVec
	outboxDelay       prometheus.Histogram
	outboxPending     prometheus.Gauge
	outboxOldestAge   prometheus.Gauge
	deadLetterDepth   *prometheus.GaugeVec
	reconciliations   *prometheus.CounterVec
	referenceAttempts *prometheus.CounterVec
}

// New cria e registra as métricas em reg.
func New(reg prometheus.Registerer) *Prometheus {
	p := &Prometheus{
		operations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wallet_operations_total",
			Help: "Operações concluídas de fato (sem replays), por canal, tipo, status e código de falha.",
		}, []string{"channel", "kind", "status", "failure_code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "wallet_operation_duration_seconds",
			Help:    "Duração do processamento no caso de uso (inclui retries internos); outcome é o status ou o código do erro.",
			Buckets: prometheus.DefBuckets,
		}, []string{"channel", "kind", "outcome"}),
		duplicates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wallet_duplicates_total",
			Help: "Recebimentos repetidos: replays idempotentes e reentregas reconhecidas pela inbox.",
		}, []string{"channel", "reason"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wallet_retries_total",
			Help: "Novas tentativas por componente e motivo.",
		}, []string{"component", "reason"}),
		deadLetters: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wallet_dead_letters_total",
			Help: "Mensagens enviadas à DLQ pelo consumidor, por fila e código de erro.",
		}, []string{"queue", "error_code"}),
		conflicts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wallet_concurrency_conflicts_total",
			Help: "Conflitos de concorrência no banco, por tipo.",
		}, []string{"kind"}),
		outboxDelay: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "wallet_outbox_publish_delay_seconds",
			Help:    "Tempo entre o commit do evento e a sua publicação.",
			Buckets: append(append([]float64{}, prometheus.DefBuckets...), 30, 60, 300, 900),
		}),
		outboxPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "wallet_outbox_pending_events",
			Help: "Eventos da outbox ainda não publicados (valor global, lido do banco).",
		}),
		outboxOldestAge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "wallet_outbox_oldest_pending_age_seconds",
			Help: "Idade do evento pendente mais antigo da outbox; 0 sem pendências.",
		}),
		deadLetterDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "wallet_dead_letter_queue_messages",
			Help: "Mensagens na DLQ (inclui as movidas pelo redrive do SQS).",
		}, []string{"queue"}),
		reconciliations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wallet_reconciliations_total",
			Help: "Reconciliações executadas, por resultado.",
		}, []string{"result"}),
		referenceAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wallet_pending_reference_attempts_total",
			Help: "Tentativas do worker de referências, por resultado.",
		}, []string{"result"}),
	}
	reg.MustRegister(p.operations, p.duration, p.duplicates, p.retries, p.deadLetters, p.conflicts,
		p.outboxDelay, p.outboxPending, p.outboxOldestAge, p.deadLetterDepth, p.reconciliations,
		p.referenceAttempts)
	return p
}

var _ app.Metrics = (*Prometheus)(nil)

func (p *Prometheus) OperationCompleted(ch app.Channel, kind, status, failureCode string) {
	p.operations.WithLabelValues(string(ch), kind, status, failureCode).Inc()
}

func (p *Prometheus) OperationDuration(ch app.Channel, kind, outcome string, d time.Duration) {
	p.duration.WithLabelValues(string(ch), kind, outcome).Observe(d.Seconds())
}

func (p *Prometheus) Duplicate(ch app.Channel, reason string) {
	p.duplicates.WithLabelValues(string(ch), reason).Inc()
}

func (p *Prometheus) Retry(component, reason string) {
	p.retries.WithLabelValues(component, reason).Inc()
}

func (p *Prometheus) DeadLetter(queue, code string) {
	p.deadLetters.WithLabelValues(queue, code).Inc()
}

func (p *Prometheus) ConcurrencyConflict(kind string) { p.conflicts.WithLabelValues(kind).Inc() }

func (p *Prometheus) OutboxPublished(delay time.Duration) { p.outboxDelay.Observe(delay.Seconds()) }

func (p *Prometheus) OutboxBacklog(pending int, oldest time.Duration) {
	p.outboxPending.Set(float64(pending))
	p.outboxOldestAge.Set(oldest.Seconds())
}

func (p *Prometheus) DeadLetterDepth(queue string, n int) {
	p.deadLetterDepth.WithLabelValues(queue).Set(float64(n))
}

func (p *Prometheus) Reconciliation(consistent bool) {
	result := "consistent"
	if !consistent {
		result = "divergent"
	}
	p.reconciliations.WithLabelValues(result).Inc()
}

func (p *Prometheus) PendingReferenceAttempt(result string) {
	p.referenceAttempts.WithLabelValues(result).Inc()
}
