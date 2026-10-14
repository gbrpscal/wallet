//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gbrpscal/wallet/internal/adapters/httpapi"
	"github.com/gbrpscal/wallet/internal/adapters/postgres"
	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/platform/config"
	"github.com/gbrpscal/wallet/internal/testutil/pgtest"
)

var (
	server  *httptest.Server
	appPool *pgxpool.Pool
)

// TestMain sobe a API completa (rotas, autenticação, casos de uso) sobre um
// banco temporário, validando tokens do Keycloak real.
func TestMain(m *testing.M) {
	ctx := context.Background()
	db, err := pgtest.New(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	appPool = db.App

	cfg := config.Config{
		OIDCIssuer:       os.Getenv("OIDC_ISSUER"),
		OIDCJWKSURL:      os.Getenv("OIDC_JWKS_URL"),
		OIDCAudience:     os.Getenv("OIDC_AUDIENCE"),
		ReadinessTimeout: time.Second,
	}
	if cfg.OIDCIssuer == "" || cfg.OIDCJWKSURL == "" || os.Getenv("OIDC_TOKEN_URL") == "" {
		fmt.Fprintln(os.Stderr, "OIDC_ISSUER, OIDC_JWKS_URL and OIDC_TOKEN_URL are required")
		os.Exit(1)
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	svc := app.NewService(postgres.NewUnitOfWork(db.App, 5*time.Second), app.SystemClock{},
		app.DefaultRetryPolicy, app.DefaultReferencePolicy, log)
	api := httpapi.NewAPI(svc, httpapi.NewOIDCAuthenticator(cfg), log)
	health := httpapi.NewHealthHandler(httpapi.HealthParams{Config: cfg, Log: log})
	server = httptest.NewServer(httpapi.NewHandler(health, api, log))

	code := m.Run()
	server.Close()
	db.Close()
	os.Exit(code)
}

var (
	tokenMu sync.Mutex
	tokens  = map[string]string{}
)

// token obtém um access token real (client_credentials) e o reaproveita.
func token(t *testing.T, client string) string {
	t.Helper()
	tokenMu.Lock()
	defer tokenMu.Unlock()
	if tok, ok := tokens[client]; ok {
		return tok
	}
	tok := freshToken(t, client)
	tokens[client] = tok
	return tok
}

func freshToken(t *testing.T, client string) string {
	t.Helper()
	form := url.Values{"grant_type": {"client_credentials"}}
	req, _ := http.NewRequest(http.MethodPost, os.Getenv("OIDC_TOKEN_URL"), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(client, client+"-secret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("token request: %v", err)
	}
	defer res.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil || body.AccessToken == "" {
		t.Fatalf("token for %s: status %d, %v", client, res.StatusCode, err)
	}
	return body.AccessToken
}

type response struct {
	Status int
	Header http.Header
	Body   map[string]any
	Raw    string
}

// call faz uma requisição à API. client vazio envia sem Authorization.
func call(t *testing.T, method, path, client string, body any, headers ...string) response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			reader = strings.NewReader(b)
		default:
			raw, _ := json.Marshal(b)
			reader = bytes.NewReader(raw)
		}
	}
	req, _ := http.NewRequest(method, server.URL+path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if client != "" {
		req.Header.Set("Authorization", "Bearer "+token(t, client))
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	return do(t, req)
}

func do(t *testing.T, req *http.Request) response {
	t.Helper()
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	r := response{Status: res.StatusCode, Header: res.Header, Raw: string(raw)}
	_ = json.Unmarshal(raw, &r.Body)
	return r
}

func newPlayer(t *testing.T) string {
	t.Helper()
	id, err := domain.NewID[domain.PlayerID]()
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

func money(amount string) map[string]string {
	return map[string]string{"amount": amount, "currency": "BRL"}
}

// openWallet abre uma carteira pelo client interno e devolve (walletId, playerId).
func openWallet(t *testing.T, amount string) (string, string) {
	t.Helper()
	player := newPlayer(t)
	res := call(t, http.MethodPost, "/wallets", "wallet-backoffice",
		map[string]any{"playerId": player, "initialBalance": money(amount)})
	if res.Status != http.StatusCreated {
		t.Fatalf("open wallet: %d %s", res.Status, res.Raw)
	}
	return res.Body["id"].(string), player
}

// operation monta o corpo de uma operação no formato do enunciado.
func operation(provider, extID, walletID, playerID, kind, amount, ref string) map[string]any {
	body := map[string]any{
		"providerId": provider, "externalTransactionId": extID, "playerId": playerID, "walletId": walletID,
		"roundId": "round-987", "gameId": "fortune-chimp", "kind": kind, "money": money(amount),
	}
	if ref != "" {
		body["referenceExternalTransactionId"] = ref
	}
	return body
}

func submit(t *testing.T, client string, body map[string]any) response {
	t.Helper()
	key := body["providerId"].(string) + ":" + body["externalTransactionId"].(string)
	return call(t, http.MethodPost, "/wagering/transactions", client, body, "Idempotency-Key", key)
}

func ledgerCount(t *testing.T, walletID string) int {
	t.Helper()
	var n int
	if err := appPool.QueryRow(context.Background(), `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, walletID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func balanceOf(t *testing.T, walletID string) string {
	t.Helper()
	res := call(t, http.MethodGet, "/wallets/"+walletID, "wallet-backoffice", nil)
	if res.Status != http.StatusOK {
		t.Fatalf("get wallet: %d %s", res.Status, res.Raw)
	}
	return res.Body["balance"].(map[string]any)["amount"].(string)
}

func assertProblem(t *testing.T, r response, status int, code string) {
	t.Helper()
	if r.Status != status || r.Body["code"] != code {
		t.Fatalf("got %d %s, want %d %s", r.Status, r.Raw, status, code)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if r.Body["correlationId"] == "" || r.Body["correlationId"] == nil {
		t.Error("problem without correlationId")
	}
}

func newJSONRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(method, server.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}
