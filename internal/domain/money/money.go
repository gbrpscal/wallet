// Package money implements an exact monetary value object stored as int64
// minor units with a fixed scale of two decimal places.
package money

import (
	"encoding/json"
	"math"
	"strconv"

	"github.com/gbrpscal/wallet/internal/domain"
)

// Scale is the number of decimal places of every supported currency.
const Scale = 2

const minorPerUnit = 100

// Currency is an ISO 4217 code from the supported set. The zero value is
// invalid.
type Currency struct {
	code string
}

// Supported currencies. All of them use two decimal places, matching Scale.
var (
	BRL = Currency{code: "BRL"}
	USD = Currency{code: "USD"}
	EUR = Currency{code: "EUR"}
)

var supported = map[string]Currency{"BRL": BRL, "USD": USD, "EUR": EUR}

// ParseCurrency accepts only the exact upper-case code of a supported currency.
func ParseCurrency(code string) (Currency, error) {
	c, ok := supported[code]
	if !ok {
		return Currency{}, domain.Errorf(domain.ErrUnsupportedCurrency, "unsupported currency %q", code)
	}
	return c, nil
}

// String returns the ISO 4217 code.
func (c Currency) String() string { return c.code }

// IsZero reports whether the currency is uninitialized.
func (c Currency) IsZero() bool { return c.code == "" }

// Money is an immutable amount in minor units plus its currency. Negative
// values are allowed for internal differences; external parsing rejects them.
// The zero value is invalid: use Zero(currency).
type Money struct {
	minor    int64
	currency Currency
}

// Zero returns zero in the given currency.
func Zero(c Currency) (Money, error) {
	if c.IsZero() {
		return Money{}, domain.Errorf(domain.ErrUnsupportedCurrency, "uninitialized currency")
	}
	return Money{currency: c}, nil
}

// FromMinor builds a value from minor units, as read from persistence.
func FromMinor(minor int64, c Currency) (Money, error) {
	if c.IsZero() {
		return Money{}, domain.Errorf(domain.ErrUnsupportedCurrency, "uninitialized currency")
	}
	return Money{minor: minor, currency: c}, nil
}

// Parse reads an external amount. Only the strict form
// `^(0|[1-9][0-9]*)\.[0-9]{2}$` is accepted: no sign, no exponent, no
// whitespace, no leading zeros and exactly two decimals. Nothing is rounded.
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

// MinorUnits returns the amount in minor units.
func (m Money) MinorUnits() int64 { return m.minor }

// Currency returns the currency.
func (m Money) Currency() Currency { return m.currency }

// IsValid reports whether the value was properly initialized.
func (m Money) IsValid() bool { return !m.currency.IsZero() }

// IsZero reports whether the amount is zero.
func (m Money) IsZero() bool { return m.minor == 0 }

// IsPositive reports whether the amount is greater than zero.
func (m Money) IsPositive() bool { return m.minor > 0 }

// IsNegative reports whether the amount is less than zero.
func (m Money) IsNegative() bool { return m.minor < 0 }

// Equal reports whether both values have the same amount and currency.
func (m Money) Equal(o Money) bool { return m == o }

// Add returns m + o.
func (m Money) Add(o Money) (Money, error) {
	if err := m.compatible(o); err != nil {
		return Money{}, err
	}
	if (o.minor > 0 && m.minor > math.MaxInt64-o.minor) || (o.minor < 0 && m.minor < math.MinInt64-o.minor) {
		return Money{}, domain.Errorf(domain.ErrInvalidAmount, "addition overflow")
	}
	return Money{minor: m.minor + o.minor, currency: m.currency}, nil
}

// Sub returns m - o.
func (m Money) Sub(o Money) (Money, error) {
	if err := m.compatible(o); err != nil {
		return Money{}, err
	}
	if (o.minor < 0 && m.minor > math.MaxInt64+o.minor) || (o.minor > 0 && m.minor < math.MinInt64+o.minor) {
		return Money{}, domain.Errorf(domain.ErrInvalidAmount, "subtraction overflow")
	}
	return Money{minor: m.minor - o.minor, currency: m.currency}, nil
}

// Neg returns -m.
func (m Money) Neg() (Money, error) {
	if !m.IsValid() {
		return Money{}, domain.Errorf(domain.ErrInvalidAmount, "uninitialized money")
	}
	if m.minor == math.MinInt64 {
		return Money{}, domain.Errorf(domain.ErrInvalidAmount, "negation overflow")
	}
	return Money{minor: -m.minor, currency: m.currency}, nil
}

// Cmp returns -1, 0 or +1 comparing m to o.
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

// Amount formats the amount with exactly two decimals, e.g. "-0.05".
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

// String returns "<amount> <currency>" for logs and messages.
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

// MarshalJSON produces {"amount":"25.00","currency":"BRL"}.
func (m Money) MarshalJSON() ([]byte, error) {
	if !m.IsValid() {
		return nil, domain.Errorf(domain.ErrInvalidAmount, "uninitialized money")
	}
	return json.Marshal(wire{Amount: m.Amount(), Currency: m.currency.String()})
}

// UnmarshalJSON applies the strict external parsing rules of Parse.
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
