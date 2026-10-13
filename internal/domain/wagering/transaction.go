// Package wagering contém a entidade WagerTransaction, a requisição externa
// validada e as regras que transformam uma requisição em resultado financeiro.
package wagering

import (
	"time"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/money"
)

// Kind é o tipo de uma transação de wagering.
type Kind string

const (
	Opening  Kind = "OPENING"
	Bet      Kind = "BET"
	Win      Kind = "WIN"
	Loss     Kind = "LOSS"
	Refund   Kind = "REFUND"
	Rollback Kind = "ROLLBACK"
)

// ParseExternalKind aceita os tipos que um provedor pode enviar. OPENING é
// interno e rejeitado com KIND_NOT_ALLOWED, como qualquer tipo desconhecido.
func ParseExternalKind(s string) (Kind, error) {
	switch k := Kind(s); k {
	case Bet, Win, Loss, Refund, Rollback:
		return k, nil
	default:
		return "", domain.Errorf(domain.ErrKindNotAllowed, "kind %q is not allowed", clip(s))
	}
}

// ParseKind valida um tipo persistido.
func ParseKind(s string) (Kind, error) {
	switch k := Kind(s); k {
	case Opening, Bet, Win, Loss, Refund, Rollback:
		return k, nil
	default:
		return "", domain.Errorf(domain.ErrInconsistentState, "unknown kind %q", clip(s))
	}
}

// IsReversal informa se o tipo desfaz uma transação referenciada.
func (k Kind) IsReversal() bool { return k == Refund || k == Rollback }

// Status é o estado de uma transação de wagering.
type Status string

const (
	Pending          Status = "PENDING"
	PendingReference Status = "PENDING_REFERENCE"
	Processed        Status = "PROCESSED"
	Rejected         Status = "REJECTED"
	Failed           Status = "FAILED"
)

// ParseStatus valida um estado persistido.
func ParseStatus(s string) (Status, error) {
	switch st := Status(s); st {
	case Pending, PendingReference, Processed, Rejected, Failed:
		return st, nil
	default:
		return "", domain.Errorf(domain.ErrInconsistentState, "unknown status %q", clip(s))
	}
}

// IsTerminal informa se nenhuma outra transição é permitida.
func (s Status) IsTerminal() bool { return s == Processed || s == Rejected || s == Failed }

// transitions é a máquina de estados completa:
//
//	PENDING           -> PROCESSED | REJECTED | FAILED | PENDING_REFERENCE
//	PENDING_REFERENCE -> PROCESSED | REJECTED | FAILED
//	PROCESSED, REJECTED, FAILED: terminais
var transitions = map[Status][]Status{
	Pending:          {Processed, Rejected, Failed, PendingReference},
	PendingReference: {Processed, Rejected, Failed},
}

// CanTransition informa se a transição from -> to é permitida.
func CanTransition(from, to Status) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// PayloadHash é o hash de idempotência versionado dos campos de negócio.
type PayloadHash string

// WagerTransaction registra uma operação sobre uma carteira. Operações
// externas carregam os metadados do provedor; o OPENING interno não carrega
// nenhum.
type WagerTransaction struct {
	id       domain.TransactionID
	kind     Kind
	walletID domain.WalletID
	playerID domain.PlayerID
	money    money.Money
	status   Status

	providerID          string
	externalID          string
	idempotencyKey      string
	payloadHash         PayloadHash
	roundID             string
	gameID              string
	referenceExternalID string

	referenceTxID domain.TransactionID
	failureCode   domain.Code
	resultBalance money.Money

	createdAt time.Time
	updatedAt time.Time
}

// Snapshot carrega todos os campos, para reconstrução a partir da
// persistência. ResultBalance é o Money zero quando ausente.
type Snapshot struct {
	ID                             domain.TransactionID
	Kind                           Kind
	WalletID                       domain.WalletID
	PlayerID                       domain.PlayerID
	Money                          money.Money
	Status                         Status
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PayloadHash                    PayloadHash
	RoundID                        string
	GameID                         string
	ReferenceExternalTransactionID string
	ReferenceTransactionID         domain.TransactionID
	FailureCode                    domain.Code
	ResultBalance                  money.Money
	CreatedAt                      time.Time
	UpdatedAt                      time.Time
}

