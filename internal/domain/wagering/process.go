package wagering

import (
	"errors"
	"time"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/event"
	"github.com/gbrpscal/wallet/internal/domain/money"
	"github.com/gbrpscal/wallet/internal/domain/wallet"
)

// Reference é o que quem chama encontrou para a referência da transação,
// carregado na mesma transação SQL por (providerId, referenceExternalTransactionId).
type Reference struct {
	// Transaction é nil quando não existe transação com esse id externo.
	Transaction *WagerTransaction
	// Reversed é true quando uma reversão PROCESSED já aponta para ela.
	Reversed bool
}

// Outcome é tudo o que um passo de processamento produziu. Quem chama persiste
// transação, carteira, lançamento e eventos em um único commit.
type Outcome struct {
	Transaction *WagerTransaction
	// Entry é nil quando o saldo não mudou.
	Entry  *wallet.LedgerEntry
	Events []event.Event
	// Waiting é true quando a transação continua PENDING_REFERENCE sem
	// nenhuma mudança; não há o que persistir além do controle de retry.
	Waiting bool
}

// Process aplica as regras do tipo da transação à carteira. Rejeições de
// negócio não são erros: voltam como Outcome com a transação REJECTED. Um erro
// retornado significa que nada pode ser persistido.
func Process(tx *WagerTransaction, w *wallet.Wallet, ref Reference, trace event.Trace, now time.Time) (Outcome, error) {
	if tx == nil || w == nil {
		return Outcome{}, domain.Errorf(domain.ErrInconsistentState, "process requires transaction and wallet")
	}
	if tx.status != Pending && tx.status != PendingReference {
		return Outcome{}, domain.Errorf(domain.ErrInvalidTransition, "transaction %s is %s", tx.id, tx.status)
	}
	if tx.kind == Opening {
		return Outcome{}, domain.Errorf(domain.ErrInconsistentState, "opening is completed by OpenWallet")
	}
	if w.ID() != tx.walletID {
		return Outcome{}, domain.Errorf(domain.ErrInconsistentState, "wallet %s does not match transaction wallet %s", w.ID(), tx.walletID)
	}
	if err := trace.Validate(); err != nil {
		return Outcome{}, err
	}

	p := processor{tx: tx, w: w, trace: trace, now: now}
	if w.PlayerID() != tx.playerID {
		return p.reject(domain.CodeWalletPlayerMismatch)
	}
	if w.Currency() != tx.money.Currency() {
		return p.reject(domain.CodeCurrencyMismatch)
	}

	switch tx.kind {
	case Bet:
		return p.move(wallet.Debit, domain.TransactionID{}, domain.CodeInsufficientFunds)
	case Loss:
		return p.processed(nil, domain.TransactionID{})
	case Win:
		return p.win(ref)
	case Refund, Rollback:
		return p.reversal(ref)
	default:
		return Outcome{}, domain.Errorf(domain.ErrInconsistentState, "unknown kind %q", tx.kind)
	}
}

// Expire finaliza uma transação cuja referência não ficou disponível dentro da
// política de retry. w pode ser nil quando a carteira não foi carregada; nesse
// caso o saldo observado não é registrado.
func Expire(tx *WagerTransaction, w *wallet.Wallet, trace event.Trace, now time.Time) (Outcome, error) {
	if tx == nil {
		return Outcome{}, domain.Errorf(domain.ErrInconsistentState, "expire requires a transaction")
	}
	if tx.status != PendingReference {
		return Outcome{}, domain.Errorf(domain.ErrInvalidTransition, "only %s transactions expire, %s is %s", PendingReference, tx.id, tx.status)
	}
	if err := trace.Validate(); err != nil {
		return Outcome{}, err
	}
	p := processor{tx: tx, w: w, trace: trace, now: now}
	return p.reject(domain.CodeReferenceNotFound)
}

type processor struct {
	tx    *WagerTransaction
	w     *wallet.Wallet
	trace event.Trace
	now   time.Time
}

