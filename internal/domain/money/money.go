// Package money implementa um value object monetário exato, armazenado em
// unidades mínimas int64 com escala fixa de duas casas decimais.
package money

import (
	"encoding/json"
	"math"
	"strconv"

	"github.com/gbrpscal/wallet/internal/domain"
)

// Scale é o número de casas decimais de toda moeda suportada.
const Scale = 2

const minorPerUnit = 100

// Currency é um código ISO 4217 do conjunto suportado. O valor zero é inválido.
type Currency struct {
	code string
}

// Moedas suportadas. Todas usam duas casas decimais, coerente com Scale.
var (
	BRL = Currency{code: "BRL"}
	USD = Currency{code: "USD"}
	EUR = Currency{code: "EUR"}
)

var supported = map[string]Currency{"BRL": BRL, "USD": USD, "EUR": EUR}

// ParseCurrency aceita somente o código exato, em maiúsculas, de uma moeda suportada.
func ParseCurrency(code string) (Currency, error) {
	c, ok := supported[code]
	if !ok {
		return Currency{}, domain.Errorf(domain.ErrUnsupportedCurrency, "unsupported currency %q", code)
	}
	return c, nil
}

// String devolve o código ISO 4217.
func (c Currency) String() string { return c.code }

// IsZero informa se a moeda não foi inicializada.
func (c Currency) IsZero() bool { return c.code == "" }

// Money é um valor imutável em unidades mínimas mais a sua moeda. Valores
// negativos são permitidos em diferenças internas; o parsing externo os
// rejeita. O valor zero do tipo é inválido: use Zero(currency).
type Money struct {
	minor    int64
	currency Currency
}

// Zero devolve zero na moeda informada.
func Zero(c Currency) (Money, error) {
	if c.IsZero() {
		return Money{}, domain.Errorf(domain.ErrUnsupportedCurrency, "uninitialized currency")
	}
	return Money{currency: c}, nil
}

// FromMinor constrói um valor a partir de unidades mínimas, como lidas da persistência.
func FromMinor(minor int64, c Currency) (Money, error) {
	if c.IsZero() {
		return Money{}, domain.Errorf(domain.ErrUnsupportedCurrency, "uninitialized currency")
	}
	return Money{minor: minor, currency: c}, nil
}

// Parse lê um valor externo. Só a forma estrita `^(0|[1-9][0-9]*)\.[0-9]{2}$`
// é aceita: sem sinal, sem expoente, sem espaços, sem zeros à esquerda e com
// exatamente duas casas. Nada é arredondado.
func Parse(amount, currency string) (Money, error) {
	c, err := ParseCurrency(currency)
	if err != nil {
		return Money{}, err
	}
	minor, err := parseAmount(amount)
	if err != nil {
		return Money{}, err
	}
	return Money{minor: minor, currency: c}, nil
}

func parseAmount(s string) (int64, error) {
	invalid := func(reason string) error {
		return domain.Errorf(domain.ErrInvalidAmount, "amount %q: %s", clip(s), reason)
	}
	if s == "" {
		return 0, invalid("empty")
	}
	dot := len(s) - Scale - 1
	if dot < 1 || s[dot] != '.' {
		return 0, invalid("must have exactly two decimal places")
	}
	intPart, fracPart := s[:dot], s[dot+1:]
	for i := 0; i < len(intPart); i++ {
		if intPart[i] < '0' || intPart[i] > '9' {
			return 0, invalid("must be a non-negative decimal number")
		}
	}
	for i := 0; i < len(fracPart); i++ {
		if fracPart[i] < '0' || fracPart[i] > '9' {
			return 0, invalid("must be a non-negative decimal number")
		}
	}
	if len(intPart) > 1 && intPart[0] == '0' {
		return 0, invalid("leading zeros are not allowed")
	}
	units, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, invalid("out of range")
	}
	cents, _ := strconv.ParseInt(fracPart, 10, 64)
	if units > (math.MaxInt64-cents)/minorPerUnit {
		return 0, invalid("out of range")
	}
	return units*minorPerUnit + cents, nil
}

// MinorUnits devolve o valor em unidades mínimas.
func (m Money) MinorUnits() int64 { return m.minor }

// Currency devolve a moeda.
func (m Money) Currency() Currency { return m.currency }

