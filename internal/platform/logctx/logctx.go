// Package logctx guarda identificadores no context.Context para que toda
// linha de log registrada com esse contexto os inclua, com nomes padronizados.
// Cada camada acrescenta o que conhece (HTTP: correlationId; consumidor:
// messageId; caso de uso: transactionId e walletId).
package logctx

import (
	"context"
	"log/slog"
)

// Nomes padronizados dos campos de log.
const (
	CorrelationID = "correlationId"
	MessageID     = "messageId"
	TransactionID = "transactionId"
	WalletID      = "walletId"
	ProviderID    = "providerId"
	ClientID      = "clientId"
	EventID       = "eventId"
)

type key struct{}

// With devolve um contexto com os pares chave/valor acrescentados. Uma chave
// repetida substitui o valor anterior.
func With(ctx context.Context, args ...any) context.Context {
	current, _ := ctx.Value(key{}).([]slog.Attr)
	attrs := make([]slog.Attr, 0, len(current)+len(args)/2)
	added := slog.Group("", args...).Value.Group()
	replaced := make(map[string]bool, len(added))
	for _, a := range added {
		replaced[a.Key] = true
	}
	for _, a := range current {
		if !replaced[a.Key] {
			attrs = append(attrs, a)
		}
	}
	attrs = append(attrs, added...)
	return context.WithValue(ctx, key{}, attrs)
}

// Attrs devolve os atributos guardados no contexto.
func Attrs(ctx context.Context) []slog.Attr {
	attrs, _ := ctx.Value(key{}).([]slog.Attr)
	return attrs
}

// Handler acrescenta a cada registro os atributos do contexto.
type Handler struct {
	slog.Handler
}

// Handle inclui os atributos do contexto e delega.
func (h Handler) Handle(ctx context.Context, r slog.Record) error {
	if attrs := Attrs(ctx); len(attrs) > 0 {
		r = r.Clone()
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs preserva o encapsulamento ao derivar o handler.
func (h Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return Handler{h.Handler.WithAttrs(attrs)}
}

// WithGroup preserva o encapsulamento ao derivar o handler.
func (h Handler) WithGroup(name string) slog.Handler {
	return Handler{h.Handler.WithGroup(name)}
}
