package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/event"
	"github.com/gbrpscal/wallet/internal/domain/money"
	"github.com/gbrpscal/wallet/internal/domain/wagering"
	"github.com/gbrpscal/wallet/internal/domain/wallet"
	"github.com/gbrpscal/wallet/internal/platform/logctx"
)

// Service reúne os casos de uso. HTTP e SQS chamam os mesmos métodos, então as
// garantias de idempotência e de integridade são as mesmas nos dois canais.
type Service struct {
	uow       UnitOfWork
	clock     Clock
	retry     RetryPolicy
	reference ReferencePolicy
	metrics   Metrics
	log       *slog.Logger
}

// NewService constrói o serviço.
func NewService(uow UnitOfWork, clock Clock, retry RetryPolicy, reference ReferencePolicy, metrics Metrics, log *slog.Logger) *Service {
	return &Service{uow: uow, clock: clock, retry: retry, reference: reference, metrics: metrics, log: log}
}

// OpenWalletInput é a abertura de carteira vinda de fora.
type OpenWalletInput struct {
	PlayerID string
	Amount   string
	Currency string
}

// OperationResult é o resultado de uma operação, nova ou reproduzida.
type OperationResult struct {
	Transaction *wagering.WagerTransaction
	// Replay é true quando o resultado veio do registro persistido, sem
	// reaplicar a operação.
	Replay bool
}

// LedgerPage é uma página do ledger em ordem de walletVersion.
type LedgerPage struct {
	Entries []wallet.LedgerEntry
	// NextCursor é vazio na última página.
	NextCursor string
}

// Reconciliation é o confronto do saldo armazenado com o ledger.
type Reconciliation struct {
	WalletID       domain.WalletID
	Stored         money.Money
	Calculated     money.Money
	Difference     money.Money
	Consistent     bool
	CheckedEntries int
}

// ResolveStatus descreve o que aconteceu com uma transação em espera.
type ResolveStatus string

const (
	// ResolveCompleted: a referência apareceu e a transação foi concluída
	// (PROCESSED ou REJECTED).
	ResolveCompleted ResolveStatus = "completed"
	// ResolveWaiting: a referência continua indisponível; nova tentativa agendada.
	ResolveWaiting ResolveStatus = "waiting"
	// ResolveExpired: a espera terminou; a transação foi rejeitada com
	// REFERENCE_NOT_FOUND.
	ResolveExpired ResolveStatus = "expired"
	// ResolveSkipped: outra instância já tratou a transação ou ela ainda não
	// venceu.
	ResolveSkipped ResolveStatus = "skipped"
)

// openWallet contém a abertura; OpenWallet a mede.
func (s *Service) openWallet(ctx context.Context, in OpenWalletInput, trace event.Trace) (*wallet.Wallet, error) {
	playerID, err := domain.ParseID[domain.PlayerID](in.PlayerID)
	if err != nil {
		return nil, domain.Errorf(domain.ErrInvalidRequest, "playerId must be a uuid")
	}
	initial, err := money.Parse(in.Amount, in.Currency)
	if err != nil {
		return nil, err
	}
	if err := trace.Validate(); err != nil {
		return nil, err
	}

	var opened *wallet.Wallet
	err = s.retryDo(ctx, func() error {
		out, err := wagering.OpenWallet(playerID, initial, trace, s.clock.Now())
		if err != nil {
			return err
		}
		err = s.uow.Do(ctx, func(ctx context.Context, tx Tx) error {
			if err := tx.Wallets().Insert(ctx, out.Wallet); err != nil {
				return err
			}
			if out.Opening != nil {
				if err := tx.Transactions().Insert(ctx, out.Opening, TransactionMeta{Trace: trace}); err != nil {
					return err
				}
				if err := tx.Ledger().Append(ctx, *out.Entry); err != nil {
					return err
				}
			}
			return tx.Outbox().Append(ctx, out.Events...)
		})
		if err == nil {
			opened = out.Wallet
		}
		return err
	})
	return opened, err
}

// submitOperation contém o envio de operação; SubmitOperation o mede.
func (s *Service) submitOperation(ctx context.Context, in wagering.RequestInput, trace event.Trace) (OperationResult, error) {
	req, err := wagering.NewRequest(in)
	if err != nil {
		return OperationResult{}, err
	}
	if err := trace.Validate(); err != nil {
		return OperationResult{}, err
	}

	var result OperationResult
	err = s.retryDo(ctx, func() error {
		return s.uow.Do(ctx, func(ctx context.Context, tx Tx) error {
			var err error
			result, err = s.submit(ctx, tx, req, trace)
			return err
		})
	})
	return result, err
}

