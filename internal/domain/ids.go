package domain

import (
	"fmt"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// ID é um UUID tipado pela entidade que identifica, de modo que um WalletID
// nunca pode ser passado onde se espera um TransactionID. O valor zero é inválido.
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

// AnyID é satisfeito por todo ID tipado (WalletID, TransactionID, ...).
type AnyID interface {
	~struct{ value uuid.UUID }
}

// NewID devolve um novo UUIDv7 ordenado no tempo, por exemplo NewID[WalletID]().
func NewID[I AnyID]() (I, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return I{}, fmt.Errorf("generate id: %w", err)
	}
	return I{value: u}, nil
}

// ParseID aceita só a forma canônica de 36 caracteres com hífens, em qualquer
// caixa; outras formas aceitas pela biblioteca uuid (URN, chaves, sem hífens)
// são rejeitadas. O UUID nulo é rejeitado.
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

// MustParseID interpreta um literal sabidamente válido. Destinado a testes.
func MustParseID[I AnyID](s string) I {
	id, err := ParseID[I](s)
	if err != nil {
		panic(err)
	}
	return id
}

// String devolve a forma canônica em minúsculas.
func (id ID[T]) String() string { return id.value.String() }

// IsZero informa se o id não foi inicializado.
func (id ID[T]) IsZero() bool { return id.value == uuid.Nil }

// MarshalText implementa encoding.TextMarshaler; um id não inicializado falha.
func (id ID[T]) MarshalText() ([]byte, error) {
	if id.IsZero() {
		return nil, Errorf(ErrInconsistentState, "uninitialized id")
	}
	return []byte(id.String()), nil
}

// MaxTextIDLength é o tamanho máximo, em bytes, dos identificadores textuais
// informados pelo provedor.
const MaxTextIDLength = 255

// ValidateTextID confere um identificador informado pelo provedor: não vazio,
// no máximo MaxTextIDLength bytes, UTF-8 válido, sem caracteres de controle e
// sem espaço nas pontas (rejeitado, não removido).
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
