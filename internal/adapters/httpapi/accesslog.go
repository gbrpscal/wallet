package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// requestInfo é preenchido durante o atendimento (rota casada e identidade
// autenticada) e lido pelo access log ao final.
type requestInfo struct {
	route      string
	clientID   string
	providerID string
}

type requestInfoKey struct{}

func infoFrom(ctx context.Context) *requestInfo {
	info, _ := ctx.Value(requestInfoKey{}).(*requestInfo)
	return info
}

// routed registra o padrão da rota (ex.: /wallets/{walletId}), nunca a URL
// com ids, para o access log.
func routed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if info := infoFrom(r.Context()); info != nil {
			info.route = r.Pattern
		}
		next(w, r)
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// accessLog registra uma linha por requisição: método, rota, status, duração,
// clientId e providerId (o correlationId vem do contexto). Health checks vão
// para debug, para não poluir o log.
func accessLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &requestInfo{}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		ctx := context.WithValue(r.Context(), requestInfoKey{}, info)
		next.ServeHTTP(rec, r.WithContext(ctx))

		route := info.route
		if route == "" {
			route = "unmatched"
		}
		level := slog.LevelInfo
		if strings.HasPrefix(route, "GET /health/") {
			level = slog.LevelDebug
		}
		attrs := []any{"method", r.Method, "route", route, "status", rec.status, "durationMs", time.Since(start).Milliseconds()}
		if info.clientID != "" {
			attrs = append(attrs, "clientId", info.clientID)
		}
		if info.providerID != "" {
			attrs = append(attrs, "providerId", info.providerID)
		}
		log.Log(r.Context(), level, "http request", attrs...)
	})
}