// MessageResult é o resultado do tratamento de uma mensagem recebida.
type MessageResult struct {
	// Operation é vazio quando a mensagem já tinha sido tratada (Duplicate).
	Operation OperationResult
	// Duplicate é true quando a inbox já registrava a mensagem: nada foi
	// reprocessado e ela pode ser removida da fila.
	Duplicate bool
}

// handleMessage contém o tratamento de mensagem; HandleMessage o mede.
func (s *Service) handleMessage(ctx context.Context, msg InboxMessage, in wagering.RequestInput, trace event.Trace) (MessageResult, error) {
	req, err := wagering.NewRequest(in)
	if err != nil {
		return MessageResult{}, err
	}
	if err := trace.Validate(); err != nil {
		return MessageResult{}, err
	}

	var result MessageResult
	err = s.retryDo(ctx, func() error {
		return s.uow.Do(ctx, func(ctx context.Context, tx Tx) error {
			result = MessageResult{}
			msg.ReceivedAt = domain.Timestamp(s.clock.Now())
			existing, inserted, err := tx.Inbox().Register(ctx, msg)
			if err != nil {
				return err
			}
			if !inserted {
				if existing.PayloadHash != msg.PayloadHash {
					return domain.Errorf(domain.ErrMessageHashMismatch,
						"message %s was already received with different content", msg.MessageID)
				}
				result.Duplicate = true
				return nil
			}
			if result.Operation, err = s.submit(ctx, tx, req, trace); err != nil {
				return err
			}
			return tx.Inbox().MarkCompleted(ctx, msg.ConsumerName, msg.MessageID, domain.Timestamp(s.clock.Now()))
		})
	})
	return result, err
}

func (s *Service) submit(ctx context.Context, tx Tx, req wagering.Request, trace event.Trace) (OperationResult, error) {
	// 1) Replay rápido, sem travar a carteira.
	if res, found, err := findExisting(ctx, tx, req); found || err != nil {
		return res, err
	}
	// 2) Lock da carteira: requisições da mesma carteira entram em fila.
	w, err := tx.Wallets().Lock(ctx, req.WalletID())
	if errors.Is(err, ErrNotFound) {
		return OperationResult{}, domain.Errorf(domain.ErrWalletNotFound, "wallet %s not found", req.WalletID())
	}
	if err != nil {
		return OperationResult{}, err
	}
	// 3) Nova busca sob o lock: quem esperou enxerga o que acabou de ser confirmado.
	if res, found, err := findExisting(ctx, tx, req); found || err != nil {
		return res, err
	}

	ref, err := loadReference(ctx, tx, req.ProviderID(), req.ReferenceExternalTransactionID())
	if err != nil {
		return OperationResult{}, err
	}
	now := s.clock.Now()
	wtx, err := wagering.NewExternal(req, now)
	if err != nil {
		return OperationResult{}, err
	}
	expectedVersion := w.Version()
	out, err := wagering.Process(wtx, w, ref, trace, now)
	if err != nil {
		return OperationResult{}, err
	}

	meta := TransactionMeta{Trace: trace}
	if out.Transaction.Status() == wagering.PendingReference {
		sched := s.reference.Initial(out.Transaction.CreatedAt())
		meta.Schedule = &sched
	}
	if err := tx.Transactions().Insert(ctx, out.Transaction, meta); err != nil {
		return OperationResult{}, err
	}
	if err := persistMovement(ctx, tx, out, w, expectedVersion); err != nil {
		return OperationResult{}, err
	}
	return OperationResult{Transaction: out.Transaction}, nil
}

// findExisting procura a operação pela chave e, depois, pelo id externo,
// sempre dentro do provedor.
func findExisting(ctx context.Context, tx Tx, req wagering.Request) (OperationResult, bool, error) {
	existing, err := tx.Transactions().FindByIdempotencyKey(ctx, req.ProviderID(), req.IdempotencyKey())
	if errors.Is(err, ErrNotFound) {
		existing, err = tx.Transactions().FindByExternalID(ctx, req.ProviderID(), req.ExternalTransactionID())
	}
	if errors.Is(err, ErrNotFound) {
		return OperationResult{}, false, nil
	}
	if err != nil {
		return OperationResult{}, false, err
	}
	if err := existing.CheckReplay(req); err != nil {
		return OperationResult{}, true, err
	}
	return OperationResult{Transaction: existing, Replay: true}, true, nil
}

func loadReference(ctx context.Context, tx Tx, providerID, externalID string) (wagering.Reference, error) {
	if externalID == "" {
		return wagering.Reference{}, nil
	}
	r, err := tx.Transactions().FindByExternalID(ctx, providerID, externalID)
	if errors.Is(err, ErrNotFound) {
		return wagering.Reference{}, nil
	}
	if err != nil {
		return wagering.Reference{}, err
	}
	reversed, err := tx.Transactions().HasProcessedReversal(ctx, r.ID())
	if err != nil {
		return wagering.Reference{}, err
	}
	return wagering.Reference{Transaction: r, Reversed: reversed}, nil
}

