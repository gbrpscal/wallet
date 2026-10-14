//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSpecificationFlow percorre os exemplos do enunciado: abertura de
// 1000.00, aposta de 25.00, replay e reconciliação com checkedEntries 2.
func TestSpecificationFlow(t *testing.T) {
	walletID, playerID := openWallet(t, "1000.00")
	got := call(t, http.MethodGet, "/wallets/"+walletID, "wallet-backoffice", nil)
	if got.Body["version"] != float64(1) || got.Body["playerId"] != playerID || balanceOf(t, walletID) != "1000.00" {
		t.Fatalf("wallet = %s", got.Raw)
	}

	bet := operation("provider-a", "transaction-123", walletID, playerID, "BET", "25.00", "")
	first := submit(t, "provider-a", bet)
	if first.Status != http.StatusOK || first.Body["status"] != "PROCESSED" || first.Body["idempotentReplay"] != false ||
		first.Body["balance"].(map[string]any)["amount"] != "975.00" {
		t.Fatalf("bet = %d %s", first.Status, first.Raw)
	}
	again := submit(t, "provider-a", bet)
	if again.Status != http.StatusOK || again.Body["idempotentReplay"] != true || again.Body["transactionId"] != first.Body["transactionId"] {
		t.Fatalf("replay = %d %s", again.Status, again.Raw)
	}

	rec := call(t, http.MethodPost, "/wallets/"+walletID+"/reconciliation", "wallet-backoffice", nil)
	want := `"storedBalance":{"amount":"975.00","currency":"BRL"},"calculatedBalance":{"amount":"975.00","currency":"BRL"},"difference":{"amount":"0.00","currency":"BRL"},"consistent":true,"checkedEntries":2`
	if rec.Status != http.StatusOK || !strings.Contains(rec.Raw, want) {
		t.Fatalf("reconciliation = %d %s", rec.Status, rec.Raw)
	}

	txID := first.Body["transactionId"].(string)
	byID := call(t, http.MethodGet, "/wagering/transactions/"+txID, "provider-a", nil)
	byExt := call(t, http.MethodGet, "/providers/provider-a/wagering/transactions/transaction-123", "provider-a", nil)
	for _, r := range []response{byID, byExt} {
		if r.Status != http.StatusOK || r.Body["transactionId"] != txID || r.Body["status"] != "PROCESSED" || r.Body["kind"] != "BET" {
			t.Fatalf("transaction lookup = %d %s", r.Status, r.Raw)
		}
	}

	page := call(t, http.MethodGet, "/wallets/"+walletID+"/ledger?limit=1", "wallet-backoffice", nil)
	entries := page.Body["entries"].([]any)
	if page.Status != http.StatusOK || len(entries) != 1 || page.Body["nextCursor"] == nil {
		t.Fatalf("ledger page 1 = %s", page.Raw)
	}
	next := call(t, http.MethodGet, "/wallets/"+walletID+"/ledger?limit=1&cursor="+page.Body["nextCursor"].(string), "wallet-backoffice", nil)
	if len(next.Body["entries"].([]any)) != 1 || next.Body["nextCursor"] != nil {
		t.Fatalf("ledger page 2 = %s", next.Raw)
	}
}

func TestWalletRules(t *testing.T) {
	player := newPlayer(t)
	first := call(t, http.MethodPost, "/wallets", "wallet-backoffice", map[string]any{"playerId": player, "initialBalance": money("0.00")})
	if first.Status != http.StatusCreated || first.Body["version"] != float64(1) || first.Header.Get("Location") == "" {
		t.Fatalf("zero opening = %d %s", first.Status, first.Raw)
	}
	if n := ledgerCount(t, first.Body["id"].(string)); n != 0 {
		t.Fatalf("zero opening created %d ledger entries", n)
	}
	dup := call(t, http.MethodPost, "/wallets", "wallet-backoffice", map[string]any{"playerId": player, "initialBalance": money("5.00")})
	assertProblem(t, dup, http.StatusConflict, "WALLET_ALREADY_EXISTS")

	missing := call(t, http.MethodGet, "/wallets/0192f291-27dd-7d3f-8071-000000000009", "wallet-backoffice", nil)
	assertProblem(t, missing, http.StatusNotFound, "WALLET_NOT_FOUND")
	badID := call(t, http.MethodGet, "/wallets/not-a-uuid", "wallet-backoffice", nil)
	assertProblem(t, badID, http.StatusBadRequest, "INVALID_REQUEST")
}