// NewExternal registra uma requisição validada do provedor como PENDING.
func NewExternal(req Request, now time.Time) (*WagerTransaction, error) {
	if req.hash == "" {
		return nil, domain.Errorf(domain.ErrInvalidRequest, "request was not built with NewRequest")
	}
	if now.IsZero() {
		return nil, domain.Errorf(domain.ErrInconsistentState, "transaction requires creation time")
	}
	id, err := domain.NewID[domain.TransactionID]()
	if err != nil {
		return nil, err
	}
	now = domain.Timestamp(now)
	return &WagerTransaction{
		id:                  id,
		kind:                req.kind,
		walletID:            req.walletID,
		playerID:            req.playerID,
		money:               req.money,
		status:              Pending,
		providerID:          req.providerID,
		externalID:          req.externalID,
		idempotencyKey:      req.idempotencyKey,
		payloadHash:         req.hash,
		roundID:             req.roundID,
		gameID:              req.gameID,
		referenceExternalID: req.referenceExternalID,
		createdAt:           now,
		updatedAt:           now,
	}, nil
}

// newOpening cria a transação interna OPENING como PENDING; quem chama a
// conclui na mesma operação que abre a carteira.
func newOpening(id domain.TransactionID, walletID domain.WalletID, playerID domain.PlayerID, amount money.Money, now time.Time) (*WagerTransaction, error) {
	if id.IsZero() || walletID.IsZero() || playerID.IsZero() {
		return nil, domain.Errorf(domain.ErrInconsistentState, "opening requires ids")
	}
	if !amount.IsValid() || !amount.IsPositive() {
		return nil, domain.Errorf(domain.ErrInconsistentState, "opening amount must be positive")
	}
	now = domain.Timestamp(now)
	return &WagerTransaction{
		id:        id,
		kind:      Opening,
		walletID:  walletID,
		playerID:  playerID,
		money:     amount,
		status:    Pending,
		createdAt: now,
		updatedAt: now,
	}, nil
}

// Rehydrate reconstrói uma transação persistida sem executar nenhuma transição.
func Rehydrate(s Snapshot) (*WagerTransaction, error) {
	t := &WagerTransaction{
		id:                  s.ID,
		kind:                s.Kind,
		walletID:            s.WalletID,
		playerID:            s.PlayerID,
		money:               s.Money,
		status:              s.Status,
		providerID:          s.ProviderID,
		externalID:          s.ExternalTransactionID,
		idempotencyKey:      s.IdempotencyKey,
		payloadHash:         s.PayloadHash,
		roundID:             s.RoundID,
		gameID:              s.GameID,
		referenceExternalID: s.ReferenceExternalTransactionID,
		referenceTxID:       s.ReferenceTransactionID,
		failureCode:         s.FailureCode,
		resultBalance:       s.ResultBalance,
		createdAt:           s.CreatedAt,
		updatedAt:           s.UpdatedAt,
	}
	if err := t.validate(); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *WagerTransaction) validate() error {
	bad := func(reason string) error {
		return domain.Errorf(domain.ErrInconsistentState, "transaction %s: %s", t.id, reason)
	}
	if t.id.IsZero() || t.walletID.IsZero() || t.playerID.IsZero() {
		return bad("missing identifiers")
	}
	if _, err := ParseKind(string(t.kind)); err != nil {
		return err
	}
	if _, err := ParseStatus(string(t.status)); err != nil {
		return err
	}
	if !t.money.IsValid() || t.money.IsNegative() {
		return bad("invalid money")
	}
	if t.createdAt.IsZero() || t.updatedAt.Before(t.createdAt) {
		return bad("invalid timestamps")
	}

	if t.kind == Opening {
		if t.providerID != "" || t.externalID != "" || t.idempotencyKey != "" || t.payloadHash != "" ||
			t.roundID != "" || t.gameID != "" || t.referenceExternalID != "" || !t.referenceTxID.IsZero() {
			return bad("opening must not carry external metadata")
		}
		if t.status != Processed || !t.money.IsPositive() {
			return bad("opening must be processed with a positive amount")
		}
	} else {
		if t.providerID == "" || t.externalID == "" || t.idempotencyKey == "" || t.payloadHash == "" ||
			t.roundID == "" || t.gameID == "" {
			return bad("missing external metadata")
		}
		if t.kind == Loss && !t.money.IsZero() {
			return bad("loss amount must be zero")
		}
		if t.kind != Loss && !t.money.IsPositive() {
			return bad("amount must be positive")
		}
		if t.kind.IsReversal() && t.referenceExternalID == "" {
			return bad("reversal without reference")
		}
		if (t.kind == Bet || t.kind == Loss) && t.referenceExternalID != "" {
			return bad("unexpected reference")
		}
	}

	switch t.status {
	case Pending, PendingReference:
		if t.failureCode != "" || t.resultBalance.IsValid() || !t.referenceTxID.IsZero() {
			return bad("pending transaction carries a result")
		}
		if t.status == PendingReference && !t.kind.IsReversal() {
			return bad("only reversals wait for a reference")
		}
	case Processed:
		if t.failureCode != "" || !t.resultBalance.IsValid() || t.resultBalance.IsNegative() ||
			t.resultBalance.Currency() != t.money.Currency() {
			return bad("processed transaction requires a non-negative result balance")
		}
		if t.kind.IsReversal() && t.referenceTxID.IsZero() {
			return bad("processed reversal requires the resolved reference")
		}
		if (t.kind == Bet || t.kind == Loss || t.kind == Opening) && !t.referenceTxID.IsZero() {
			return bad("unexpected resolved reference")
		}
	case Rejected:
		if !isCode(t.failureCode, domain.KindRejection) {
			return bad("rejected transaction requires a rejection code")
		}
	case Failed:
		if !isCode(t.failureCode, domain.KindInvariant) {
			return bad("failed transaction requires an invariant code")
		}
	}
	return nil
}

