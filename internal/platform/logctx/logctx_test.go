package logctx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/gbrpscal/wallet/internal/platform/logctx"
)

func TestContextAttributesReachEveryLine(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(logctx.Handler{Handler: slog.NewJSONHandler(&buf, nil)})

	ctx := logctx.With(context.Background(), logctx.CorrelationID, "corr-1", logctx.WalletID, "w-1")
	ctx = logctx.With(ctx, logctx.TransactionID, "t-1", logctx.WalletID, "w-2") // substitui walletId
	log.InfoContext(ctx, "processed", "status", "PROCESSED")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"correlationId": "corr-1", "transactionId": "t-1", "walletId": "w-2", "status": "PROCESSED"}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("%s = %v, want %v (line %s)", k, line[k], v, buf.String())
		}
	}
	if n := bytes.Count(buf.Bytes(), []byte(`"walletId"`)); n != 1 {
		t.Errorf("walletId appears %d times; a repeated key must replace the previous value", n)
	}
}

func TestContextIsNotMutated(t *testing.T) {
	parent := logctx.With(context.Background(), logctx.CorrelationID, "parent")
	_ = logctx.With(parent, logctx.CorrelationID, "child")
	attrs := logctx.Attrs(parent)
	if len(attrs) != 1 || attrs[0].Value.String() != "parent" {
		t.Fatalf("parent context changed: %v", attrs)
	}
}