func TestOperationResponses(t *testing.T) {
	walletID, playerID := openWallet(t, "100.00")

	rejected := submit(t, "provider-a", operation("provider-a", "too-big", walletID, playerID, "BET", "500.00", ""))
	if rejected.Status != http.StatusUnprocessableEntity || rejected.Body["status"] != "REJECTED" ||
		rejected.Body["failureCode"] != "INSUFFICIENT_FUNDS" || rejected.Body["transactionId"] == nil {
		t.Fatalf("rejection = %d %s", rejected.Status, rejected.Raw)
	}
	replayed := submit(t, "provider-a", operation("provider-a", "too-big", walletID, playerID, "BET", "500.00", ""))
	if replayed.Status != http.StatusUnprocessableEntity || replayed.Body["idempotentReplay"] != true {
		t.Fatalf("replayed rejection = %d %s", replayed.Status, replayed.Raw)
	}

	pending := submit(t, "provider-a", operation("provider-a", "early-refund", walletID, playerID, "REFUND", "10.00", "late-bet"))
	if pending.Status != http.StatusAccepted || pending.Body["status"] != "PENDING_REFERENCE" ||
		pending.Header.Get("Location") != "/wagering/transactions/"+pending.Body["transactionId"].(string) {
		t.Fatalf("pending = %d %s", pending.Status, pending.Raw)
	}
	follow := call(t, http.MethodGet, pending.Header.Get("Location"), "provider-a", nil)
	if follow.Status != http.StatusOK || follow.Body["status"] != "PENDING_REFERENCE" {
		t.Fatalf("follow pending = %d %s", follow.Status, follow.Raw)
	}

	loss := submit(t, "provider-a", operation("provider-a", "loss-1", walletID, playerID, "LOSS", "0.00", ""))
	if loss.Status != http.StatusOK || loss.Body["status"] != "PROCESSED" {
		t.Fatalf("loss = %d %s", loss.Status, loss.Raw)
	}
}

