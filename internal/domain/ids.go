package domain

import (
	"fmt"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// ID is a UUID typed by the entity it identifies, so a WalletID can never be
// passed where a TransactionID is expected. The zero value is invalid.
type ID[T any] struct {
	value uuid.UUID
}

type (
	walletTag      struct{}
	playerTag      struct{}
	transactionTag struct{}
	ledgerEntryTag struct{}
	eventTag       struct{}
)

type (
	WalletID      = ID[walletTag]
	PlayerID      = ID[playerTag]
	TransactionID = ID[transactionTag]
	LedgerEntryID = ID[ledgerEntryTag]
	EventID       = ID[eventTag]
)

// AnyID is satisfied by every typed ID (WalletID, TransactionID, ...).
type AnyID interface {
	~struct{ value uuid.UUID }
}

// NewID returns a new time-ordered UUIDv7, e.g. NewID[WalletID]().
func NewID[I AnyID]() (I, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return I{}, fmt.Errorf("generate id: %w", err)
	}
	return I{value: u}, nil
}

// ParseID accepts only the canonical 36-character hyphenated form, in any
// letter case; other forms accepted by the uuid library (URN, braces, no
// hyphens) are rejected. The nil UUID is rejected.
func ParseID[I AnyID](s string) (I, error) {
	if len(s) != 36 {
		return I{}, Errorf(ErrInvalidRequest, "invalid uuid %q", truncate(s))
	}
	u, err := uuid.Parse(s)
	if err != nil || u == uuid.Nil {
		return I{}, Errorf(ErrInvalidRequest, "invalid uuid %q", truncate(s))
	}
	return I{value: u}, nil
}

// MustParseID parses a known-valid literal. Intended for tests.
func MustParseID[I AnyID](s string) I {
	id, err := ParseID[I](s)
	if err != nil {
		panic(err)
	}
	return id
}

// String returns the canonical lowercase form.
func (id ID[T]) String() string { return id.value.String() }

// IsZero reports whether the id is uninitialized.
func (id ID[T]) IsZero() bool { return id.value == uuid.Nil }

// MarshalText implements encoding.TextMarshaler; an uninitialized id fails.
func (id ID[T]) MarshalText() ([]byte, error) {
	if id.IsZero() {
		return nil, Errorf(ErrInconsistentState, "uninitialized id")
	}
	return []byte(id.String()), nil
}

// MaxTextIDLength is the maximum size, in bytes, of provider supplied textual
// identifiers.
const MaxTextIDLength = 255

// ValidateTextID checks a provider supplied identifier: non-empty, at most
// MaxTextIDLength bytes, valid UTF-8, no control characters and no leading or
// trailing whitespace (rejected rather than trimmed).
func ValidateTextID(field, s string) error {
	switch {
	case s == "":
		return Errorf(ErrInvalidRequest, "%s is required", field)
	case len(s) > MaxTextIDLength:
		return Errorf(ErrInvalidRequest, "%s exceeds %d bytes", field, MaxTextIDLength)
	case !utf8.ValidString(s):
		return Errorf(ErrInvalidRequest, "%s is not valid UTF-8", field)
	}
	first, _ := utf8.DecodeRuneInString(s)
	last, _ := utf8.DecodeLastRuneInString(s)
	if unicode.IsSpace(first) || unicode.IsSpace(last) {
		return Errorf(ErrInvalidRequest, "%s has leading or trailing whitespace", field)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return Errorf(ErrInvalidRequest, "%s contains control characters", field)
		}
	}
	return nil
}

func truncate(s string) string {
	const limit = 64
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
