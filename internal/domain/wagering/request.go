package wagering

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/money"
)

// RequestInput é a operação bruta do provedor, idêntica para HTTP e SQS.
type RequestInput struct {
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PlayerID                       string
	WalletID                       string
	RoundID                        string
	GameID                         string
	Kind                           string
	Amount                         string
	Currency                       string
	ReferenceExternalTransactionID string
}

// Request é uma operação validada do provedor com seu hash de idempotência.
// Só pode ser construída por NewRequest.
type Request struct {
	providerID          string
	externalID          string
	idempotencyKey      string
	playerID            domain.PlayerID
	walletID            domain.WalletID
	roundID             string
	gameID              string
	kind                Kind
	money               money.Money
	referenceExternalID string
	hash                PayloadHash
}

// NewRequest valida a entrada. Todo erro é de validação corrigível
// (KindValidation): nada é persistido e a chave não é consumida.
func NewRequest(in RequestInput) (Request, error) {
	for _, f := range []struct{ name, value string }{
		{"providerId", in.ProviderID},
		{"externalTransactionId", in.ExternalTransactionID},
		{"idempotencyKey", in.IdempotencyKey},
		{"roundId", in.RoundID},
		{"gameId", in.GameID},
	} {
		if err := domain.ValidateTextID(f.name, f.value); err != nil {
			return Request{}, err
		}
	}
	playerID, err := domain.ParseID[domain.PlayerID](in.PlayerID)
	if err != nil {
		return Request{}, domain.Errorf(domain.ErrInvalidRequest, "playerId must be a uuid")
	}
	walletID, err := domain.ParseID[domain.WalletID](in.WalletID)
	if err != nil {
		return Request{}, domain.Errorf(domain.ErrInvalidRequest, "walletId must be a uuid")
	}
	kind, err := ParseExternalKind(in.Kind)
	if err != nil {
		return Request{}, err
	}
	m, err := money.Parse(in.Amount, in.Currency)
	if err != nil {
		return Request{}, err
	}
	if kind == Loss && !m.IsZero() {
		return Request{}, domain.Errorf(domain.ErrLossAmountMustBeZero, "LOSS amount must be 0.00")
	}
	if kind != Loss && !m.IsPositive() {
		return Request{}, domain.Errorf(domain.ErrAmountMustBePositive, "%s amount must be greater than zero", kind)
	}

	ref := in.ReferenceExternalTransactionID
	switch kind {
	case Refund, Rollback:
		if ref == "" {
			return Request{}, domain.Errorf(domain.ErrReferenceRequired, "%s requires referenceExternalTransactionId", kind)
		}
	case Bet, Loss:
		if ref != "" {
			return Request{}, domain.Errorf(domain.ErrInvalidRequest, "%s does not accept referenceExternalTransactionId", kind)
		}
	}
	if ref != "" {
		if err := domain.ValidateTextID("referenceExternalTransactionId", ref); err != nil {
			return Request{}, err
		}
		if ref == in.ExternalTransactionID {
			return Request{}, domain.Errorf(domain.ErrInvalidRequest, "a transaction cannot reference itself")
		}
	}

	r := Request{
		providerID:          in.ProviderID,
		externalID:          in.ExternalTransactionID,
		idempotencyKey:      in.IdempotencyKey,
		playerID:            playerID,
		walletID:            walletID,
		roundID:             in.RoundID,
		gameID:              in.GameID,
		kind:                kind,
		money:               m,
		referenceExternalID: ref,
	}
	r.hash, err = r.computeHash()
	if err != nil {
		return Request{}, err
	}
	return r, nil
}

// hashVersion prefixa todo hash, para que o conjunto de campos possa evoluir
// sem ambiguidade com os hashes já persistidos.
const hashVersion = "v1:"

// computeHash é SHA-256 sobre o JSON canônico dos campos de negócio: chaves
// ordenadas (encoding/json ordena as chaves de map), sem espaços
// insignificantes, UTF-8 e sem escape de HTML. UUIDs na forma canônica
// minúscula e o valor na forma estrita de duas casas. A chave de idempotência
// e os metadados de transporte ficam de fora; referenceExternalTransactionId é
// omitido quando vazio.
func (r Request) computeHash() (PayloadHash, error) {
	fields := map[string]any{
		"providerId":            r.providerID,
		"externalTransactionId": r.externalID,
		"playerId":              r.playerID.String(),
		"walletId":              r.walletID.String(),
		"roundId":               r.roundID,
		"gameId":                r.gameID,
		"kind":                  string(r.kind),
		"money": map[string]string{
			"amount":   r.money.Amount(),
			"currency": r.money.Currency().String(),
		},
	}
	if r.referenceExternalID != "" {
		fields["referenceExternalTransactionId"] = r.referenceExternalID
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(fields); err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return PayloadHash(hashVersion + hex.EncodeToString(sum[:])), nil
}

func (r Request) ProviderID() string            { return r.providerID }
func (r Request) ExternalTransactionID() string { return r.externalID }
func (r Request) IdempotencyKey() string        { return r.idempotencyKey }
func (r Request) PlayerID() domain.PlayerID     { return r.playerID }
func (r Request) WalletID() domain.WalletID     { return r.walletID }
func (r Request) RoundID() string               { return r.roundID }
func (r Request) GameID() string                { return r.gameID }
func (r Request) Kind() Kind                    { return r.kind }
func (r Request) Money() money.Money            { return r.money }
func (r Request) Hash() PayloadHash             { return r.hash }
func (r Request) ReferenceExternalTransactionID() string {
	return r.referenceExternalID
}