func (p processor) win(ref Reference) (Outcome, error) {
	if ref.Transaction == nil {
		// O crédito do WIN não depende da aposta: uma referência opcional
		// desconhecida fica registrada e não é resolvida.
		return p.move(wallet.Credit, domain.TransactionID{}, "")
	}
	r := ref.Transaction
	if r.kind != Bet || !p.sameScope(r) {
		return p.reject(domain.CodeReferenceMismatch)
	}
	if r.status != Processed {
		return p.reject(domain.CodeReferenceNotProcessed)
	}
	return p.move(wallet.Credit, r.id, "")
}

// reversal implementa REFUND e ROLLBACK. As checagens estáticas vêm primeiro,
// para que uma requisição que nunca terá sucesso seja rejeitada sem esperar.
func (p processor) reversal(ref Reference) (Outcome, error) {
	r := ref.Transaction
	if r == nil {
		return p.wait()
	}
	if !reversible(p.tx.kind, r.kind) {
		return p.reject(domain.CodeReferenceNotReversible)
	}
	if !p.sameScope(r) {
		return p.reject(domain.CodeReferenceMismatch)
	}
	if !r.money.Equal(p.tx.money) {
		return p.reject(domain.CodeReferenceAmount)
	}
	switch r.status {
	case Pending, PendingReference:
		return p.wait()
	case Rejected, Failed:
		return p.reject(domain.CodeReferenceNotProcessed)
	}
	if ref.Reversed {
		return p.reject(domain.CodeAlreadyReversed)
	}
	// A reversão movimenta no sentido contrário ao da operação original.
	if r.kind == Bet {
		return p.move(wallet.Credit, r.id, "")
	}
	return p.move(wallet.Debit, r.id, domain.CodeReversalInsufficient)
}

// reversible: REFUND desfaz uma BET; ROLLBACK desfaz uma BET, WIN ou REFUND.
func reversible(kind, target Kind) bool {
	switch kind {
	case Refund:
		return target == Bet
	case Rollback:
		return target == Bet || target == Win || target == Refund
	default:
		return false
	}
}

// sameScope confere os campos que a operação e sua referência devem compartilhar.
func (p processor) sameScope(r *WagerTransaction) bool {
	return r.providerID == p.tx.providerID &&
		r.playerID == p.tx.playerID &&
		r.walletID == p.tx.walletID &&
		r.money.Currency() == p.tx.money.Currency() &&
		r.roundID == p.tx.roundID
}

// move debita ou credita a carteira. insufficient é o código de rejeição usado
// quando um débito deixaria o saldo negativo.
func (p processor) move(dir wallet.Direction, ref domain.TransactionID, insufficient domain.Code) (Outcome, error) {
	var entry wallet.LedgerEntry
	var err error
	if dir == wallet.Debit {
		entry, err = p.w.Debit(p.tx.id, p.tx.money, p.now)
	} else {
		entry, err = p.w.Credit(p.tx.id, p.tx.money, p.now)
	}
	if err != nil {
		if errors.Is(err, domain.ErrInsufficientFunds) && insufficient != "" {
			return p.reject(insufficient)
		}
		return Outcome{}, err
	}
	return p.processed(&entry, ref)
}

func (p processor) processed(entry *wallet.LedgerEntry, ref domain.TransactionID) (Outcome, error) {
	if err := p.tx.MarkProcessed(p.w.Balance(), ref, p.now); err != nil {
		return Outcome{}, err
	}
	data := event.WagerTransactionProcessedData{TransactionRef: transactionRef(p.tx), Balance: p.w.Balance()}
	if !ref.IsZero() {
		data.ReferenceTransactionID = ref.String()
	}
	processed, err := event.NewWagerTransactionProcessed(p.trace, p.now, data)
	if err != nil {
		return Outcome{}, err
	}
	events := []event.Event{processed}
	if entry != nil {
		changed, err := event.NewWalletBalanceChanged(p.trace, p.now, *entry)
		if err != nil {
			return Outcome{}, err
		}
		events = append(events, changed)
	}
	return Outcome{Transaction: p.tx, Entry: entry, Events: events}, nil
}