func isCode(code domain.Code, kind domain.ErrorKind) bool {
	e, ok := domain.LookupCode(code)
	return ok && e.Kind == kind
}

// MarkPendingReference registra que a transação referenciada ainda não está
// disponível. Somente reversões esperam.
func (t *WagerTransaction) MarkPendingReference(now time.Time) error {
	if !t.kind.IsReversal() {
		return domain.Errorf(domain.ErrInvalidTransition, "%s cannot wait for a reference", t.kind)
	}
	return t.transition(PendingReference, now)
}

// MarkProcessed conclui a transação. balance é o saldo da carteira observado
// logo após o processamento, devolvido em todo replay; ref é a transação
// referenciada já resolvida (obrigatória em reversões, opcional em WIN,
// proibida nos demais tipos).
func (t *WagerTransaction) MarkProcessed(balance money.Money, ref domain.TransactionID, now time.Time) error {
	if !balance.IsValid() || balance.IsNegative() || balance.Currency() != t.money.Currency() {
		return domain.Errorf(domain.ErrInconsistentState, "invalid result balance")
	}
	switch {
	case t.kind.IsReversal() && ref.IsZero():
		return domain.Errorf(domain.ErrInconsistentState, "reversal requires the resolved reference")
	case (t.kind == Bet || t.kind == Loss || t.kind == Opening) && !ref.IsZero():
		return domain.Errorf(domain.ErrInconsistentState, "%s does not take a reference", t.kind)
	}
	if err := t.transition(Processed, now); err != nil {
		return err
	}
	t.resultBalance = balance
	t.referenceTxID = ref
	return nil
}

// MarkRejected registra uma rejeição de negócio definitiva. observed é o
// saldo da carteira no momento da rejeição, ou o Money zero quando desconhecido.
func (t *WagerTransaction) MarkRejected(code domain.Code, observed money.Money, now time.Time) error {
	if !isCode(code, domain.KindRejection) {
		return domain.Errorf(domain.ErrInconsistentState, "%q is not a rejection code", code)
	}
	if observed.IsValid() && (observed.IsNegative() || observed.Currency() != t.money.Currency()) {
		return domain.Errorf(domain.ErrInconsistentState, "invalid observed balance")
	}
	if err := t.transition(Rejected, now); err != nil {
		return err
	}
	t.failureCode = code
	t.resultBalance = observed
	return nil
}

// MarkFailed registra uma falha permanente para auditoria.
func (t *WagerTransaction) MarkFailed(code domain.Code, now time.Time) error {
	if !isCode(code, domain.KindInvariant) {
		return domain.Errorf(domain.ErrInconsistentState, "%q is not a permanent failure code", code)
	}
	if err := t.transition(Failed, now); err != nil {
		return err
	}
	t.failureCode = code
	return nil
}

