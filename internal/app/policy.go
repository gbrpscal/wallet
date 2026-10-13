package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/gbrpscal/wallet/internal/domain"
)

// Clock fornece o instante atual; os testes injetam um relógio controlado.
type Clock interface {
	Now() time.Time
}

// SystemClock usa o relógio do sistema.
type SystemClock struct{}

// Now devolve o instante atual.
func (SystemClock) Now() time.Time { return time.Now() }

// RetryPolicy repete operações cujo erro é seguro de repetir, porque nada foi
// confirmado: falhas transitórias, versão desatualizada e colisão de
// unicidade (que, relida, vira replay ou conflito).
type RetryPolicy struct {
	// Retries é o número de novas tentativas depois da primeira.
	Retries int
	// BaseDelay é a espera antes da primeira nova tentativa; dobra a cada uma.
	BaseDelay time.Duration
}

// DefaultRetryPolicy: até 3 novas tentativas, esperando 50, 100 e 200 ms (±20%).
var DefaultRetryPolicy = RetryPolicy{Retries: 3, BaseDelay: 50 * time.Millisecond}

// Do executa fn e a repete enquanto o erro for repetível.
func (p RetryPolicy) Do(ctx context.Context, fn func() error) error {
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil || !retryable(err) || attempt >= p.Retries {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(jitter(p.BaseDelay << attempt)):
		}
	}
}

func retryable(err error) bool {
	return IsRetryable(err) || errors.Is(err, ErrDuplicate)
}

// jitter aplica ±20% com aritmética inteira.
func jitter(d time.Duration) time.Duration {
	spread := d / 5
	if spread <= 0 {
		return d
	}
	return d - spread + rand.N(2*spread+1)
}

// ReferencePolicy é a política de espera por referência (D4): backoff
// exponencial com teto e jitter, encerrada pelo que vier primeiro entre
// MaxAttempts tentativas do worker e TTL desde a criação.
type ReferencePolicy struct {
	MaxAttempts int
	TTL         time.Duration
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// DefaultReferencePolicy: 10 tentativas ou 10 minutos; espera de 1s·2ⁿ com
// teto de 60s.
var DefaultReferencePolicy = ReferencePolicy{
	MaxAttempts: 10,
	TTL:         10 * time.Minute,
	BaseDelay:   time.Second,
	MaxDelay:    time.Minute,
}

// Initial agenda a primeira tentativa do worker para uma transação criada em
// createdAt.
func (p ReferencePolicy) Initial(createdAt time.Time) ReferenceSchedule {
	return ReferenceSchedule{
		Attempts:      0,
		NextAttemptAt: domain.Timestamp(createdAt.Add(p.delay(0))),
		ExpiresAt:     domain.Timestamp(createdAt.Add(p.TTL)),
	}
}

// Next registra uma tentativa sem sucesso feita em now e agenda a seguinte,
// nunca depois do prazo final.
func (p ReferencePolicy) Next(s ReferenceSchedule, now time.Time) ReferenceSchedule {
	next := now.Add(p.delay(s.Attempts + 1))
	if next.After(s.ExpiresAt) {
		next = s.ExpiresAt
	}
	return ReferenceSchedule{
		Attempts:      s.Attempts + 1,
		NextAttemptAt: domain.Timestamp(next),
		ExpiresAt:     s.ExpiresAt,
	}
}

// Exhausted informa se a espera terminou: tentativas esgotadas ou prazo vencido.
func (p ReferencePolicy) Exhausted(s ReferenceSchedule, now time.Time) bool {
	return s.Attempts >= p.MaxAttempts || !now.Before(s.ExpiresAt)
}

func (p ReferencePolicy) delay(attempt int) time.Duration {
	d := p.BaseDelay
	for i := 0; i < attempt && d < p.MaxDelay; i++ {
		d *= 2
	}
	if d > p.MaxDelay {
		d = p.MaxDelay
	}
	return jitter(d)
}

// Paginação do ledger.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// cursor é o conteúdo do cursor opaco: base64url(JSON) com versão de formato.
type cursor struct {
	V     int   `json:"v"`
	After int64 `json:"after"`
}

// EncodeCursor gera o cursor da página seguinte a afterVersion.
func EncodeCursor(afterVersion int64) string {
	b, _ := json.Marshal(cursor{V: 1, After: afterVersion})
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor interpreta um cursor; vazio significa o início do ledger.
func DecodeCursor(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, domain.Errorf(domain.ErrInvalidRequest, "invalid cursor")
	}
	var c cursor
	if err := json.Unmarshal(b, &c); err != nil || c.V != 1 || c.After < 0 {
		return 0, domain.Errorf(domain.ErrInvalidRequest, "invalid cursor")
	}
	return c.After, nil
}
