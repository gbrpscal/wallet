package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
)

// UnitOfWork implementa app.UnitOfWork com uma transação READ COMMITTED por
// chamada. A serialização por carteira vem do SELECT ... FOR UPDATE feito por
// WalletRepository.Lock, não do nível de isolamento.
type UnitOfWork struct {
	pool        *pgxpool.Pool
	lockTimeout time.Duration
	metrics     app.Metrics
}

// NewUnitOfWork cria a UnitOfWork. lockTimeout limita a espera por locks de
// linha; ao estourar, o erro é transitório (55P03) e nada é confirmado. Cada
// conflito de concorrência classificado aqui é contado em metrics.
func NewUnitOfWork(pool *pgxpool.Pool, lockTimeout time.Duration, metrics app.Metrics) *UnitOfWork {
	return &UnitOfWork{pool: pool, lockTimeout: lockTimeout, metrics: metrics}
}

// Do executa fn em uma transação. Commit só acontece se fn não devolver erro;
// as conferências diferidas do banco (saldo x ledger) rodam no commit e, se
// falharem, nada é confirmado.
func (u *UnitOfWork) Do(ctx context.Context, fn func(ctx context.Context, tx app.Tx) error) error {
	return u.run(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, fn)
}

// View executa fn numa transação REPEATABLE READ READ ONLY: um único snapshot
// para todas as leituras, sem bloquear escritores e sem aceitar escritas.
func (u *UnitOfWork) View(ctx context.Context, fn func(ctx context.Context, tx app.Tx) error) error {
	return u.run(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, fn)
}

func (u *UnitOfWork) run(ctx context.Context, opts pgx.TxOptions, fn func(ctx context.Context, tx app.Tx) error) (err error) {
	defer func() { u.countConflict(err) }()
	tx, err := u.pool.BeginTx(ctx, opts)
	if err != nil {
		return translate(err)
	}
	defer func() {
		if err != nil {
			// O rollback usa um contexto próprio para rodar mesmo com ctx cancelado.
			rbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rbCtx)
		}
	}()

	if u.lockTimeout > 0 {
		if _, err = tx.Exec(ctx, "SELECT set_config('lock_timeout', $1, true)", fmt.Sprintf("%dms", u.lockTimeout.Milliseconds())); err != nil {
			return translate(err)
		}
	}
	if err = fn(ctx, &repos{tx: tx}); err != nil {
		return translateIfDB(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return translate(err)
	}
	return nil
}

// countConflict registra o tipo de conflito de concorrência de um erro já
// classificado: versão desatualizada, unicidade, deadlock, lock_timeout ou
// serialização. Falhas de conexão não são conflitos.
func (u *UnitOfWork) countConflict(err error) {
	if err == nil {
		return
	}
	var te *app.TransientError
	switch {
	case errors.As(err, &te) && te.Conflict != "":
		u.metrics.ConcurrencyConflict(te.Conflict)
	case errors.Is(err, app.ErrConcurrentUpdate):
		u.metrics.ConcurrencyConflict(app.ConflictStaleVersion)
	case errors.Is(err, app.ErrDuplicate):
		u.metrics.ConcurrencyConflict(app.ConflictUnique)
	}
}

// translateIfDB preserva erros já classificados (pelos repositórios ou pelo
// caso de uso) e classifica os demais.
func translateIfDB(err error) error {
	if _, ok := domain.KindOf(err); ok ||
		errors.Is(err, app.ErrTransient) || errors.Is(err, app.ErrConcurrentUpdate) ||
		errors.Is(err, app.ErrDuplicate) || errors.Is(err, app.ErrNotFound) {
		return err
	}
	return translate(err)
}

type repos struct {
	tx pgx.Tx
}

func (r *repos) Wallets() app.WalletRepository           { return walletRepo{q: r.tx} }
func (r *repos) Transactions() app.TransactionRepository { return transactionRepo{q: r.tx} }
func (r *repos) Ledger() app.LedgerRepository            { return ledgerRepo{q: r.tx} }
func (r *repos) Outbox() app.OutboxRepository            { return outboxRepo{q: r.tx} }
func (r *repos) Inbox() app.InboxRepository              { return inboxRepo{q: r.tx} }
