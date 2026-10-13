package wagering_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/wagering"
)

func TestRequestZeroAmountPolicy(t *testing.T) {
	cases := []struct {
		kind, amount, ref string
		want              *domain.Error
	}{
		{"BET", "25.00", "", nil},
		{"BET", "0.00", "", domain.ErrAmountMustBePositive},
		{"WIN", "25.00", "", nil},
		{"WIN", "0.00", "", domain.ErrAmountMustBePositive},
		{"LOSS", "0.00", "", nil},
		{"LOSS", "25.00", "", domain.ErrLossAmountMustBeZero},
		{"REFUND", "25.00", "bet-1", nil},
		{"REFUND", "0.00", "bet-1", domain.ErrAmountMustBePositive},
		{"ROLLBACK", "25.00", "bet-1", nil},
		{"ROLLBACK", "0.00", "bet-1", domain.ErrAmountMustBePositive},
	}
	for _, c := range cases {
		_, err := wagering.NewRequest(input(op(c.kind, "tx-1", c.amount, c.ref)))
		if c.want == nil && err != nil {
			t.Errorf("%s %s: unexpected %v", c.kind, c.amount, err)
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s %s: error = %v, want %s", c.kind, c.amount, err, c.want.Code)
		}
	}
}

func TestRequestKinds(t *testing.T) {
	for _, k := range []string{"OPENING", "bet", "DEPOSIT", ""} {
		_, err := wagering.NewRequest(input(func(in *wagering.RequestInput) { in.Kind = k }))
		if !errors.Is(err, domain.ErrKindNotAllowed) {
			t.Errorf("kind %q: error = %v, want KIND_NOT_ALLOWED", k, err)
		}
	}
}

func TestRequestReferences(t *testing.T) {
	cases := []struct {
		name string
		opt  func(*wagering.RequestInput)
		want *domain.Error
	}{
		{"refund without reference", op("REFUND", "r-1", "25.00", ""), domain.ErrReferenceRequired},
		{"rollback without reference", op("ROLLBACK", "r-1", "25.00", ""), domain.ErrReferenceRequired},
		{"bet with reference", op("BET", "b-1", "25.00", "x"), domain.ErrInvalidRequest},
		{"loss with reference", op("LOSS", "l-1", "0.00", "x"), domain.ErrInvalidRequest},
		{"self reference", op("REFUND", "r-1", "25.00", "r-1"), domain.ErrInvalidRequest},
		{"blank reference", op("REFUND", "r-1", "25.00", " bet"), domain.ErrInvalidRequest},
	}
	for _, c := range cases {
		if _, err := wagering.NewRequest(input(c.opt)); !errors.Is(err, c.want) {
			t.Errorf("%s: error = %v, want %s", c.name, err, c.want.Code)
		}
	}
	if _, err := wagering.NewRequest(input(op("WIN", "w-1", "50.00", "bet-1"))); err != nil {
		t.Errorf("WIN with optional reference rejected: %v", err)
	}
}

func TestRequestFieldValidation(t *testing.T) {
	cases := map[string]func(*wagering.RequestInput){
		"no provider":     func(in *wagering.RequestInput) { in.ProviderID = "" },
		"no external id":  func(in *wagering.RequestInput) { in.ExternalTransactionID = "" },
		"no key":          func(in *wagering.RequestInput) { in.IdempotencyKey = "" },
		"no round":        func(in *wagering.RequestInput) { in.RoundID = "" },
		"no game":         func(in *wagering.RequestInput) { in.GameID = "" },
		"bad player":      func(in *wagering.RequestInput) { in.PlayerID = "player-1" },
		"bad wallet":      func(in *wagering.RequestInput) { in.WalletID = "" },
		"long provider":   func(in *wagering.RequestInput) { in.ProviderID = strings.Repeat("p", 256) },
		"padded key":      func(in *wagering.RequestInput) { in.IdempotencyKey = "key " },
		"bad amount":      func(in *wagering.RequestInput) { in.Amount = "25" },
		"bad currency":    func(in *wagering.RequestInput) { in.Currency = "JPY" },
		"scientific":      func(in *wagering.RequestInput) { in.Amount = "2.5e1" },
		"negative amount": func(in *wagering.RequestInput) { in.Amount = "-25.00" },
	}
	for name, mutate := range cases {
		_, err := wagering.NewRequest(input(mutate))
		if k, _ := domain.KindOf(err); k != domain.KindValidation {
			t.Errorf("%s: error = %v, want a validation error", name, err)
		}
	}
}