// persistMovement grava lançamento, saldo e eventos de um Outcome. A
// transação já foi gravada (Insert ou Save) por quem chama.
func persistMovement(ctx context.Context, tx Tx, out wagering.Outcome, w *wallet.Wallet, expectedVersion int64) error {
	if out.Entry != nil {
		if err := tx.Ledger().Append(ctx, *out.Entry); err != nil {
			return err
		}
		if err := tx.Wallets().Save(ctx, w, expectedVersion); err != nil {
			return err
		}
	}
	return tx.Outbox().Append(ctx, out.Events...)
}

// DuePendingReferences lista transações em espera cuja tentativa venceu.
func (s *Service) DuePendingReferences(ctx context.Context, limit int) ([]PendingReference, error) {
	var due []PendingReference
	err := s.uow.View(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		due, err = tx.Transactions().DuePendingReferences(ctx, s.clock.Now(), limit)
		return err
	})
	return due, err
}

// ResolvePendingReference faz uma tentativa de concluir uma transação em
// PENDING_REFERENCE. É seguro chamá-la de várias instâncias ao mesmo tempo:
// o lock da carteira e da transação serializa as tentativas, e quem chega
// depois encontra a transação concluída ou ainda não vencida.
func (s *Service) ResolvePendingReference(ctx context.Context, id domain.TransactionID) (ResolveStatus, error) {
	ctx = logctx.With(ctx, logctx.TransactionID, id.String())
	start := time.Now()
	var (
		status ResolveStatus
		t      *wagering.WagerTransaction
	)
	err := s.retryDo(ctx, func() error {
		return s.uow.Do(ctx, func(ctx context.Context, tx Tx) error {
			var err error
			status, t, err = s.resolve(ctx, tx, id)
			return err
		})
	})
	s.recordResolution(status, t, err, time.Since(start))
	return status, err
}

// recordResolution registra uma tentativa do worker: a tentativa em si, a
// duração e, quando a pendência termina (concluída ou expirada), a operação.
func (s *Service) recordResolution(status ResolveStatus, t *wagering.WagerTransaction, err error, d time.Duration) {
	switch {
	case err != nil:
		s.metrics.OperationDuration(ChannelWorker, "unknown", errorCode(err), d)
	case status == ResolveSkipped || t == nil:
	case status == ResolveWaiting:
		s.metrics.PendingReferenceAttempt(string(status))
		s.metrics.OperationDuration(ChannelWorker, string(t.Kind()), string(t.Status()), d)
	default:
		s.metrics.PendingReferenceAttempt(string(status))
		s.metrics.OperationCompleted(ChannelWorker, string(t.Kind()), string(t.Status()), string(t.FailureCode()))
		s.metrics.OperationDuration(ChannelWorker, string(t.Kind()), string(t.Status()), d)
	}
}

func (s *Service) resolve(ctx context.Context, tx Tx, id domain.TransactionID) (ResolveStatus, *wagering.WagerTransaction, error) {
	current, err := tx.Transactions().Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return "", nil, domain.Errorf(domain.ErrTransactionNotFound, "transaction %s not found", id)
	}
	if err != nil {
		return "", nil, err
	}
	if current.Status() != wagering.PendingReference {
		return ResolveSkipped, nil, nil
	}
	// Ordem de locks: carteira primeiro, depois a transação.
	w, err := tx.Wallets().Lock(ctx, current.WalletID())
	if err != nil {
		return "", nil, err
	}
	t, info, err := tx.Transactions().LockPending(ctx, id)
	if err != nil {
		return "", nil, err
	}
	now := s.clock.Now()
	if t.Status() != wagering.PendingReference || now.Before(info.Schedule.NextAttemptAt) {
		return ResolveSkipped, nil, nil
	}

	ref, err := loadReference(ctx, tx, t.ProviderID(), t.ReferenceExternalTransactionID())
	if err != nil {
		return "", nil, err
	}
	expectedVersion := w.Version()
	out, err := wagering.Process(t, w, ref, info.Trace, now)
	if err != nil {
		return "", nil, err
	}
	status := ResolveCompleted
	if out.Waiting {
		next := s.reference.Next(info.Schedule, now)
		if !s.reference.Exhausted(next, now) {
			return ResolveWaiting, t, tx.Transactions().Save(ctx, t, wagering.PendingReference, &next)
		}
		if out, err = wagering.Expire(t, w, info.Trace, now); err != nil {
			return "", nil, err
		}
		status = ResolveExpired
		s.log.InfoContext(logctx.With(ctx, logctx.WalletID, t.WalletID().String(), logctx.ProviderID, t.ProviderID(),
			logctx.CorrelationID, info.Trace.CorrelationID), "pending reference expired", "attempts", next.Attempts)
	}
	if err := tx.Transactions().Save(ctx, out.Transaction, wagering.PendingReference, nil); err != nil {
		return "", nil, err
	}
	return status, out.Transaction, persistMovement(ctx, tx, out, w, expectedVersion)
}

