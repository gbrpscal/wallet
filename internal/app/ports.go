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
	Insert(ctx context.Context, t *wagering.WagerTransaction) error
	// Save grava estado, resultado e referência resolvida, desde que o estado
	// persistido ainda seja previous; caso contrário, ErrConcurrentUpdate.
	Save(ctx context.Context, t *wagering.WagerTransaction, previous wagering.Status) error
	Get(ctx context.Context, id domain.TransactionID) (*wagering.WagerTransaction, error)
	FindByIdempotencyKey(ctx context.Context, providerID, key string) (*wagering.WagerTransaction, error)
	FindByExternalID(ctx context.Context, providerID, externalID string) (*wagering.WagerTransaction, error)
	// HasProcessedReversal informa se alguma reversão PROCESSED já aponta para id.
	HasProcessedReversal(ctx context.Context, id domain.TransactionID) (bool, error)
}

// LedgerRepository acrescenta e lê lançamentos; não há alteração nem remoção.
type LedgerRepository interface {
	Append(ctx context.Context, e wallet.LedgerEntry) error
	// List devolve até limit lançamentos com versão maior que afterVersion,
	// em ordem crescente de versão.
	List(ctx context.Context, walletID domain.WalletID, afterVersion int64, limit int) ([]wallet.LedgerEntry, error)
}

// OutboxRepository grava eventos para publicação posterior ao commit.
type OutboxRepository interface {
	Append(ctx context.Context, events ...event.Event) error
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