// IsValid informa se o valor foi inicializado corretamente.
func (m Money) IsValid() bool { return !m.currency.IsZero() }

// IsZero informa se o valor é zero.
func (m Money) IsZero() bool { return m.minor == 0 }

// IsPositive informa se o valor é maior que zero.
func (m Money) IsPositive() bool { return m.minor > 0 }

// IsNegative informa se o valor é menor que zero.
func (m Money) IsNegative() bool { return m.minor < 0 }

// Equal informa se os dois valores têm o mesmo montante e a mesma moeda.
func (m Money) Equal(o Money) bool { return m == o }

// Add devolve m + o.
func (m Money) Add(o Money) (Money, error) {
	if err := m.compatible(o); err != nil {
		return Money{}, err
	}
	if (o.minor > 0 && m.minor > math.MaxInt64-o.minor) || (o.minor < 0 && m.minor < math.MinInt64-o.minor) {
		return Money{}, domain.Errorf(domain.ErrInvalidAmount, "addition overflow")
	}
	return Money{minor: m.minor + o.minor, currency: m.currency}, nil
}

// Sub devolve m - o.
func (m Money) Sub(o Money) (Money, error) {
	if err := m.compatible(o); err != nil {
		return Money{}, err
	}
	if (o.minor < 0 && m.minor > math.MaxInt64+o.minor) || (o.minor > 0 && m.minor < math.MinInt64+o.minor) {
		return Money{}, domain.Errorf(domain.ErrInvalidAmount, "subtraction overflow")
	}
	return Money{minor: m.minor - o.minor, currency: m.currency}, nil
}

// Neg devolve -m.
func (m Money) Neg() (Money, error) {
	if !m.IsValid() {
		return Money{}, domain.Errorf(domain.ErrInvalidAmount, "uninitialized money")
	}
	if m.minor == math.MinInt64 {
		return Money{}, domain.Errorf(domain.ErrInvalidAmount, "negation overflow")
	}
	return Money{minor: -m.minor, currency: m.currency}, nil
}

// Cmp devolve -1, 0 ou +1 comparando m com o.
func (m Money) Cmp(o Money) (int, error) {
	if err := m.compatible(o); err != nil {
		return 0, err
	}
	switch {
	case m.minor < o.minor:
		return -1, nil
	case m.minor > o.minor:
		return 1, nil
	default:
		return 0, nil
	}
}

func (m Money) compatible(o Money) error {
	if !m.IsValid() || !o.IsValid() {
		return domain.Errorf(domain.ErrInvalidAmount, "uninitialized money")
	}
	if m.currency != o.currency {
		return domain.Errorf(domain.ErrCurrencyMismatch, "%s and %s are not compatible", m.currency, o.currency)
	}
	return nil
}

// Amount formata o valor com exatamente duas casas, por exemplo "-0.05".
func (m Money) Amount() string {
	var abs uint64
	neg := m.minor < 0
	if neg {
		abs = uint64(-(m.minor + 1)) + 1
	} else {
		abs = uint64(m.minor)
	}
	frac := abs % minorPerUnit
	s := strconv.FormatUint(abs/minorPerUnit, 10) + "."
	if frac < 10 {
		s += "0"
	}
	s += strconv.FormatUint(frac, 10)
	if neg {
		return "-" + s
	}
	return s
}

// String devolve "<valor> <moeda>" para logs e mensagens.
func (m Money) String() string {
	if !m.IsValid() {
		return "<invalid money>"
	}
	return m.Amount() + " " + m.currency.String()
}

type wire struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// MarshalJSON produz {"amount":"25.00","currency":"BRL"}.
func (m Money) MarshalJSON() ([]byte, error) {
	if !m.IsValid() {
		return nil, domain.Errorf(domain.ErrInvalidAmount, "uninitialized money")
	}
	return json.Marshal(wire{Amount: m.Amount(), Currency: m.currency.String()})
}

// UnmarshalJSON aplica as regras estritas de parsing externo de Parse.
func (m *Money) UnmarshalJSON(b []byte) error {
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return domain.Errorf(domain.ErrInvalidAmount, "money must be an object with string amount and currency")
	}
	parsed, err := Parse(w.Amount, w.Currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}

func clip(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}
