package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/wagering"
)

func TestBearerToken(t *testing.T) {
	cases := map[string]struct {
		header string
		token  string
		ok     bool
	}{
		"bearer":       {"Bearer abc.def.ghi", "abc.def.ghi", true},
		"case":         {"bearer abc", "abc", true},
		"missing":      {"", "", false},
		"basic":        {"Basic dXNlcjpwYXNz", "", false},
		"empty token":  {"Bearer   ", "", false},
		"no separator": {"Bearerabc", "", false},
	}
	for name, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if c.header != "" {
			r.Header.Set("Authorization", c.header)
		}
		tok, ok := bearerToken(r)
		if tok != c.token || ok != c.ok {
			t.Errorf("%s: got %q %v", name, tok, ok)
		}
	}
}

func TestMoneyInputRejectsNumbers(t *testing.T) {
	var m moneyInput
	if err := json.Unmarshal([]byte(`{"amount":"25.00","currency":"BRL"}`), &m); err != nil || m.Amount != "25.00" || m.Currency != "BRL" {
		t.Fatalf("string amount: %+v %v", m, err)
	}
	for _, bad := range []string{`{"amount":25.00,"currency":"BRL"}`, `{"amount":25,"currency":"BRL"}`, `{"amount":"1.00","currency":"BRL","x":1}`, `"25.00"`} {
		var m moneyInput
		if err := json.Unmarshal([]byte(bad), &m); !errors.Is(err, domain.ErrInvalidAmount) {
			t.Errorf("%s: error = %v, want INVALID_AMOUNT", bad, err)
		}
	}
}

func TestDecodeJSON(t *testing.T) {
	type body struct {
		A string `json:"a"`
	}
	decode := func(ct, payload string) error {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(payload))
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}
		var b body
		return decodeJSON(httptest.NewRecorder(), r, &b)
	}
	if err := decode("application/json; charset=utf-8", `{"a":"x"}`); err != nil {
		t.Fatalf("valid body: %v", err)
	}
	if err := decode("text/plain", `{"a":"x"}`); !errors.Is(err, errUnsupportedMediaType) {
		t.Errorf("wrong content type: %v", err)
	}
	for name, payload := range map[string]string{
		"unknown field": `{"a":"x","b":1}`,
		"trailing data": `{"a":"x"} {"a":"y"}`,
		"malformed":     `{"a":`,
		"too large":     `{"a":"` + strings.Repeat("x", maxBodyBytes) + `"}`,
	} {
		if err := decode("application/json", payload); !errors.Is(err, domain.ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestErrorMapping(t *testing.T) {
	api := &API{log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{domain.Errorf(domain.ErrInvalidAmount, "x"), 400, "INVALID_AMOUNT"},
		{domain.Errorf(domain.ErrWalletNotFound, "x"), 404, "WALLET_NOT_FOUND"},
		{domain.Errorf(domain.ErrIdempotencyConflict, "x"), 409, "IDEMPOTENCY_KEY_CONFLICT"},
		{domain.Errorf(domain.ErrInsufficientFunds, "x"), 422, "INSUFFICIENT_FUNDS"},
		{&app.TransientError{Err: errors.New("deadlock")}, 503, "UNAVAILABLE"},
		{app.ErrConcurrentUpdate, 503, "UNAVAILABLE"},
		{domain.Errorf(domain.ErrInconsistentState, "secret detail"), 500, "INCONSISTENT_STATE"},
		{errors.New("boom"), 500, "INTERNAL_ERROR"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		api.fail(rec, httptest.NewRequest(http.MethodGet, "/", nil), c.err)
		var body problemDoc
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != c.status || body.Code != c.code {
			t.Errorf("%v: %d %s, want %d %s", c.err, rec.Code, body.Code, c.status, c.code)
		}
		if c.status == 503 && rec.Header().Get("Retry-After") == "" {
			t.Errorf("%v: missing Retry-After", c.err)
		}
		if c.status == 500 && strings.Contains(rec.Body.String(), "secret detail") {
			t.Errorf("internal detail leaked: %s", rec.Body.String())
		}
	}
}

func TestStatusFor(t *testing.T) {
	want := map[string]int{"PROCESSED": 200, "REJECTED": 422, "PENDING_REFERENCE": 202, "PENDING": 202, "FAILED": 500}
	for s, code := range want {
		if got := statusFor(wageringStatus(s)); got != code {
			t.Errorf("%s = %d, want %d", s, got, code)
		}
	}
}

func TestRecovererReturnsProblem(t *testing.T) {
	h := withCorrelation(recoverer(slog.New(slog.NewJSONHandler(io.Discard, nil)), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "INTERNAL_ERROR") || rec.Header().Get(CorrelationHeader) == "" {
		t.Fatalf("panic response = %d %s", rec.Code, rec.Body.String())
	}
}

func wageringStatus(s string) wagering.Status { return wagering.Status(s) }
