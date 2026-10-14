package sqs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/wagering"
)

// MessageType é o único tipo aceito na fila de entrada.
const MessageType = "WagerTransactionRequested"

// Envelope é a mensagem de entrada, no formato do enunciado.
type Envelope struct {
	MessageID  string       `json:"messageId"`
	Type       string       `json:"type"`
	OccurredAt string       `json:"occurredAt"`
	Data       *MessageData `json:"data"`
}

// MessageData é a operação; os mesmos campos do HTTP, com a chave de
// idempotência no corpo.
type MessageData struct {
	ProviderID                     string       `json:"providerId"`
	ExternalTransactionID          string       `json:"externalTransactionId"`
	IdempotencyKey                 string       `json:"idempotencyKey"`
	PlayerID                       string       `json:"playerId"`
	WalletID                       string       `json:"walletId"`
	RoundID                        string       `json:"roundId"`
	GameID                         string       `json:"gameId"`
	Kind                           string       `json:"kind"`
	Money                          MessageMoney `json:"money"`
	ReferenceExternalTransactionID string       `json:"referenceExternalTransactionId,omitempty"`
}

// MessageMoney aceita só {"amount":"<string>","currency":"<string>"}: um
// valor numérico (que passaria por ponto flutuante) é rejeitado.
type MessageMoney struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// UnmarshalJSON rejeita amount numérico e campos desconhecidos.
func (m *MessageMoney) UnmarshalJSON(b []byte) error {
	var raw struct {
		Amount   json.RawMessage `json:"amount"`
		Currency string          `json:"currency"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return domain.Errorf(domain.ErrInvalidAmount, "money must be an object with string amount and currency")
	}
	if len(raw.Amount) > 0 {
		if raw.Amount[0] != '"' {
			return domain.Errorf(domain.ErrInvalidAmount, "amount must be a decimal string")
		}
		if err := json.Unmarshal(raw.Amount, &m.Amount); err != nil {
			return domain.Errorf(domain.ErrInvalidAmount, "amount must be a decimal string")
		}
	}
	m.Currency = raw.Currency
	return nil
}

// codeInvalidMessage marca, na DLQ, mensagens que não seguem o contrato do
// envelope (JSON malformado, tipo desconhecido, campos ausentes).
const codeInvalidMessage domain.Code = "INVALID_MESSAGE"

// ParseEnvelope lê o corpo de forma estrita: um único objeto, sem campos
// desconhecidos, com messageId, type, occurredAt (RFC 3339) e data.
func ParseEnvelope(body string) (Envelope, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(body)))
	dec.DisallowUnknownFields()
	var env Envelope
	if err := dec.Decode(&env); err != nil {
		var de *domain.Error
		if errors.As(err, &de) {
			return Envelope{}, de
		}
		return Envelope{}, invalidMessage("malformed envelope: %v", err)
	}
	if dec.More() {
		return Envelope{}, invalidMessage("body must contain a single JSON object")
	}
	if err := domain.ValidateTextID("messageId", env.MessageID); err != nil {
		return Envelope{}, invalidMessage("invalid messageId")
	}
	if env.Type != MessageType {
		return Envelope{}, invalidMessage("unsupported message type %q", clip(env.Type))
	}
	if _, err := time.Parse(time.RFC3339Nano, env.OccurredAt); err != nil {
		return Envelope{}, invalidMessage("occurredAt must be RFC 3339")
	}
	if env.Data == nil {
		return Envelope{}, invalidMessage("data is required")
	}
	return env, nil
}

// RequestInput converte a mensagem na mesma entrada usada pelo HTTP.
func (e Envelope) RequestInput() wagering.RequestInput {
	d := e.Data
	return wagering.RequestInput{
		ProviderID: d.ProviderID, ExternalTransactionID: d.ExternalTransactionID, IdempotencyKey: d.IdempotencyKey,
		PlayerID: d.PlayerID, WalletID: d.WalletID, RoundID: d.RoundID, GameID: d.GameID, Kind: d.Kind,
		Amount: d.Money.Amount, Currency: d.Money.Currency,
		ReferenceExternalTransactionID: d.ReferenceExternalTransactionID,
	}
}

// BodyHash é o hash registrado na inbox: SHA-256 do corpo recebido. Uma
// reentrega do SQS traz o corpo idêntico; o mesmo messageId com corpo
// diferente resulta em MESSAGE_HASH_MISMATCH.
func BodyHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "v1:" + hex.EncodeToString(sum[:])
}

func invalidMessage(format string, args ...any) *domain.Error {
	return &domain.Error{Kind: domain.KindValidation, Code: codeInvalidMessage, Message: fmt.Sprintf(format, args...)}
}

func clip(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}
