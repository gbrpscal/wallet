package money_test

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/money"
)

func mustParse(t *testing.T, amount, currency string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, currency)
	if err != nil {
		t.Fatalf("Parse(%q, %q): %v", amount, currency, err)
	}
	return m
}

func mustMinor(t *testing.T, minor int64, c money.Currency) money.Money {
	t.Helper()
	m, err := money.FromMinor(minor, c)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParseAcceptsStrictForm(t *testing.T) {
	cases := map[string]int64{
		"0.00":                 0,
		"0.01":                 1,
		"25.00":                2500,
		"1000.00":              100000,
		"92233720368547758.07": math.MaxInt64,
		"10.50":                1050,
	}
	for in, want := range cases {
		m := mustParse(t, in, "BRL")
		if m.MinorUnits() != want {
			t.Errorf("Parse(%q) = %d minor units, want %d", in, m.MinorUnits(), want)
		}
		if m.Amount() != in {
			t.Errorf("Parse(%q).Amount() = %q, round trip lost", in, m.Amount())
		}
	}
}

func TestParseRejectsInvalidAmounts(t *testing.T) {
	invalid := []string{
		"", " ", "NaN", "nan", "Infinity", "-Infinity", "+Inf", "1e3", "1E2", "2.5e1",
		"25", "25.0", "25.000", "25.", ".25", "-1.00", "+1.00", "-0.00", " 1.00", "1.00 ",
		"01.00", "00.00", "1,00", "1_000.00", "0x10.00", "１.００", "92233720368547758.08",
		"99999999999999999999.00",
	}
	for _, in := range invalid {
		_, err := money.Parse(in, "BRL")
		if !errors.Is(err, domain.ErrInvalidAmount) {
			t.Errorf("Parse(%q) error = %v, want INVALID_AMOUNT", in, err)
		}
		if k, _ := domain.KindOf(err); k != domain.KindValidation {
			t.Errorf("Parse(%q) kind = %v, want validation", in, k)
		}
	}
}

func TestParseCurrency(t *testing.T) {
	for _, c := range []string{"BRL", "USD", "EUR"} {
		if _, err := money.ParseCurrency(c); err != nil {
			t.Errorf("ParseCurrency(%q): %v", c, err)
		}
	}
	for _, c := range []string{"", "brl", "Brl", "JPY", "KWD", "BR", "BRLL", " BRL"} {
		if _, err := money.ParseCurrency(c); !errors.Is(err, domain.ErrUnsupportedCurrency) {
			t.Errorf("ParseCurrency(%q) error = %v, want UNSUPPORTED_CURRENCY", c, err)
		}
	}
}

func TestZeroAndUninitialized(t *testing.T) {
	z, err := money.Zero(money.BRL)
	if err != nil || !z.IsZero() || !z.IsValid() || z.Amount() != "0.00" {
		t.Fatalf("Zero(BRL) = %v, %v", z, err)
	}
	if _, err := money.Zero(money.Currency{}); err == nil {
		t.Error("Zero with uninitialized currency must fail")
	}
	if _, err := money.FromMinor(1, money.Currency{}); err == nil {
		t.Error("FromMinor with uninitialized currency must fail")
	}

	var uninit money.Money
	if uninit.IsValid() {
		t.Error("zero value Money must be invalid")
	}
	if _, err := uninit.Add(z); !errors.Is(err, domain.ErrInvalidAmount) {
		t.Errorf("Add on uninitialized = %v", err)
	}
	if _, err := z.Sub(uninit); !errors.Is(err, domain.ErrInvalidAmount) {
		t.Errorf("Sub with uninitialized = %v", err)
	}
	if _, err := uninit.Neg(); !errors.Is(err, domain.ErrInvalidAmount) {
		t.Errorf("Neg on uninitialized = %v", err)
	}
	if _, err := uninit.Cmp(z); !errors.Is(err, domain.ErrInvalidAmount) {
		t.Errorf("Cmp on uninitialized = %v", err)
	}
	if _, err := json.Marshal(uninit); err == nil {
		t.Error("marshal of uninitialized money must fail")
	}
}

func TestArithmetic(t *testing.T) {
	a := mustParse(t, "100.00", "BRL")
	b := mustParse(t, "80.00", "BRL")

	sum, err := a.Add(b)
	if err != nil || sum.Amount() != "180.00" {
		t.Errorf("Add = %v, %v", sum, err)
	}
	diff, err := b.Sub(a)
	if err != nil || diff.Amount() != "-20.00" || !diff.IsNegative() {
		t.Errorf("Sub = %v, %v (negative differences are allowed)", diff, err)
	}
	neg, err := a.Neg()
	if err != nil || neg.Amount() != "-100.00" {
		t.Errorf("Neg = %v, %v", neg, err)
	}
	if c, _ := a.Cmp(b); c != 1 {
		t.Errorf("Cmp(100, 80) = %d", c)
	}
	if c, _ := b.Cmp(a); c != -1 {
		t.Errorf("Cmp(80, 100) = %d", c)
	}
	if c, _ := a.Cmp(mustParse(t, "100.00", "BRL")); c != 0 {
		t.Errorf("Cmp(100, 100) = %d", c)
	}
	// Imutabilidade: os operandos não mudam.
	if a.Amount() != "100.00" || b.Amount() != "80.00" {
		t.Error("operations must not modify operands")
	}
}

func TestSmallNegativeFormatting(t *testing.T) {
	if got := mustMinor(t, -5, money.BRL).Amount(); got != "-0.05" {
		t.Errorf("Amount(-5) = %q", got)
	}
	if got := mustMinor(t, math.MinInt64, money.BRL).Amount(); got != "-92233720368547758.08" {
		t.Errorf("Amount(MinInt64) = %q", got)
	}
}

func TestOverflow(t *testing.T) {
	max := mustMinor(t, math.MaxInt64, money.BRL)
	min := mustMinor(t, math.MinInt64, money.BRL)
	one := mustMinor(t, 1, money.BRL)
	minusOne := mustMinor(t, -1, money.BRL)

	checks := []struct {
		name string
		fn   func() (money.Money, error)
	}{
		{"max+1", func() (money.Money, error) { return max.Add(one) }},
		{"min+(-1)", func() (money.Money, error) { return min.Add(minusOne) }},
		{"min-1", func() (money.Money, error) { return min.Sub(one) }},
		{"max-(-1)", func() (money.Money, error) { return max.Sub(minusOne) }},
		{"0-min", func() (money.Money, error) { return mustMinor(t, 0, money.BRL).Sub(min) }},
		{"-min", func() (money.Money, error) { return min.Neg() }},
	}
	for _, c := range checks {
		if _, err := c.fn(); !errors.Is(err, domain.ErrInvalidAmount) {
			t.Errorf("%s: error = %v, want overflow", c.name, err)
		}
	}
	if v, err := max.Sub(one); err != nil || v.MinorUnits() != math.MaxInt64-1 {
		t.Errorf("max-1 = %v, %v", v, err)
	}
}

func TestCurrencyMismatch(t *testing.T) {
	brl := mustParse(t, "10.00", "BRL")
	usd := mustParse(t, "10.00", "USD")
	if _, err := brl.Add(usd); !errors.Is(err, domain.ErrCurrencyMismatch) {
		t.Errorf("Add BRL+USD = %v", err)
	}
	if _, err := brl.Sub(usd); !errors.Is(err, domain.ErrCurrencyMismatch) {
		t.Errorf("Sub BRL-USD = %v", err)
	}
	if _, err := brl.Cmp(usd); !errors.Is(err, domain.ErrCurrencyMismatch) {
		t.Errorf("Cmp BRL/USD = %v", err)
	}
	if brl.Equal(usd) {
		t.Error("same amount in different currencies must not be equal")
	}
}

func TestJSON(t *testing.T) {
	m := mustParse(t, "25.00", "BRL")
	b, err := json.Marshal(m)
	if err != nil || string(b) != `{"amount":"25.00","currency":"BRL"}` {
		t.Fatalf("Marshal = %s, %v", b, err)
	}
	var back money.Money
	if err := json.Unmarshal(b, &back); err != nil || !back.Equal(m) {
		t.Fatalf("Unmarshal = %v, %v", back, err)
	}

	bad := []string{
		`{"amount":25.00,"currency":"BRL"}`,
		`{"amount":"25","currency":"BRL"}`,
		`{"amount":"-1.00","currency":"BRL"}`,
		`{"amount":"1.00","currency":"brl"}`,
		`{"amount":"1.00"}`,
		`{"amount":"NaN","currency":"BRL"}`,
		`"25.00"`,
	}
	for _, in := range bad {
		var v money.Money
		if err := json.Unmarshal([]byte(in), &v); err == nil {
			t.Errorf("Unmarshal(%s) must fail", in)
		}
	}
}