func (t *WagerTransaction) transition(to Status, now time.Time) error {
	if !CanTransition(t.status, to) {
		return domain.Errorf(domain.ErrInvalidTransition, "transaction %s: %s -> %s", t.id, t.status, to)
	}
	if now.IsZero() {
		return domain.Errorf(domain.ErrInconsistentState, "transition requires a timestamp")
	}
	t.status = to
	if now = domain.Timestamp(now); now.After(t.updatedAt) {
		t.updatedAt = now
	}
	return nil
}

// CheckReplay compara uma nova requisição com esta transação persistida,
// encontrada por (providerId, idempotencyKey) ou por (providerId,
// externalTransactionId). Resultado nil significa replay idempotente: o
// resultado persistido deve ser devolvido sem reaplicar a operação.
func (t *WagerTransaction) CheckReplay(req Request) error {
	if t.kind == Opening || t.providerID != req.providerID {
		return domain.Errorf(domain.ErrInconsistentState, "replay lookup crossed provider or internal scope")
	}
	if t.idempotencyKey != req.idempotencyKey {
		return domain.Errorf(domain.ErrExternalTxConflict,
			"external transaction %q was registered under another idempotency key", clip(req.externalID))
	}
	if t.payloadHash != req.hash {
		return domain.Errorf(domain.ErrIdempotencyConflict, "idempotency key reused with a different payload")
	}
	return nil
}

func (t *WagerTransaction) ID() domain.TransactionID      { return t.id }
func (t *WagerTransaction) Kind() Kind                    { return t.kind }
func (t *WagerTransaction) WalletID() domain.WalletID     { return t.walletID }
func (t *WagerTransaction) PlayerID() domain.PlayerID     { return t.playerID }
func (t *WagerTransaction) Money() money.Money            { return t.money }
func (t *WagerTransaction) Status() Status                { return t.status }
func (t *WagerTransaction) ProviderID() string            { return t.providerID }
func (t *WagerTransaction) ExternalTransactionID() string { return t.externalID }
func (t *WagerTransaction) IdempotencyKey() string        { return t.idempotencyKey }
func (t *WagerTransaction) PayloadHash() PayloadHash      { return t.payloadHash }
func (t *WagerTransaction) RoundID() string               { return t.roundID }
func (t *WagerTransaction) GameID() string                { return t.gameID }
func (t *WagerTransaction) FailureCode() domain.Code      { return t.failureCode }
func (t *WagerTransaction) CreatedAt() time.Time          { return t.createdAt }
func (t *WagerTransaction) UpdatedAt() time.Time          { return t.updatedAt }
func (t *WagerTransaction) IsInternal() bool              { return t.kind == Opening }
func (t *WagerTransaction) ReferenceExternalTransactionID() string {
	return t.referenceExternalID
}

// ReferenceTransactionID devolve a referência resolvida, se houver.
func (t *WagerTransaction) ReferenceTransactionID() (domain.TransactionID, bool) {
	return t.referenceTxID, !t.referenceTxID.IsZero()
}

// ResultBalance devolve o saldo observado quando a transação foi concluída.
func (t *WagerTransaction) ResultBalance() (money.Money, bool) {
	return t.resultBalance, t.resultBalance.IsValid()
}

// Snapshot exporta todos os campos para persistência.
func (t *WagerTransaction) Snapshot() Snapshot {
	return Snapshot{
		ID:                             t.id,
		Kind:                           t.kind,
		WalletID:                       t.walletID,
		PlayerID:                       t.playerID,
		Money:                          t.money,
		Status:                         t.status,
		ProviderID:                     t.providerID,
		ExternalTransactionID:          t.externalID,
		IdempotencyKey:                 t.idempotencyKey,
		PayloadHash:                    t.payloadHash,
		RoundID:                        t.roundID,
		GameID:                         t.gameID,
		ReferenceExternalTransactionID: t.referenceExternalID,
		ReferenceTransactionID:         t.referenceTxID,
		FailureCode:                    t.failureCode,
		ResultBalance:                  t.resultBalance,
		CreatedAt:                      t.createdAt,
		UpdatedAt:                      t.updatedAt,
	}
}

func clip(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}
