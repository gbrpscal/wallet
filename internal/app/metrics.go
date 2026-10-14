package app

import "time"

// Channel é a origem de uma operação nas métricas.
type Channel string

const (
	ChannelHTTP   Channel = "http"
	ChannelSQS    Channel = "sqs"
	ChannelWorker Channel = "worker"
)

// Motivos de duplicata.
const (
	DuplicateIdempotentReplay = "idempotent_replay"
	DuplicateInboxRedelivery  = "inbox_redelivery"
)

// Componentes e motivos de retry.
const (
	RetryComponentUseCase   = "usecase"
	RetryComponentConsumer  = "sqs_consumer"
	RetryComponentPublisher = "outbox_publisher"

	RetryReasonTransient    = "transient"
	RetryReasonStaleVersion = "stale_version"
	RetryReasonDuplicate    = "duplicate"
	RetryReasonBrokerError  = "broker_error"
)

// Tipos de conflito de concorrência, classificados pelo adaptador de banco.
const (
	ConflictStaleVersion  = "stale_version"
	ConflictLockTimeout   = "lock_timeout"
	ConflictDeadlock      = "deadlock"
	ConflictSerialization = "serialization"
	ConflictUnique        = "unique"
)

// Metrics é a porta de métricas: a camada app registra fatos de negócio e
// operacionais sem depender da biblioteca de métricas.
type Metrics interface {
	// OperationCompleted conta uma operação concluída de fato (não replays):
	// status PROCESSED, REJECTED ou PENDING_REFERENCE.
	OperationCompleted(channel Channel, kind, status, failureCode string)
	// OperationDuration mede o processamento; outcome é o status ou o código do erro.
	OperationDuration(channel Channel, kind, outcome string, d time.Duration)
	Duplicate(channel Channel, reason string)
	Retry(component, reason string)
	DeadLetter(queue, errorCode string)
	ConcurrencyConflict(kind string)
	OutboxPublished(delay time.Duration)
	OutboxBacklog(pending int, oldestAge time.Duration)
	DeadLetterDepth(queue string, messages int)
	// Reconciliation conta o resultado; o valor da divergência nunca entra em
	// métrica (que só aceita float) e fica na resposta e no log.
	Reconciliation(consistent bool)
	PendingReferenceAttempt(result string)
}

// NopMetrics descarta tudo; usado quando as métricas estão desligadas e nos
// testes que não as verificam.
type NopMetrics struct{}

func (NopMetrics) OperationCompleted(Channel, string, string, string)       {}
func (NopMetrics) OperationDuration(Channel, string, string, time.Duration) {}
func (NopMetrics) Duplicate(Channel, string)                                {}
func (NopMetrics) Retry(string, string)                                     {}
func (NopMetrics) DeadLetter(string, string)                                {}
func (NopMetrics) ConcurrencyConflict(string)                               {}
func (NopMetrics) OutboxPublished(time.Duration)                            {}
func (NopMetrics) OutboxBacklog(int, time.Duration)                         {}
func (NopMetrics) DeadLetterDepth(string, int)                              {}
func (NopMetrics) Reconciliation(bool)                                      {}
func (NopMetrics) PendingReferenceAttempt(string)                           {}
