package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/gbrpscal/wallet/internal/platform/config"
	"github.com/gbrpscal/wallet/internal/platform/logctx"
)

// Scopes OAuth 2.0 exigidos por operação (D8).
const (
	ScopeWageringWrite    = "wagering:write"
	ScopeWageringRead     = "wagering:read"
	ScopeWalletsRead      = "wallets:read"
	ScopeWalletsWrite     = "wallets:write"
	ScopeWalletsReconcile = "wallets:reconcile"
)

// Principal é a identidade autenticada de um client (client_credentials).
type Principal struct {
	ClientID string
	// ProviderID vem do claim provider_id; vazio para clients internos.
	ProviderID string
	Scopes     map[string]bool
}

// Has informa se o token concede o scope.
func (p Principal) Has(scope string) bool { return p.Scopes[scope] }

// Authenticator valida um bearer token e devolve o principal.
type Authenticator interface {
	Authenticate(ctx context.Context, rawToken string) (Principal, error)
}

// errInvalidToken cobre token ausente, malformado, com assinatura, emissor ou
// audiência inválidos, ou expirado. O motivo exato vai só para o log.
var errInvalidToken = errors.New("invalid token")

// OIDCAuthenticator valida JWTs do Keycloak: assinatura RS256 pelas chaves do
// JWKS (com cache e rotação), emissor, audiência e expiração.
type OIDCAuthenticator struct {
	verifier *oidc.IDTokenVerifier
}

// NewOIDCAuthenticator cria o autenticador. As chaves são buscadas sob
// demanda, então o processo sobe mesmo com o IdP fora do ar; nesse caso as
// requisições autenticadas falham com 401 até o IdP voltar.
func NewOIDCAuthenticator(cfg config.Config) *OIDCAuthenticator {
	keys := oidc.NewRemoteKeySet(context.Background(), cfg.OIDCJWKSURL)
	return &OIDCAuthenticator{verifier: oidc.NewVerifier(cfg.OIDCIssuer, keys, &oidc.Config{
		ClientID:             cfg.OIDCAudience,
		SupportedSigningAlgs: []string{oidc.RS256},
	})}
}

type accessTokenClaims struct {
	AuthorizedParty string `json:"azp"`
	Scope           string `json:"scope"`
	ProviderID      string `json:"provider_id"`
}

// Authenticate valida o token e extrai client, provedor e scopes.
func (a *OIDCAuthenticator) Authenticate(ctx context.Context, raw string) (Principal, error) {
	token, err := a.verifier.Verify(ctx, raw)
	if err != nil {
		return Principal{}, errors.Join(errInvalidToken, err)
	}
	var claims accessTokenClaims
	if err := token.Claims(&claims); err != nil {
		return Principal{}, errors.Join(errInvalidToken, err)
	}
	p := Principal{ClientID: claims.AuthorizedParty, ProviderID: claims.ProviderID, Scopes: map[string]bool{}}
	for _, s := range strings.Fields(claims.Scope) {
		p.Scopes[s] = true
	}
	return p, nil
}

type principalKey struct{}

// principalFrom devolve o principal autenticado da requisição.
func principalFrom(ctx context.Context) Principal {
	p, _ := ctx.Value(principalKey{}).(Principal)
	return p
}

// requireScope autentica a requisição e exige o scope. Sem token válido: 401
// com WWW-Authenticate; token válido sem o scope: 403. Em nenhum dos casos o
// handler roda, então não há efeito financeiro nem exposição de dados.
func (a *API) requireScope(scope string, next http.HandlerFunc) http.Handler {
	return routed(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="wallet"`)
			a.problem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "missing bearer token")
			return
		}
		p, err := a.auth.Authenticate(r.Context(), raw)
		if err != nil {
			a.log.InfoContext(r.Context(), "authentication failed", "error", err)
			w.Header().Set("WWW-Authenticate", `Bearer realm="wallet", error="invalid_token"`)
			a.problem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "invalid or expired token")
			return
		}
		if info := infoFrom(r.Context()); info != nil {
			info.clientID, info.providerID = p.ClientID, p.ProviderID
		}
		ctx := logctx.With(r.Context(), logctx.ClientID, p.ClientID)
		if p.ProviderID != "" {
			ctx = logctx.With(ctx, logctx.ProviderID, p.ProviderID)
		}
		r = r.WithContext(ctx)
		if !p.Has(scope) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="wallet", error="insufficient_scope", scope="`+scope+`"`)
			a.problem(w, r, http.StatusForbidden, CodeForbidden, "token lacks the required scope "+scope)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}