func TestInputAndConflictErrors(t *testing.T) {
	walletID, playerID := openWallet(t, "100.00")
	valid := operation("provider-a", "err-1", walletID, playerID, "BET", "10.00", "")

	noKey := call(t, http.MethodPost, "/wagering/transactions", "provider-a", valid)
	assertProblem(t, noKey, http.StatusBadRequest, "INVALID_REQUEST")
	// A mensagem aponta o header que falta, não um campo interno.
	if detail, _ := noKey.Body["detail"].(string); !strings.Contains(detail, "Idempotency-Key header") {
		t.Errorf("missing key detail = %q", detail)
	}

	cases := map[string]struct {
		body   any
		status int
		code   string
	}{
		"unknown field":       {strings.Replace(mustJSON(valid), `"kind"`, `"refrenceExternalTransactionId":"x","kind"`, 1), 400, "INVALID_REQUEST"},
		"numeric amount":      {strings.Replace(mustJSON(valid), `"amount":"10.00"`, `"amount":10.00`, 1), 400, "INVALID_AMOUNT"},
		"scientific amount":   {strings.Replace(mustJSON(valid), `"10.00"`, `"1e1"`, 1), 400, "INVALID_AMOUNT"},
		"two objects":         {mustJSON(valid) + `{}`, 400, "INVALID_REQUEST"},
		"malformed":           {`{"providerId":`, 400, "INVALID_REQUEST"},
		"opening by provider": {strings.Replace(mustJSON(valid), `"BET"`, `"OPENING"`, 1), 400, "KIND_NOT_ALLOWED"},
		"zero bet":            {strings.Replace(mustJSON(valid), `"10.00"`, `"0.00"`, 1), 400, "AMOUNT_MUST_BE_POSITIVE"},
		"unknown wallet":      {strings.Replace(mustJSON(valid), walletID, "0192f291-27dd-7d3f-8071-00000000000a", 1), 404, "WALLET_NOT_FOUND"},
	}
	for name, c := range cases {
		r := call(t, http.MethodPost, "/wagering/transactions", "provider-a", c.body, "Idempotency-Key", "provider-a:err-1")
		if r.Status != c.status || r.Body["code"] != c.code {
			t.Errorf("%s: %d %s, want %d %s", name, r.Status, r.Raw, c.status, c.code)
		}
	}

	wrongType := call(t, http.MethodPost, "/wagering/transactions", "provider-a", nil, "Idempotency-Key", "k", "Content-Type", "text/plain")
	assertProblem(t, wrongType, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE")

	// Nenhum dos erros acima consumiu a chave.
	if r := submit(t, "provider-a", valid); r.Status != http.StatusOK || r.Body["idempotentReplay"] != false {
		t.Fatalf("valid request after errors = %d %s", r.Status, r.Raw)
	}
	changed := operation("provider-a", "err-1", walletID, playerID, "BET", "11.00", "")
	assertProblem(t, submit(t, "provider-a", changed), http.StatusConflict, "IDEMPOTENCY_KEY_CONFLICT")
	otherKey := call(t, http.MethodPost, "/wagering/transactions", "provider-a", valid, "Idempotency-Key", "another-key")
	assertProblem(t, otherKey, http.StatusConflict, "EXTERNAL_TRANSACTION_CONFLICT")
	if balanceOf(t, walletID) != "90.00" {
		t.Fatalf("balance = %s", balanceOf(t, walletID))
	}
}

// mustJSON serializa o corpo (chaves em ordem alfabética) para variações textuais.
func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// TestAuthenticationFailures: sem token, token malformado, assinatura
// adulterada, audiência errada e token expirado; nenhum produz efeito.
func TestAuthenticationFailures(t *testing.T) {
	walletID, playerID := openWallet(t, "100.00")
	body := operation("provider-a", "auth-1", walletID, playerID, "BET", "10.00", "")
	post := func(auth string) response {
		req := newJSONRequest(t, http.MethodPost, "/wagering/transactions", body)
		req.Header.Set("Idempotency-Key", "provider-a:auth-1")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		return do(t, req)
	}

	valid := freshToken(t, "provider-a")
	parts := strings.Split(valid, ".")
	tampered := parts[0] + "." + parts[1] + "." + strings.Repeat("A", len(parts[2]))
	short := freshToken(t, "provider-short-lived")
	time.Sleep(3 * time.Second)

	cases := map[string]string{
		"missing":        "",
		"not bearer":     "Basic " + valid,
		"malformed":      "Bearer not-a-jwt",
		"bad signature":  "Bearer " + tampered,
		"wrong audience": "Bearer " + freshToken(t, "no-audience"),
		"expired":        "Bearer " + short,
	}
	for name, auth := range cases {
		r := post(auth)
		if r.Status != http.StatusUnauthorized || r.Body["code"] != "UNAUTHENTICATED" || !strings.HasPrefix(r.Header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s: %d %s", name, r.Status, r.Raw)
		}
	}
	if n := ledgerCount(t, walletID); n != 1 {
		t.Fatalf("unauthenticated requests moved money: %d entries", n)
	}
	if r := post("Bearer " + valid); r.Status != http.StatusOK {
		t.Fatalf("valid token rejected: %d %s", r.Status, r.Raw)
	}
}

// TestAuthorizationByScope: cada client só acessa as operações dos seus scopes.
func TestAuthorizationByScope(t *testing.T) {
	walletID, playerID := openWallet(t, "100.00")
	cases := []struct {
		name, client, method, path string
		body                       any
	}{
		{"provider opens wallet", "provider-a", http.MethodPost, "/wallets", map[string]any{"playerId": newPlayer(t), "initialBalance": money("1.00")}},
		{"provider reads wallet", "provider-a", http.MethodGet, "/wallets/" + walletID, nil},
		{"provider reads ledger", "provider-a", http.MethodGet, "/wallets/" + walletID + "/ledger", nil},
		{"provider reconciles", "provider-a", http.MethodPost, "/wallets/" + walletID + "/reconciliation", nil},
		{"internal submits operation", "wallet-backoffice", http.MethodPost, "/wagering/transactions",
			operation("provider-a", "scope-1", walletID, playerID, "BET", "1.00", "")},
		{"internal reads transaction", "wallet-backoffice", http.MethodGet, "/providers/provider-a/wagering/transactions/scope-1", nil},
	}
	for _, c := range cases {
		r := call(t, c.method, c.path, c.client, c.body, "Idempotency-Key", "provider-a:scope-1")
		if r.Status != http.StatusForbidden || r.Body["code"] != "FORBIDDEN" ||
			!strings.Contains(r.Header.Get("WWW-Authenticate"), "insufficient_scope") {
			t.Errorf("%s: %d %s", c.name, r.Status, r.Raw)
		}
	}
	if n := ledgerCount(t, walletID); n != 1 {
		t.Fatalf("forbidden requests moved money: %d entries", n)
	}
}

// TestProviderIsolation: um provedor não opera, não consulta e não faz replay
// em nome de outro.
func TestProviderIsolation(t *testing.T) {
	walletID, playerID := openWallet(t, "100.00")
	mine := submit(t, "provider-a", operation("provider-a", "iso-1", walletID, playerID, "BET", "10.00", ""))
	if mine.Status != http.StatusOK {
		t.Fatalf("provider-a bet = %d %s", mine.Status, mine.Raw)
	}
	txID := mine.Body["transactionId"].(string)

	// provider-b tentando operar como provider-a.
	spoof := submit(t, "provider-b", operation("provider-a", "iso-2", walletID, playerID, "BET", "10.00", ""))
	assertProblem(t, spoof, http.StatusForbidden, "FORBIDDEN")
	// provider-b tentando o replay da operação de provider-a.
	replay := submit(t, "provider-b", operation("provider-a", "iso-1", walletID, playerID, "BET", "10.00", ""))
	assertProblem(t, replay, http.StatusForbidden, "FORBIDDEN")
	// provider-b consultando a transação de provider-a: 404, sem revelar a existência.
	byID := call(t, http.MethodGet, "/wagering/transactions/"+txID, "provider-b", nil)
	assertProblem(t, byID, http.StatusNotFound, "TRANSACTION_NOT_FOUND")
	if strings.Contains(byID.Raw, "provider-a") || strings.Contains(byID.Raw, walletID) {
		t.Fatalf("404 leaked data: %s", byID.Raw)
	}
	byExt := call(t, http.MethodGet, "/providers/provider-a/wagering/transactions/iso-1", "provider-b", nil)
	assertProblem(t, byExt, http.StatusForbidden, "FORBIDDEN")
	ownScope := call(t, http.MethodGet, "/providers/provider-b/wagering/transactions/iso-1", "provider-b", nil)
	assertProblem(t, ownScope, http.StatusNotFound, "TRANSACTION_NOT_FOUND")

	// O OPENING interno também não é visível para provedores.
	var openingID string
	_ = appPool.QueryRow(context.Background(), `SELECT id::text FROM wager_transactions WHERE wallet_id = $1 AND kind = 'OPENING'`, walletID).Scan(&openingID)
	assertProblem(t, call(t, http.MethodGet, "/wagering/transactions/"+openingID, "provider-a", nil), http.StatusNotFound, "TRANSACTION_NOT_FOUND")

	// A mesma chave e o mesmo id externo em nome de provider-b são operações independentes.
	own := submit(t, "provider-b", operation("provider-b", "iso-1", walletID, playerID, "BET", "10.00", ""))
	if own.Status != http.StatusOK || own.Body["idempotentReplay"] != false || own.Body["transactionId"] == txID {
		t.Fatalf("provider-b own operation = %d %s", own.Status, own.Raw)
	}
	if balanceOf(t, walletID) != "80.00" {
		t.Fatalf("balance = %s", balanceOf(t, walletID))
	}
}

func TestCorrelationID(t *testing.T) {
	walletID, playerID := openWallet(t, "100.00")
	r := call(t, http.MethodPost, "/wagering/transactions", "provider-a",
		operation("provider-a", "corr-1", walletID, playerID, "BET", "1.00", ""),
		"Idempotency-Key", "provider-a:corr-1", "X-Correlation-Id", "client-trace-42")
	if r.Header.Get("X-Correlation-Id") != "client-trace-42" {
		t.Fatalf("correlation header = %q", r.Header.Get("X-Correlation-Id"))
	}
	var stored string
	_ = appPool.QueryRow(context.Background(), `SELECT correlation_id FROM wager_transactions WHERE id = $1`, r.Body["transactionId"]).Scan(&stored)
	var evt string
	_ = appPool.QueryRow(context.Background(), `SELECT payload::jsonb->>'correlationId' FROM outbox_events WHERE aggregate_id = $1 LIMIT 1`, r.Body["transactionId"]).Scan(&evt)
	if stored != "client-trace-42" || evt != "client-trace-42" {
		t.Fatalf("stored %q, event %q", stored, evt)
	}

	generated := call(t, http.MethodGet, "/health/live", "", nil, "X-Correlation-Id", strings.Repeat("x", 300))
	if id := generated.Header.Get("X-Correlation-Id"); len(id) != 36 {
		t.Fatalf("invalid client id was not replaced: %q", id)
	}
}

func TestHealthIsPublic(t *testing.T) {
	if r := call(t, http.MethodGet, "/health/live", "", nil); r.Status != http.StatusOK {
		t.Fatalf("live = %d", r.Status)
	}
}