// GetWallet lê uma carteira.
func (s *Service) GetWallet(ctx context.Context, id domain.WalletID) (*wallet.Wallet, error) {
	var w *wallet.Wallet
	err := s.uow.View(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		w, err = getWallet(ctx, tx, id)
		return err
	})
	return w, err
}

func getWallet(ctx context.Context, tx Tx, id domain.WalletID) (*wallet.Wallet, error) {
	w, err := tx.Wallets().Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, domain.Errorf(domain.ErrWalletNotFound, "wallet %s not found", id)
	}
	return w, err
}

// ListLedger devolve uma página do ledger. limit 0 usa o padrão; fora de
// 1..MaxPageSize é erro de entrada.
func (s *Service) ListLedger(ctx context.Context, id domain.WalletID, cursorText string, limit int) (LedgerPage, error) {
	if limit == 0 {
		limit = DefaultPageSize
	}
	if limit < 1 || limit > MaxPageSize {
		return LedgerPage{}, domain.Errorf(domain.ErrInvalidRequest, "limit must be between 1 and %d", MaxPageSize)
	}
	after, err := DecodeCursor(cursorText)
	if err != nil {
		return LedgerPage{}, err
	}
	var page LedgerPage
	err = s.uow.View(ctx, func(ctx context.Context, tx Tx) error {
		if _, err := getWallet(ctx, tx, id); err != nil {
			return err
		}
		// Um a mais para saber se existe página seguinte.
		entries, err := tx.Ledger().List(ctx, id, after, limit+1)
		if err != nil {
			return err
		}
		if len(entries) > limit {
			entries = entries[:limit]
			page.NextCursor = EncodeCursor(entries[limit-1].WalletVersion())
		}
		page.Entries = entries
		return nil
	})
	return page, err
}

// GetTransaction lê uma transação pelo id interno. A autorização por provedor
// é feita por quem chama.
func (s *Service) GetTransaction(ctx context.Context, id domain.TransactionID) (*wagering.WagerTransaction, error) {
	var t *wagering.WagerTransaction
	err := s.uow.View(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		t, err = tx.Transactions().Get(ctx, id)
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return nil, domain.Errorf(domain.ErrTransactionNotFound, "transaction %s not found", id)
	}
	return t, err
}

// GetTransactionByExternalID lê uma transação pelo id externo, dentro do provedor.
func (s *Service) GetTransactionByExternalID(ctx context.Context, providerID, externalID string) (*wagering.WagerTransaction, error) {
	var t *wagering.WagerTransaction
	err := s.uow.View(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		t, err = tx.Transactions().FindByExternalID(ctx, providerID, externalID)
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return nil, domain.Errorf(domain.ErrTransactionNotFound, "transaction not found")
	}
	return t, err
}

// Reconcile reconstrói o saldo a partir do ledger (incluindo a abertura) num
// único snapshot e o compara com o saldo armazenado. Nunca altera o saldo.
func (s *Service) Reconcile(ctx context.Context, id domain.WalletID) (Reconciliation, error) {
	var r Reconciliation
	err := s.uow.View(ctx, func(ctx context.Context, tx Tx) error {
		w, err := getWallet(ctx, tx, id)
		if err != nil {
			return err
		}
		net, count, err := tx.Ledger().Totals(ctx, id)
		if err != nil {
			return err
		}
		calculated, err := money.FromMinor(net, w.Currency())
		if err != nil {
			return err
		}
		diff, err := w.Balance().Sub(calculated)
		if err != nil {
			return err
		}
		r = Reconciliation{
			WalletID: id, Stored: w.Balance(), Calculated: calculated, Difference: diff,
			Consistent: diff.IsZero(), CheckedEntries: count,
		}
		return nil
	})
	if err != nil {
		return Reconciliation{}, err
	}
	s.metrics.Reconciliation(r.Consistent)
	if !r.Consistent {
		// Única linha de log com valores: o enunciado exige reportar a
		// divergência nos logs.
		s.log.ErrorContext(logctx.With(ctx, logctx.WalletID, id.String()), "reconciliation divergence",
			"stored", r.Stored.Amount(), "calculated", r.Calculated.Amount(),
			"difference", r.Difference.Amount(), "currency", r.Stored.Currency().String(),
			"checkedEntries", r.CheckedEntries)
	}
	return r, nil
}
