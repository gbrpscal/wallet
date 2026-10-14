package httpapi

import (
	"context"
	"net/http"

	"github.com/gbrpscal/wallet/internal/domain"
)

// CorrelationHeader carrega o id de correlação entre o cliente e o serviço.
const CorrelationHeader = "X-Correlation-Id"

type correlationKey struct{}

// withCorrelation usa o X-Correlation-Id do cliente quando válido (1 a 255
// bytes, sem caracteres de controle nem espaço nas pontas); caso contrário,
// gera um UUIDv7. O valor volta no header da resposta e segue para logs,
// eventos e corpos de erro.
func withCorrelation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(CorrelationHeader)
		if domain.ValidateTextID("correlationId", id) != nil {
			generated, err := domain.NewID[domain.EventID]()
			if err == nil {
				id = generated.String()
			} else {
				id = "unavailable"
			}
		}
		w.Header().Set(CorrelationHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), correlationKey{}, id)))
	})
}

func correlationFrom(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	return id
}
