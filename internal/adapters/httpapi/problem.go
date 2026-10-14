package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"

	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
)

// Códigos de erro próprios da camada HTTP; os de negócio vêm de domain.Code.
const (
	CodeUnauthenticated      domain.Code = "UNAUTHENTICATED"
	CodeForbidden            domain.Code = "FORBIDDEN"
	CodeUnavailable          domain.Code = "UNAVAILABLE"
	CodeInternal             domain.Code = "INTERNAL_ERROR"
	CodeUnsupportedMediaType domain.Code = "UNSUPPORTED_MEDIA_TYPE"
)

// problemDoc é o corpo RFC 9457 (application/problem+json), com as extensões
// code (estável, para máquinas) e correlationId.
type problemDoc struct {
	Type          string `json:"type"`
	Title         string `json:"title"`
	Status        int    `json:"status"`
	Detail        string `json:"detail,omitempty"`
	Code          string `json:"code"`
	CorrelationID string `json:"correlationId,omitempty"`
}

func (a *API) problem(w http.ResponseWriter, r *http.Request, status int, code domain.Code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problemDoc{
		Type:          "urn:wallet:error:" + string(code),
		Title:         http.StatusText(status),
		Status:        status,
		Detail:        detail,
		Code:          string(code),
		CorrelationID: correlationFrom(r.Context()),
	})
}

// fail mapeia um erro dos casos de uso para status e corpo (D6):
//
//	KindValidation  -> 400   entrada corrigível; nada persistido
//	KindNotFound    -> 404
//	KindConflict    -> 409   idempotência, operação com outra chave, carteira duplicada
//	KindRejection   -> 422   (rejeições persistidas respondem com o corpo da transação)
//	transitório     -> 503   com Retry-After; nada confirmado, pode repetir
//	KindInvariant e demais -> 500, sem detalhes internos na resposta
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	var de *domain.Error
	if errors.As(err, &de) {
		switch de.Kind {
		case domain.KindValidation:
			a.problem(w, r, http.StatusBadRequest, de.Code, de.Message)
			return
		case domain.KindNotFound:
			a.problem(w, r, http.StatusNotFound, de.Code, de.Message)
			return
		case domain.KindConflict:
			a.problem(w, r, http.StatusConflict, de.Code, de.Message)
			return
		case domain.KindRejection:
			a.problem(w, r, http.StatusUnprocessableEntity, de.Code, de.Message)
			return
		}
	}
	if app.IsRetryable(err) || errors.Is(err, app.ErrDuplicate) || errors.Is(err, context.DeadlineExceeded) {
		a.log.WarnContext(r.Context(), "transient failure", "error", err)
		w.Header().Set("Retry-After", "1")
		a.problem(w, r, http.StatusServiceUnavailable, CodeUnavailable, "temporarily unavailable, retry later")
		return
	}
	a.log.ErrorContext(r.Context(), "request failed", "error", err)
	code := CodeInternal
	if de != nil {
		code = de.Code
	}
	a.problem(w, r, http.StatusInternalServerError, code, "internal error")
}

// maxBodyBytes limita o corpo das requisições.
const maxBodyBytes = 64 << 10

// decodeJSON lê exatamente um objeto JSON: Content-Type application/json,
// até 64 KiB, sem campos desconhecidos e sem conteúdo depois do objeto.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return errUnsupportedMediaType
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var de *domain.Error
		if errors.As(err, &de) {
			return de
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return domain.Errorf(domain.ErrInvalidRequest, "request body exceeds %d bytes", maxBodyBytes)
		}
		return domain.Errorf(domain.ErrInvalidRequest, "malformed JSON body: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return domain.Errorf(domain.ErrInvalidRequest, "request body must contain a single JSON object")
	}
	return nil
}

var errUnsupportedMediaType = errors.New("unsupported media type")

// failDecode responde a um erro de decodificação.
func (a *API) failDecode(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errUnsupportedMediaType) {
		a.problem(w, r, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	a.fail(w, r, err)
}

// recoverer transforma um panic em 500, sem derrubar o processo.
func recoverer(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.ErrorContext(r.Context(), "panic in handler", "panic", v)
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(problemDoc{Type: "urn:wallet:error:" + string(CodeInternal),
					Title: http.StatusText(http.StatusInternalServerError), Status: http.StatusInternalServerError,
					Code: string(CodeInternal), CorrelationID: correlationFrom(r.Context())})
			}
		}()
		next.ServeHTTP(w, r)
	})
}
