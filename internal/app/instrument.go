package app

import (
	"context"
	"errors"
	"time"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/event"
	"github.com/gbrpscal/wallet/internal/domain/wagering"
	"github.com/gbrpscal/wallet/internal/domain/wallet"
	"github.com/gbrpscal/wallet/internal/platform/logctx"
)

// Códigos de resultado das métricas para erros que não são de domínio; são os
// mesmos que a API devolve.
const (
	outcomeUnavailable = "UNAVAILABLE"
	outcomeInternal    = "INTERNAL_ERROR"
	kindUnknown        = "unknown"
)

// OpenWallet abre uma carteira. Saldo inicial positivo grava, no mesmo commit,
// a carteira, o OPENING, o lançamento de crédito e os eventos.
func (s *Service) OpenWallet(ctx context.Context, in OpenWalletInput, trace event.Trace) (*wallet.Wallet, error) {
	start := time.Now()
	w, err := s.openWallet(ctx, in, trace)
	outcome := string(wagering.Processed)
	if err != nil {
		outcome = errorCode(err)
	} else if w.Balance().IsPositive() {
		s.metrics.OperationCompleted(ChannelHTTP, string(wagering.Opening), string(wagering.Processed), "")
	}
	s.metrics.OperationDuration(ChannelHTTP, string(wagering.Opening), outcome, time.Since(start))
	return w, err
}

// SubmitOperation registra e processa uma operação de provedor (BET, WIN,
// LOSS, REFUND ou ROLLBACK). A mesma chave com conteúdo equivalente devolve o
// resultado persistido (Replay); com conteúdo diferente, conflito.
func (s *Service) SubmitOperation(ctx context.Context, in wagering.RequestInput, trace event.Trace) (OperationResult, error) {
	ctx = logctx.With(ctx, logctx.ProviderID, in.ProviderID, logctx.WalletID, in.WalletID)
	start := time.Now()
	res, err := s.submitOperation(ctx, in, trace)
	s.recordOperation(ChannelHTTP, in.Kind, res, err, time.Since(start))
	return res, err
}

// HandleMessage trata uma operação recebida por mensageria. O registro na
// inbox, a operação (transação, ledger, saldo, eventos) e a conclusão na
// inbox são confirmados na mesma transação SQL: depois do commit, a mensagem
// pode ser removida da fila; antes dele, nada ficou gravado.
//
// A mesma messageId com conteúdo diferente resulta em MESSAGE_HASH_MISMATCH.
// Mensagens diferentes com a mesma chave de idempotência seguem as regras de
// replay de SubmitOperation, inclusive entre HTTP e SQS.
func (s *Service) HandleMessage(ctx context.Context, msg InboxMessage, in wagering.RequestInput, trace event.Trace) (MessageResult, error) {
	ctx = logctx.With(ctx, logctx.MessageID, msg.MessageID, logctx.ProviderID, in.ProviderID, logctx.WalletID, in.WalletID)
	start := time.Now()
	res, err := s.handleMessage(ctx, msg, in, trace)
	if err == nil && res.Duplicate {
		s.metrics.Duplicate(ChannelSQS, DuplicateInboxRedelivery)
		s.metrics.OperationDuration(ChannelSQS, kindLabel(in.Kind), DuplicateInboxRedelivery, time.Since(start))
		return res, nil
	}
	s.recordOperation(ChannelSQS, in.Kind, res.Operation, err, time.Since(start))
	return res, err
}

// recordOperation conta a operação concluída de fato (replays só como
// duplicata) e mede a duração; erros entram só no histograma, com o código.
func (s *Service) recordOperation(ch Channel, kind string, res OperationResult, err error, d time.Duration) {
	label := kindLabel(kind)
	switch {
	case err != nil:
		s.metrics.OperationDuration(ch, label, errorCode(err), d)
	case res.Replay:
		s.metrics.Duplicate(ch, DuplicateIdempotentReplay)
		s.metrics.OperationDuration(ch, label, string(res.Transaction.Status()), d)
	default:
		t := res.Transaction
		s.metrics.OperationCompleted(ch, label, string(t.Status()), string(t.FailureCode()))
		s.metrics.OperationDuration(ch, label, string(t.Status()), d)
	}
}

// retryDo aplica a política de retry e conta cada nova tentativa pelo motivo
// do erro anterior.
func (s *Service) retryDo(ctx context.Context, fn func() error) error {
	var last error
	attempt := 0
	return s.retry.Do(ctx, func() error {
		if attempt > 0 {
			s.metrics.Retry(RetryComponentUseCase, retryReason(last))
		}
		attempt++
		last = fn()
		return last
	})
}

func retryReason(err error) string {
	switch {
	case errors.Is(err, ErrConcurrentUpdate):
		return RetryReasonStaleVersion
	case errors.Is(err, ErrDuplicate):
		return RetryReasonDuplicate
	default:
		return RetryReasonTransient
	}
}

// errorCode é o código estável de um erro, o mesmo devolvido pela API.
func errorCode(err error) string {
	if code, ok := domain.CodeOf(err); ok {
		return string(code)
	}
	if IsRetryable(err) || errors.Is(err, ErrDuplicate) || errors.Is(err, context.DeadlineExceeded) {
		return outcomeUnavailable
	}
	return outcomeInternal
}

// kindLabel limita o label kind aos tipos conhecidos, evitando cardinalidade
// aberta com entradas inválidas.
func kindLabel(kind string) string {
	if k, err := wagering.ParseKind(kind); err == nil {
		return string(k)
	}
	return kindUnknown
}
