package app

import (
	"context"
	"time"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/event"
	"github.com/gbrpscal/wallet/internal/domain/wagering"
	"github.com/gbrpscal/wallet/internal/domain/wallet"
)

// UnitOfWork delimita a transação SQL. Tudo o que fn grava pelos
// repositórios de Tx é confirmado em um único commit, ou nada é confirmado.
// O erro devolvido já vem classificado (ErrTransient, ErrConcurrentUpdate,
// ErrDuplicate, ErrNotFound ou *domain.Error).
type UnitOfWork interface {
	Do(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
	// View executa fn numa transação REPEATABLE READ somente leitura: todas
	// as leituras enxergam o mesmo snapshot, e nenhuma escrita é aceita.
	View(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}

// Tx dá acesso aos repositórios ligados à transação em andamento.
type Tx interface {
	Wallets() WalletRepository
	Transactions() TransactionRepository
	Ledger() LedgerRepository
	Outbox() OutboxRepository
	Inbox() InboxRepository
}

// WalletRepository persiste carteiras.
type WalletRepository interface {
	// Insert grava uma carteira nova; (playerId, currency) repetido resulta em
	// domain.ErrWalletAlreadyExists.
	Insert(ctx context.Context, w *wallet.Wallet) error
	// Lock lê a carteira com SELECT ... FOR UPDATE: escritores da mesma
	// carteira esperam o commit; carteiras diferentes não se bloqueiam.
	Lock(ctx context.Context, id domain.WalletID) (*wallet.Wallet, error)
	// Get lê a carteira sem lock.
	Get(ctx context.Context, id domain.WalletID) (*wallet.Wallet, error)
	// Save grava saldo, versão e updatedAt, desde que a versão persistida
	// ainda seja expectedVersion; caso contrário, ErrConcurrentUpdate.
	Save(ctx context.Context, w *wallet.Wallet, expectedVersion int64) error
}

// TransactionRepository persiste transações de wagering. Toda busca externa é
// escopada pelo provedor.
type TransactionRepository interface {
	// Insert grava a transação com o rastreio da requisição de origem e, se
	// ela estiver em PENDING_REFERENCE, com o agendamento da próxima tentativa.
	Insert(ctx context.Context, t *wagering.WagerTransaction, meta TransactionMeta) error
	// Save grava estado, resultado e referência resolvida, desde que o estado
	// persistido ainda seja previous; caso contrário, ErrConcurrentUpdate.
	// schedule só é informado quando a transação continua esperando.
	Save(ctx context.Context, t *wagering.WagerTransaction, previous wagering.Status, schedule *ReferenceSchedule) error
	Get(ctx context.Context, id domain.TransactionID) (*wagering.WagerTransaction, error)
	FindByIdempotencyKey(ctx context.Context, providerID, key string) (*wagering.WagerTransaction, error)
	FindByExternalID(ctx context.Context, providerID, externalID string) (*wagering.WagerTransaction, error)
	// HasProcessedReversal informa se alguma reversão PROCESSED já aponta para id.
	HasProcessedReversal(ctx context.Context, id domain.TransactionID) (bool, error)
	// LockPending lê com FOR UPDATE uma transação em espera, com o rastreio e
	// o agendamento persistidos. Deve ser chamado depois do lock da carteira.
	LockPending(ctx context.Context, id domain.TransactionID) (*wagering.WagerTransaction, PendingInfo, error)
	// DuePendingReferences lista, sem lock, as transações em PENDING_REFERENCE
	// cuja próxima tentativa já venceu, das mais antigas para as mais novas.
	DuePendingReferences(ctx context.Context, now time.Time, limit int) ([]PendingReference, error)
}

// TransactionMeta acompanha a gravação de uma transação.
type TransactionMeta struct {
	Trace    event.Trace
	Schedule *ReferenceSchedule
}

// ReferenceSchedule é o agendamento durável da espera por referência.
type ReferenceSchedule struct {
	Attempts      int
	NextAttemptAt time.Time
	ExpiresAt     time.Time
}

// PendingInfo é o que foi persistido junto com uma transação em espera.
type PendingInfo struct {
	Trace    event.Trace
	Schedule ReferenceSchedule
}

// PendingReference identifica uma transação esperando referência.
type PendingReference struct {
	TransactionID domain.TransactionID
	WalletID      domain.WalletID
}

// LedgerRepository acrescenta e lê lançamentos; não há alteração nem remoção.
type LedgerRepository interface {
	Append(ctx context.Context, e wallet.LedgerEntry) error
	// List devolve até limit lançamentos com versão maior que afterVersion,
	// em ordem crescente de versão.
	List(ctx context.Context, walletID domain.WalletID, afterVersion int64, limit int) ([]wallet.LedgerEntry, error)
	// Totals soma créditos − débitos de todos os lançamentos da carteira, em
	// unidades mínimas, e conta os lançamentos.
	Totals(ctx context.Context, walletID domain.WalletID) (netMinor int64, entries int, err error)
}

// OutboxRepository grava eventos para publicação posterior ao commit e
// controla a reserva (lease) dos publishers.
type OutboxRepository interface {
	Append(ctx context.Context, events ...event.Event) error
	// Claim reserva para owner, até until, no máximo limit eventos pendentes e
	// vencidos, apenas o mais antigo (por sequence) de cada agregado, e só
	// quando não há evento anterior do mesmo agregado ainda não publicado.
	Claim(ctx context.Context, owner string, now, until time.Time, limit int) ([]OutboxEvent, error)
	// Renew estende o lease dos eventos ainda reservados por owner.
	Renew(ctx context.Context, owner string, ids []domain.EventID, until time.Time) error
	// MarkPublished conclui o evento, se ainda reservado por owner. Devolve
	// false quando a reserva já não era dele (lease vencido e assumido).
	MarkPublished(ctx context.Context, id domain.EventID, owner string, at time.Time) (bool, error)
	// MarkFailed registra a falha, agenda nova tentativa e solta a reserva.
	MarkFailed(ctx context.Context, id domain.EventID, owner string, next time.Time, lastError string) error
	// Release solta reservas de owner sem contar tentativa (encerramento).
	Release(ctx context.Context, owner string, ids []domain.EventID) error
}

// OutboxEvent é um evento reservado para publicação: o snapshot exato gravado
// no commit da operação.
type OutboxEvent struct {
	ID           domain.EventID
	AggregateID  string
	EventType    string
	EventVersion int
	Payload      []byte
	Attempts     int
}

// EventPublisher entrega um evento ao broker. Republicar o mesmo evento usa o
// mesmo eventId (deduplicação no broker e nos consumidores).
type EventPublisher interface {
	Publish(ctx context.Context, e OutboxEvent) error
}

// InboxMessage é o registro de uma mensagem recebida por um consumidor.
type InboxMessage struct {
	ConsumerName string
	MessageID    string
	PayloadHash  string
	ReceivedAt   time.Time
	CompletedAt  *time.Time
}

// InboxRepository deduplica mensagens por (consumerName, messageId).
type InboxRepository interface {
	// Register grava a mensagem se ela ainda não existe. Quando já existe,
	// devolve o registro persistido e inserted = false.
	Register(ctx context.Context, m InboxMessage) (existing InboxMessage, inserted bool, err error)
	MarkCompleted(ctx context.Context, consumerName, messageID string, at time.Time) error
}