func (p processor) reject(code domain.Code) (Outcome, error) {
	// O saldo observado só é registrado quando está na moeda da operação
	// (o que não ocorre em CURRENCY_MISMATCH).
	var observed money.Money
	if p.w != nil && p.w.Currency() == p.tx.money.Currency() {
		observed = p.w.Balance()
	}
	if err := p.tx.MarkRejected(code, observed, p.now); err != nil {
		return Outcome{}, err
	}
	rejected, err := event.NewWagerTransactionRejected(p.trace, p.now, event.WagerTransactionRejectedData{
		TransactionRef:                 transactionRef(p.tx),
		ReferenceExternalTransactionID: p.tx.referenceExternalID,
		FailureCode:                    code,
	})
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Transaction: p.tx, Events: []event.Event{rejected}}, nil
}

// wait leva PENDING para PENDING_REFERENCE com o respectivo evento; uma
// transação que já está esperando fica como está, sem nada a persistir.
func (p processor) wait() (Outcome, error) {
	if p.tx.status == PendingReference {
		return Outcome{Transaction: p.tx, Waiting: true}, nil
	}
	if err := p.tx.MarkPendingReference(p.now); err != nil {
		return Outcome{}, err
	}
	pending, err := event.NewWagerTransactionPendingReference(p.trace, p.now, event.WagerTransactionPendingReferenceData{
		TransactionRef:                 transactionRef(p.tx),
		ReferenceExternalTransactionID: p.tx.referenceExternalID,
	})
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Transaction: p.tx, Events: []event.Event{pending}}, nil
}

func transactionRef(t *WagerTransaction) event.TransactionRef {
	return event.TransactionRef{
		TransactionID:         t.id,
		WalletID:              t.walletID,
		PlayerID:              t.playerID,
		Kind:                  string(t.kind),
		Money:                 t.money,
		ProviderID:            t.providerID,
		ExternalTransactionID: t.externalID,
		RoundID:               t.roundID,
		GameID:                t.gameID,
	}
}

// OpenOutcome é tudo o que a abertura de carteira produziu, confirmado junto.
type OpenOutcome struct {
	Wallet *wallet.Wallet
	// Opening, Entry e Events ficam vazios quando o saldo inicial é zero.
	Opening *WagerTransaction
	Entry   *wallet.LedgerEntry
	Events  []event.Event
}

// OpenWallet cria uma carteira. Saldo inicial positivo também produz a
// transação interna OPENING (PROCESSED), seu lançamento de crédito e os eventos
// WagerTransactionProcessed e WalletBalanceChanged. Saldo inicial zero não
// produz nenhum deles.
func OpenWallet(playerID domain.PlayerID, initial money.Money, trace event.Trace, now time.Time) (OpenOutcome, error) {
	if err := trace.Validate(); err != nil {
		return OpenOutcome{}, err
	}
	walletID, err := domain.NewID[domain.WalletID]()
	if err != nil {
		return OpenOutcome{}, err
	}
	var openingID domain.TransactionID
	if initial.IsValid() && initial.IsPositive() {
		if openingID, err = domain.NewID[domain.TransactionID](); err != nil {
			return OpenOutcome{}, err
		}
	}
	w, entry, err := wallet.Open(walletID, playerID, initial, openingID, now)
	if err != nil {
		return OpenOutcome{}, err
	}
	if entry == nil {
		return OpenOutcome{Wallet: w}, nil
	}

	opening, err := newOpening(openingID, walletID, playerID, initial, now)
	if err != nil {
		return OpenOutcome{}, err
	}
	p := processor{tx: opening, w: w, trace: trace, now: now}
	out, err := p.processed(entry, domain.TransactionID{})
	if err != nil {
		return OpenOutcome{}, err
	}
	return OpenOutcome{Wallet: w, Opening: opening, Entry: entry, Events: out.Events}, nil
}