// TestHashCanonicalVector fixa o algoritmo: SHA-256 sobre JSON canônico com
// chaves ordenadas e sem espaços, prefixado pela versão.
func TestHashCanonicalVector(t *testing.T) {
	canonical := `{"externalTransactionId":"transaction-123","gameId":"fortune-chimp","kind":"BET",` +
		`"money":{"amount":"25.00","currency":"BRL"},"playerId":"` + playerStr + `",` +
		`"providerId":"provider-a","roundId":"round-987","walletId":"` + walletStr + `"}`
	sum := sha256.Sum256([]byte(canonical))
	want := wagering.PayloadHash("v1:" + hex.EncodeToString(sum[:]))

	if got := request(t).Hash(); got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
}

func TestHashExcludesKeyAndNormalizesUUIDCase(t *testing.T) {
	base := request(t).Hash()

	otherKey := request(t, func(in *wagering.RequestInput) { in.IdempotencyKey = "another-key" })
	if otherKey.Hash() != base {
		t.Error("idempotency key must not be part of the hash")
	}
	upper := request(t, func(in *wagering.RequestInput) {
		in.PlayerID = strings.ToUpper(in.PlayerID)
		in.WalletID = strings.ToUpper(in.WalletID)
	})
	if upper.Hash() != base {
		t.Error("UUID letter case must be normalized before hashing")
	}
}

func TestHashChangesWithEveryBusinessField(t *testing.T) {
	base := request(t).Hash()
	changes := map[string]func(*wagering.RequestInput){
		"provider": func(in *wagering.RequestInput) { in.ProviderID = "provider-b" },
		"external": func(in *wagering.RequestInput) { in.ExternalTransactionID = "transaction-124" },
		"player":   func(in *wagering.RequestInput) { in.PlayerID = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a2" },
		"wallet":   func(in *wagering.RequestInput) { in.WalletID = "0192f291-27dd-7d3f-8071-5f8685deef38" },
		"round":    func(in *wagering.RequestInput) { in.RoundID = "round-988" },
		"game":     func(in *wagering.RequestInput) { in.GameID = "other-game" },
		"kind":     func(in *wagering.RequestInput) { in.Kind = "WIN" },
		"amount":   func(in *wagering.RequestInput) { in.Amount = "25.01" },
		"currency": func(in *wagering.RequestInput) { in.Currency = "USD" },
		"ref":      func(in *wagering.RequestInput) { in.Kind = "WIN"; in.ReferenceExternalTransactionID = "bet-1" },
	}
	seen := map[wagering.PayloadHash]string{base: "base"}
	for name, change := range changes {
		h := request(t, change).Hash()
		if prev, dup := seen[h]; dup {
			t.Errorf("%s produced the same hash as %s", name, prev)
		}
		seen[h] = name
	}
}

func TestHashHandlesHTMLCharactersVerbatim(t *testing.T) {
	r := request(t, func(in *wagering.RequestInput) { in.GameID = "a<b>&c" })
	canonical := `{"externalTransactionId":"transaction-123","gameId":"a<b>&c","kind":"BET",` +
		`"money":{"amount":"25.00","currency":"BRL"},"playerId":"` + playerStr + `",` +
		`"providerId":"provider-a","roundId":"round-987","walletId":"` + walletStr + `"}`
	sum := sha256.Sum256([]byte(canonical))
	if r.Hash() != wagering.PayloadHash("v1:"+hex.EncodeToString(sum[:])) {
		t.Error("HTML characters must not be escaped in the canonical form")
	}
}
