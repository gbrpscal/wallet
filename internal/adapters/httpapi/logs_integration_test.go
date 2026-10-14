//go:build integration

package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestAccessLog: cada requisição gera uma linha com o padrão da rota (sem
// ids), status, identidade autenticada e o correlationId do contexto.
func TestAccessLog(t *testing.T) {
	walletID, playerID := openWallet(t, "100.00")
	corr := "access-log-" + walletID
	call(t, http.MethodPost, "/wagering/transactions", "provider-a",
		operation("provider-a", "access-1", walletID, playerID, "BET", "1.00", ""),
		"Idempotency-Key", "provider-a:access-1", "X-Correlation-Id", corr+"-bet")
	call(t, http.MethodGet, "/wallets/"+walletID, "", nil, "X-Correlation-Id", corr+"-anon")
	call(t, http.MethodGet, "/nowhere", "", nil, "X-Correlation-Id", corr+"-unmatched")
	call(t, http.MethodGet, "/health/live", "", nil, "X-Correlation-Id", corr+"-health")

	cases := []struct {
		suffix string
		want   map[string]any
	}{
		{"-bet", map[string]any{"level": "INFO", "method": "POST", "route": "POST /wagering/transactions", "status": float64(200),
			"clientId": "provider-a", "providerId": "provider-a"}},
		{"-anon", map[string]any{"level": "INFO", "route": "GET /wallets/{walletId}", "status": float64(401)}},
		{"-unmatched", map[string]any{"level": "INFO", "route": "unmatched", "status": float64(404)}},
		{"-health", map[string]any{"level": "DEBUG", "route": "GET /health/live", "status": float64(200)}},
	}
	for _, c := range cases {
		entry := accessEntry(t, corr+c.suffix)
		for k, v := range c.want {
			if entry[k] != v {
				t.Errorf("%s: %s = %v, want %v (%v)", c.suffix, k, entry[k], v, entry)
			}
		}
		if _, ok := entry["durationMs"]; !ok {
			t.Errorf("%s: durationMs missing", c.suffix)
		}
		if c.suffix == "-anon" {
			if _, ok := entry["clientId"]; ok {
				t.Errorf("unauthenticated request logged a clientId: %v", entry)
			}
		}
	}
}

func accessEntry(t *testing.T, correlationID string) map[string]any {
	t.Helper()
	for _, l := range logs.lines(`"correlationId":"` + correlationID + `"`) {
		var entry map[string]any
		if err := json.Unmarshal([]byte(l), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["msg"] == "http request" {
			return entry
		}
	}
	t.Fatalf("no access log line for %s", correlationID)
	return nil
}

// TestLogsCarryNoCredentialsOrMoney: nenhuma linha de log contém token,
// header Authorization ou valores das operações, inclusive em falhas.
func TestLogsCarryNoCredentialsOrMoney(t *testing.T) {
	walletID, playerID := openWallet(t, "4321.00")
	corr := "secrets-" + walletID
	call(t, http.MethodPost, "/wagering/transactions", "provider-a",
		operation("provider-a", "secret-1", walletID, playerID, "BET", "1234.56", ""),
		"Idempotency-Key", "provider-a:secret-1", "X-Correlation-Id", corr)
	call(t, http.MethodPost, "/wagering/transactions", "provider-a",
		operation("provider-a", "secret-2", walletID, playerID, "BET", "9876.54", ""),
		"Idempotency-Key", "provider-a:secret-2", "X-Correlation-Id", corr)
	call(t, http.MethodGet, "/wallets/"+walletID, "", nil, "X-Correlation-Id", corr,
		"Authorization", "Bearer not-a-real-token-xyz")
	call(t, http.MethodPost, "/wagering/transactions", "provider-a", `{"amount":"5555.55"`,
		"Idempotency-Key", "provider-a:secret-3", "X-Correlation-Id", corr)

	lines := logs.lines(`"correlationId":"` + corr + `"`)
	if len(lines) < 4 {
		t.Fatalf("expected at least 4 log lines for the scenario, got %d", len(lines))
	}
	all := logs.lines("")
	forbidden := []string{token(t, "provider-a"), "not-a-real-token-xyz", "Bearer", "Authorization",
		"provider-a-secret", "4321.00", "1234.56", "3086.44", "9876.54", "5555.55", ":432100", ":123456", "\"432100\"", "\"123456\""}
	for _, l := range all {
		for _, f := range forbidden {
			if strings.Contains(l, f) {
				t.Errorf("log line carries %q: %s", f, l)
			}
		}
	}
}
