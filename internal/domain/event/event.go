// Package event define os eventos de integração gravados na outbox. Cada
// evento concreto fixa seu tipo e sua versão no construtor, e o payload é um
// snapshot imutável tirado no momento da criação.
package event

import (
	"encoding/json"
	"time"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/money"
	"github.com/gbrpscal/wallet/internal/domain/wallet"
)

// Tipos de evento.
const (
	TypeWagerTransactionProcessed        = "WagerTransactionProcessed"
	TypeWagerTransactionRejected         = "WagerTransactionRejected"
	TypeWalletBalanceChanged             = "WalletBalanceChanged"
	TypeWagerTransactionPendingReference = "WagerTransactionPendingReference"
)

// TimeLayout é RFC 3339 em UTC com precisão fixa de microssegundos.
const TimeLayout = "2006-01-02T15:04:05.000000Z07:00"

// Trace liga um evento à requisição ou mensagem que o causou.
type Trace struct {
	CorrelationID string
	CausationID   string
}

// Validate exige o correlation id; o causation id é opcional.
func (t Trace) Validate() error {
	if err := domain.ValidateTextID("correlationId", t.CorrelationID); err != nil {
		return err
	}
	if t.CausationID != "" {
		return domain.ValidateTextID("causationId", t.CausationID)
	}
	return nil
}

// Event é implementado por todo evento concreto.
type Event interface {
	ID() domain.EventID
	Type() string
	Version() int
	AggregateID() string
	CorrelationID() string
	CausationID() string
	OccurredAt() time.Time
	Data() any
}

type header struct {
	id            domain.EventID
	eventType     string
	version       int
	aggregateID   string
	correlationID string
	causationID   string
	occurredAt    time.Time
}

func newHeader(eventType string, version int, aggregateID string, trace Trace, now time.Time) (header, error) {
	if err := trace.Validate(); err != nil {
		return header{}, err
	}
	if aggregateID == "" || now.IsZero() {
		return header{}, domain.Errorf(domain.ErrInconsistentState, "event requires aggregate and time")
	}
	id, err := domain.NewID[domain.EventID]()
	if err != nil {
		return header{}, err
	}
	return header{
		id:            id,
		eventType:     eventType,
		version:       version,
		aggregateID:   aggregateID,
		correlationID: trace.CorrelationID,
		causationID:   trace.CausationID,
		occurredAt:    domain.Timestamp(now),
	}, nil
}

func (h header) ID() domain.EventID    { return h.id }
func (h header) Type() string          { return h.eventType }
func (h header) Version() int          { return h.version }
func (h header) AggregateID() string   { return h.aggregateID }
func (h header) CorrelationID() string { return h.correlationID }
func (h header) CausationID() string   { return h.causationID }
func (h header) OccurredAt() time.Time { return h.occurredAt }

// envelope é o formato publicado para os consumidores.
type envelope struct {
	EventID       string `json:"eventId"`
	EventType     string `json:"eventType"`
	AggregateID   string `json:"aggregateId"`
	CorrelationID string `json:"correlationId"`
	CausationID   string `json:"causationId,omitempty"`
	OccurredAt    string `json:"occurredAt"`
	Version       int    `json:"version"`
	Data          any    `json:"data"`
}

// Marshal serializa o envelope do evento. O resultado é o que a outbox
// armazena e o que é publicado, sem alteração, em toda tentativa.
func Marshal(e Event) ([]byte, error) {
	return json.Marshal(envelope{
		EventID:       e.ID().String(),
		EventType:     e.Type(),
		AggregateID:   e.AggregateID(),
		CorrelationID: e.CorrelationID(),
		CausationID:   e.CausationID(),
		OccurredAt:    e.OccurredAt().UTC().Format(TimeLayout),
		Version:       e.Version(),
		Data:          e.Data(),
	})
}

// TransactionRef identifica a transação de wagering de que o evento trata. Os
// campos externos ficam vazios, e são omitidos, na transação interna OPENING.
type TransactionRef struct {
	TransactionID         domain.TransactionID `json:"transactionId"`
	WalletID              domain.WalletID      `json:"walletId"`
	PlayerID              domain.PlayerID      `json:"playerId"`
	Kind                  string               `json:"kind"`
	Money                 money.Money          `json:"money"`
	ProviderID            string               `json:"providerId,omitempty"`
	ExternalTransactionID string               `json:"externalTransactionId,omitempty"`
	RoundID               string               `json:"roundId,omitempty"`
	GameID                string               `json:"gameId,omitempty"`
}

// WagerTransactionProcessedData é o payload de WagerTransactionProcessed.
type WagerTransactionProcessedData struct {
	TransactionRef
	ReferenceTransactionID string      `json:"referenceTransactionId,omitempty"`
	Balance                money.Money `json:"balance"`
}

