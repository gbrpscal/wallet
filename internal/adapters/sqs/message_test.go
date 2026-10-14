package sqs

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gbrpscal/wallet/internal/domain"
)

const validEnvelope = `{"messageId":"msg-123","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00.000Z",
"data":{"providerId":"provider-a","externalTransactionId":"transaction-123","idempotencyKey":"provider-a:transaction-123",
"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37",
"roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}}`

// TestParseSpecificationEnvelope lê o exemplo do enunciado.
func TestParseSpecificationEnvelope(t *testing.T) {
	env, err := ParseEnvelope(validEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	in := env.RequestInput()
	if env.MessageID != "msg-123" || in.IdempotencyKey != "provider-a:transaction-123" || in.Amount != "25.00" ||
		in.Currency != "BRL" || in.Kind != "BET" || in.WalletID != "0192f291-27dd-7d3f-8071-5f8685deef37" {
		t.Fatalf("parsed = %+v", in)
	}
}

func TestParseRejectsInvalidEnvelopes(t *testing.T) {
	cases := map[string]struct {
		body string
		code domain.Code
	}{
		"malformed":        {`{"messageId":`, codeInvalidMessage},
		"unknown field":    {strings.Replace(validEnvelope, `"type"`, `"x":1,"type"`, 1), codeInvalidMessage},
		"unknown data key": {strings.Replace(validEnvelope, `"kind"`, `"x":1,"kind"`, 1), codeInvalidMessage},
		"wrong type":       {strings.Replace(validEnvelope, "WagerTransactionRequested", "Other", 1), codeInvalidMessage},
		"no message id":    {strings.Replace(validEnvelope, `"msg-123"`, `""`, 1), codeInvalidMessage},
		"bad occurredAt":   {strings.Replace(validEnvelope, "2026-09-08T12:00:00.000Z", "yesterday", 1), codeInvalidMessage},
		"no data":          {`{"messageId":"m","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00Z"}`, codeInvalidMessage},
		"trailing object":  {validEnvelope + `{}`, codeInvalidMessage},
		"numeric amount":   {strings.Replace(validEnvelope, `"amount":"25.00"`, `"amount":25.00`, 1), domain.CodeInvalidAmount},
	}
	for name, c := range cases {
		_, err := ParseEnvelope(c.body)
		if code, _ := domain.CodeOf(err); code != c.code {
			t.Errorf("%s: error = %v, want %s", name, err, c.code)
		}
		if !permanent(err) {
			t.Errorf("%s: invalid envelope must be a permanent error", name)
		}
	}
}

func TestBodyHashIsStableAndSensitive(t *testing.T) {
	if BodyHash(validEnvelope) != BodyHash(validEnvelope) {
		t.Fatal("hash must be deterministic")
	}
	if BodyHash(validEnvelope) == BodyHash(strings.Replace(validEnvelope, "25.00", "26.00", 1)) {
		t.Fatal("different bodies must have different hashes")
	}
}

func TestPermanentVersusTransient(t *testing.T) {
	permanentErrs := []error{
		domain.Errorf(domain.ErrInvalidAmount, "x"), domain.Errorf(domain.ErrWalletNotFound, "x"),
		domain.Errorf(domain.ErrIdempotencyConflict, "x"), domain.Errorf(domain.ErrMessageHashMismatch, "x"),
		domain.Errorf(domain.ErrInconsistentState, "x"),
	}
	for _, err := range permanentErrs {
		if !permanent(err) {
			t.Errorf("%v must go to the dead-letter queue", err)
		}
	}
	for _, err := range []error{errors.New("connection refused"), errors.New("timeout")} {
		if permanent(err) {
			t.Errorf("%v must be retried", err)
		}
	}
}

func TestConsumerBackoff(t *testing.T) {
	c := &Consumer{cfg: ConsumerConfig{RetryBase: 2 * time.Second, RetryMax: 60 * time.Second}}
	want := map[int]time.Duration{1: 2 * time.Second, 2: 4 * time.Second, 3: 8 * time.Second, 4: 16 * time.Second, 6: 60 * time.Second, 10: 60 * time.Second}
	for receives, d := range want {
		if got := c.backoff(receives); got != d {
			t.Errorf("backoff(%d) = %v, want %v", receives, got, d)
		}
	}
}
