// Package event defines the integration events written to the outbox. Each
// concrete event fixes its own type and version in its constructor, and its
// payload is an immutable snapshot taken at creation time.
package event

import (
	"encoding/json"
	"time"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/money"
	"github.com/gbrpscal/wallet/internal/domain/wallet"
)

// Event types.
const (
	TypeWagerTransactionProcessed        = "WagerTransactionProcessed"
	TypeWagerTransactionRejected         = "WagerTransactionRejected"
	TypeWalletBalanceChanged             = "WalletBalanceChanged"
	TypeWagerTransactionPendingReference = "WagerTransactionPendingReference"
)

// TimeLayout is RFC 3339 in UTC with fixed microsecond precision.
const TimeLayout = "2006-01-02T15:04:05.000000Z07:00"

// Trace links an event to the request or message that caused it.
type Trace struct {
	CorrelationID string
	CausationID   string
}

// Validate requires a correlation id; the causation id is optional.
func (t Trace) Validate() error {
	if err := domain.ValidateTextID("correlationId", t.CorrelationID); err != nil {
		return err
	}
	if t.CausationID != "" {
		return domain.ValidateTextID("causationId", t.CausationID)
	}
	return nil
}

// Event is implemented by every concrete event.
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

// envelope is the wire format published to consumers.
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

// Marshal serializes the event envelope. The result is what the outbox stores
// and what gets published, unchanged, on every attempt.
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

// TransactionRef identifies the wager transaction an event is about. External
// fields are empty, and omitted, for the internal OPENING transaction.
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

// WagerTransactionProcessedData is the payload of WagerTransactionProcessed.
type WagerTransactionProcessedData struct {
	TransactionRef
	ReferenceTransactionID string      `json:"referenceTransactionId,omitempty"`
	Balance                money.Money `json:"balance"`
}

// WagerTransactionProcessed is emitted on every successful completion,
// including LOSS and the internal OPENING.
type WagerTransactionProcessed struct {
	header
	data WagerTransactionProcessedData
}

// NewWagerTransactionProcessed builds version 1 of the event.
func NewWagerTransactionProcessed(trace Trace, now time.Time, d WagerTransactionProcessedData) (WagerTransactionProcessed, error) {
	h, err := newHeader(TypeWagerTransactionProcessed, 1, d.TransactionID.String(), trace, now)
	if err != nil {
		return WagerTransactionProcessed{}, err
	}
	return WagerTransactionProcessed{header: h, data: d}, nil
}

// Data returns the typed payload.
func (e WagerTransactionProcessed) Data() any { return e.data }

// Payload returns the typed payload.
func (e WagerTransactionProcessed) Payload() WagerTransactionProcessedData { return e.data }

// WagerTransactionRejectedData is the payload of WagerTransactionRejected.
type WagerTransactionRejectedData struct {
	TransactionRef
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId,omitempty"`
	FailureCode                    domain.Code `json:"failureCode"`
}

// WagerTransactionRejected is emitted on a definitive business rejection.
type WagerTransactionRejected struct {
	header
	data WagerTransactionRejectedData
}

// NewWagerTransactionRejected builds version 1 of the event.
func NewWagerTransactionRejected(trace Trace, now time.Time, d WagerTransactionRejectedData) (WagerTransactionRejected, error) {
	h, err := newHeader(TypeWagerTransactionRejected, 1, d.TransactionID.String(), trace, now)
	if err != nil {
		return WagerTransactionRejected{}, err
	}
	return WagerTransactionRejected{header: h, data: d}, nil
}

// Data returns the typed payload.
func (e WagerTransactionRejected) Data() any { return e.data }

// Payload returns the typed payload.
func (e WagerTransactionRejected) Payload() WagerTransactionRejectedData { return e.data }

// WagerTransactionPendingReferenceData is the payload of
// WagerTransactionPendingReference.
type WagerTransactionPendingReferenceData struct {
	TransactionRef
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId"`
}

// WagerTransactionPendingReference is emitted when an operation starts
// waiting for its reference.
type WagerTransactionPendingReference struct {
	header
	data WagerTransactionPendingReferenceData
}

// NewWagerTransactionPendingReference builds version 1 of the event.
func NewWagerTransactionPendingReference(trace Trace, now time.Time, d WagerTransactionPendingReferenceData) (WagerTransactionPendingReference, error) {
	h, err := newHeader(TypeWagerTransactionPendingReference, 1, d.TransactionID.String(), trace, now)
	if err != nil {
		return WagerTransactionPendingReference{}, err
	}
	return WagerTransactionPendingReference{header: h, data: d}, nil
}

// Data returns the typed payload.
func (e WagerTransactionPendingReference) Data() any { return e.data }

// Payload returns the typed payload.
func (e WagerTransactionPendingReference) Payload() WagerTransactionPendingReferenceData {
	return e.data
}

// WalletBalanceChangedData is the payload of WalletBalanceChanged.
type WalletBalanceChangedData struct {
	WalletID      domain.WalletID      `json:"walletId"`
	TransactionID domain.TransactionID `json:"transactionId"`
	Direction     wallet.Direction     `json:"direction"`
	Money         money.Money          `json:"money"`
	BalanceBefore money.Money          `json:"balanceBefore"`
	BalanceAfter  money.Money          `json:"balanceAfter"`
	WalletVersion int64                `json:"walletVersion"`
}

// WalletBalanceChanged is emitted for every effective balance change.
type WalletBalanceChanged struct {
	header
	data WalletBalanceChangedData
}

// NewWalletBalanceChanged builds version 1 of the event from a ledger entry
// and the wallet version that entry produced.
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

// Data returns the typed payload.
func (e WalletBalanceChanged) Data() any { return e.data }

// Payload returns the typed payload.
func (e WalletBalanceChanged) Payload() WalletBalanceChangedData { return e.data }