// WagerTransactionProcessed é emitido em toda conclusão bem-sucedida,
// incluindo LOSS e o OPENING interno.
type WagerTransactionProcessed struct {
	header
	data WagerTransactionProcessedData
}

// NewWagerTransactionProcessed constrói a versão 1 do evento.
func NewWagerTransactionProcessed(trace Trace, now time.Time, d WagerTransactionProcessedData) (WagerTransactionProcessed, error) {
	h, err := newHeader(TypeWagerTransactionProcessed, 1, d.TransactionID.String(), trace, now)
	if err != nil {
		return WagerTransactionProcessed{}, err
	}
	return WagerTransactionProcessed{header: h, data: d}, nil
}

// Data devolve o payload tipado.
func (e WagerTransactionProcessed) Data() any { return e.data }

// Payload devolve o payload tipado.
func (e WagerTransactionProcessed) Payload() WagerTransactionProcessedData { return e.data }

// WagerTransactionRejectedData é o payload de WagerTransactionRejected.
type WagerTransactionRejectedData struct {
	TransactionRef
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId,omitempty"`
	FailureCode                    domain.Code `json:"failureCode"`
}

// WagerTransactionRejected é emitido em uma rejeição de negócio definitiva.
type WagerTransactionRejected struct {
	header
	data WagerTransactionRejectedData
}

// NewWagerTransactionRejected constrói a versão 1 do evento.
func NewWagerTransactionRejected(trace Trace, now time.Time, d WagerTransactionRejectedData) (WagerTransactionRejected, error) {
	h, err := newHeader(TypeWagerTransactionRejected, 1, d.TransactionID.String(), trace, now)
	if err != nil {
		return WagerTransactionRejected{}, err
	}
	return WagerTransactionRejected{header: h, data: d}, nil
}

// Data devolve o payload tipado.
func (e WagerTransactionRejected) Data() any { return e.data }

// Payload devolve o payload tipado.
func (e WagerTransactionRejected) Payload() WagerTransactionRejectedData { return e.data }

// WagerTransactionPendingReferenceData é o payload de
// WagerTransactionPendingReference.
type WagerTransactionPendingReferenceData struct {
	TransactionRef
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId"`
}

// WagerTransactionPendingReference é emitido quando uma operação passa a
// esperar pela sua referência.
type WagerTransactionPendingReference struct {
	header
	data WagerTransactionPendingReferenceData
}

// NewWagerTransactionPendingReference constrói a versão 1 do evento.
func NewWagerTransactionPendingReference(trace Trace, now time.Time, d WagerTransactionPendingReferenceData) (WagerTransactionPendingReference, error) {
	h, err := newHeader(TypeWagerTransactionPendingReference, 1, d.TransactionID.String(), trace, now)
	if err != nil {
		return WagerTransactionPendingReference{}, err
	}
	return WagerTransactionPendingReference{header: h, data: d}, nil
}

// Data devolve o payload tipado.
func (e WagerTransactionPendingReference) Data() any { return e.data }

// Payload devolve o payload tipado.
func (e WagerTransactionPendingReference) Payload() WagerTransactionPendingReferenceData {
	return e.data
}

// WalletBalanceChangedData é o payload de WalletBalanceChanged.
type WalletBalanceChangedData struct {
	WalletID      domain.WalletID      `json:"walletId"`
	TransactionID domain.TransactionID `json:"transactionId"`
	Direction     wallet.Direction     `json:"direction"`
	Money         money.Money          `json:"money"`
	BalanceBefore money.Money          `json:"balanceBefore"`
	BalanceAfter  money.Money          `json:"balanceAfter"`
	WalletVersion int64                `json:"walletVersion"`
}

// WalletBalanceChanged é emitido em toda alteração efetiva de saldo.
type WalletBalanceChanged struct {
	header
	data WalletBalanceChangedData
}

// NewWalletBalanceChanged constrói a versão 1 do evento a partir de um
// lançamento e da versão de carteira que esse lançamento produziu.
func NewWalletBalanceChanged(trace Trace, now time.Time, entry wallet.LedgerEntry, walletVersion int64) (WalletBalanceChanged, error) {
	d := WalletBalanceChangedData{
		WalletID:      entry.WalletID(),
		TransactionID: entry.TransactionID(),
		Direction:     entry.Direction(),
		Money:         entry.Amount(),
		BalanceBefore: entry.BalanceBefore(),
		BalanceAfter:  entry.BalanceAfter(),
		WalletVersion: walletVersion,
	}
	h, err := newHeader(TypeWalletBalanceChanged, 1, d.WalletID.String(), trace, now)
	if err != nil {
		return WalletBalanceChanged{}, err
	}
	return WalletBalanceChanged{header: h, data: d}, nil
}

// Data devolve o payload tipado.
func (e WalletBalanceChanged) Data() any { return e.data }

// Payload devolve o payload tipado.
func (e WalletBalanceChanged) Payload() WalletBalanceChangedData { return e.data }
