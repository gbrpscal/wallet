//go:build integration

package postgres_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"

	"github.com/gbrpscal/wallet/internal/domain"
)

func countTables(t *testing.T, ctx context.Context, dsn string) int {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var n int
	err = conn.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name IN
		  ('wallets', 'wager_transactions', 'wallet_ledger_entries', 'inbox_messages', 'outbox_events')`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestMigrationsApplyRevertAndReapply aplica todas as migrations, reverte até
// a versão 0 e aplica de novo, num banco próprio.
func TestMigrationsApplyRevertAndReapply(t *testing.T) {
	ctx := context.Background()
	ownerURL := os.Getenv("DATABASE_OWNER_URL")
	name, err := createDatabase(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer dropDatabase(ownerURL, name)
	dsn := withDatabase(ownerURL, name)

	steps := []struct {
		name   string
		run    func(*goose.Provider) error
		tables int
	}{
		{"up", func(p *goose.Provider) error { _, err := p.Up(ctx); return err }, 5},
		{"down to 0", func(p *goose.Provider) error { _, err := p.DownTo(ctx, 0); return err }, 0},
		{"up again", func(p *goose.Provider) error { _, err := p.Up(ctx); return err }, 5},
	}
	for _, s := range steps {
		if err := migrate(ctx, dsn, s.run); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if got := countTables(t, ctx, dsn); got != s.tables {
			t.Fatalf("%s: %d tables, want %d", s.name, got, s.tables)
		}
	}
}

func TestLedgerIsAppendOnly(t *testing.T) {
	ctx := context.Background()
	out := openWallet(t, "100.00")
	id := out.Entry.ID().String()

	statements := map[string]string{
		"update":   `UPDATE wallet_ledger_entries SET amount_minor = 1 WHERE id = '` + id + `'`,
		"delete":   `DELETE FROM wallet_ledger_entries WHERE id = '` + id + `'`,
		"truncate": `TRUNCATE wallet_ledger_entries CASCADE`,
	}
	for name, sql := range statements {
		// Papel da aplicação: sem privilégio.
		if _, err := appPool.Exec(ctx, sql); pgCode(err) != codeInsufficient {
			t.Errorf("app %s: error = %v, want permission denied", name, err)
		}
		// Dono do schema: barrado pelo trigger.
		if _, err := ownerPool.Exec(ctx, sql); pgCode(err) != codeIntegrity {
			t.Errorf("owner %s: error = %v, want trigger rejection", name, err)
		}
	}
	if entries := ledgerOf(t, out.Wallet.ID()); len(entries) != 1 || entries[0].Amount().MinorUnits() != 10000 {
		t.Fatalf("ledger changed: %+v", entries)
	}
}

func TestWalletsAndTransactionsCannotBeDeleted(t *testing.T) {
	ctx := context.Background()
	out := openWallet(t, "100.00")
	for _, sql := range []string{
		`DELETE FROM wallets WHERE id = '` + out.Wallet.ID().String() + `'`,
		`DELETE FROM wager_transactions WHERE id = '` + out.Opening.ID().String() + `'`,
	} {
		if _, err := ownerPool.Exec(ctx, sql); pgCode(err) != codeIntegrity {
			t.Errorf("%s: error = %v, want trigger rejection", sql, err)
		}
		if _, err := appPool.Exec(ctx, sql); pgCode(err) != codeInsufficient {
			t.Errorf("app %s: error = %v, want permission denied", sql, err)
		}
	}
}

func TestBalanceCannotBeNegative(t *testing.T) {
	ctx := context.Background()
	out := openWallet(t, "0.00")
	_, err := appPool.Exec(ctx, `UPDATE wallets SET balance_minor = -1, version = 2 WHERE id = $1`, out.Wallet.ID().String())
	if pgCode(err) != codeCheck {
		t.Fatalf("negative balance: error = %v, want check violation", err)
	}
}

// TestBalanceChangeRequiresLedgerEntry mostra que nem mesmo um UPDATE válido
// isoladamente (versão +1) é confirmado sem o lançamento correspondente.
func TestBalanceChangeRequiresLedgerEntry(t *testing.T) {
	ctx := context.Background()
	out := openWallet(t, "100.00")
	_, err := appPool.Exec(ctx, `UPDATE wallets SET balance_minor = 50000, version = 2 WHERE id = $1`, out.Wallet.ID().String())
	if pgCode(err) != codeIntegrity {
		t.Fatalf("balance without ledger committed: error = %v", err)
	}
	if w := getWallet(t, out.Wallet.ID()); w.Balance().Amount() != "100.00" || w.Version() != 1 {
		t.Fatalf("wallet changed: %v v%d", w.Balance(), w.Version())
	}
}

func TestWalletVersionRules(t *testing.T) {
	ctx := context.Background()
	out := openWallet(t, "100.00")
	id := out.Wallet.ID().String()
	// Dentro de uma transação, para que o erro venha do trigger no próprio
	// UPDATE, e não da conferência diferida do commit.
	for name, c := range map[string]struct{ sql, reason string }{
		"balance without version": {`UPDATE wallets SET balance_minor = 9000 WHERE id = '` + id + `'`, "must increment version by one"},
		"version skips":           {`UPDATE wallets SET balance_minor = 9000, version = 3 WHERE id = '` + id + `'`, "must increment version by one"},
		"version without balance": {`UPDATE wallets SET version = 2 WHERE id = '` + id + `'`, "changes only with its balance"},
		"identity change":         {`UPDATE wallets SET currency = 'USD' WHERE id = '` + id + `'`, "is immutable"},
	} {
		tx, err := appPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, c.sql)
		_ = tx.Rollback(ctx)
		if pgCode(err) != codeIntegrity || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: error = %v, want trigger rejection %q", name, err, c.reason)
		}
	}
}

func TestLedgerChainIsEnforced(t *testing.T) {
	ctx := context.Background()
	out := openWallet(t, "100.00")
	w := out.Wallet
	bet := mustProcess(t, operation(t, w, "provider-a", "bet-chain", "BET", "10.00", ""))

	// Uma BET pendente de 10.00, sem lançamento, para tentar lançamentos
	// com cadeia inválida.
	pending := mustProcessPending(t, w, "bet-chain-pending")

	insert := `
		INSERT INTO wallet_ledger_entries (id, wallet_id, transaction_id, direction, amount_minor, currency,
			balance_before_minor, balance_after_minor, wallet_version, created_at)
		VALUES (gen_random_uuid(), $1, $2, 'DEBIT', 1000, 'BRL', $3, $4, $5, now())`

	_, err := appPool.Exec(ctx, insert, w.ID().String(), bet.Transaction.ID().String(), 9000, 8000, 3)
	if pgCode(err) != codeUnique {
		t.Errorf("same transaction twice: error = %v, want unique violation", err)
	}
	// Dentro de uma transação, para que o erro venha do trigger de cadeia no
	// INSERT, e não da conferência diferida do commit.
	for name, args := range map[string][]any{
		"balance does not continue": {w.ID().String(), pending, 5000, 4000, 3},
		"version skips":             {w.ID().String(), pending, 9000, 8000, 5},
		"version repeats":           {w.ID().String(), pending, 9000, 8000, 2},
	} {
		tx, err := appPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, insert, args...)
		_ = tx.Rollback(ctx)
		if pgCode(err) != codeIntegrity || !strings.Contains(err.Error(), "breaks the chain") {
			t.Errorf("%s: error = %v, want chain rejection on insert", name, err)
		}
	}
	if n := len(ledgerOf(t, w.ID())); n != 2 {
		t.Fatalf("ledger has %d entries, want 2", n)
	}
	// O lançamento correto é aceito quando acompanha a carteira no mesmo commit.
	tx, err := appPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, insert, w.ID().String(), pending, 9000, 8000, 3); err != nil {
		t.Fatalf("valid chained entry rejected: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE wallets SET balance_minor = 8000, version = 3 WHERE id = $1`, w.ID().String()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("consistent wallet and entry rejected at commit: %v", err)
	}
	assertLedgerMatchesBalance(t, w.ID())
}

// mustProcessPending grava uma BET PENDING de 10.00 diretamente, sem
// processá-la, e devolve o id.
func mustProcessPending(t *testing.T, w interface {
	ID() domain.WalletID
	PlayerID() domain.PlayerID
}, extID string) string {
	t.Helper()
	var id string
	err := appPool.QueryRow(context.Background(), `
		INSERT INTO wager_transactions (id, kind, wallet_id, player_id, amount_minor, currency, status,
			provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
			created_at, updated_at)
		VALUES (gen_random_uuid(), 'BET', $1, $2, 1000, 'BRL', 'PENDING', 'provider-a', $3, $4,
			'v1:0000000000000000000000000000000000000000000000000000000000000000', 'round-1', 'game-1', now(), now())
		RETURNING id::text`, w.ID().String(), w.PlayerID().String(), extID, "provider-a:"+extID).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDatabaseEnforcesDomainRules(t *testing.T) {
	ctx := context.Background()
	out := openWallet(t, "100.00")
	w := out.Wallet.ID().String()
	p := out.Wallet.PlayerID().String()
	const hash = "v1:0000000000000000000000000000000000000000000000000000000000000000"

	insert := `
		INSERT INTO wager_transactions (id, kind, wallet_id, player_id, amount_minor, currency, status,
			provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
			reference_external_transaction_id, failure_code, result_balance_minor, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, 'BRL', $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, now(), now())`
	ext := func(id string) []any { return []any{"provider-a", id, "provider-a:" + id, hash, "round-1", "game-1"} }
	cases := map[string][]any{
		"second opening":           append([]any{"OPENING", w, p, 100, "PROCESSED"}, nil, nil, nil, nil, nil, nil, nil, nil, 100),
		"opening with provider":    append(append([]any{"OPENING", w, p, 100, "PROCESSED"}, ext("o-1")...), nil, nil, 100),
		"external without game":    append([]any{"BET", w, p, 100, "PENDING", "provider-a", "x-1", "k-1", hash, "round-1", nil}, nil, nil, nil),
		"loss with amount":         append(append([]any{"LOSS", w, p, 100, "PENDING"}, ext("l-1")...), nil, nil, nil),
		"bet with zero":            append(append([]any{"BET", w, p, 0, "PENDING"}, ext("b-0")...), nil, nil, nil),
		"bet with reference":       append(append([]any{"BET", w, p, 100, "PENDING"}, ext("b-1")...), "x", nil, nil),
		"refund without reference": append(append([]any{"REFUND", w, p, 100, "PENDING"}, ext("r-1")...), nil, nil, nil),
		"rejected unknown code":    append(append([]any{"BET", w, p, 100, "REJECTED"}, ext("b-2")...), nil, "SOMETHING", nil),
		"processed without result": append(append([]any{"BET", w, p, 100, "PROCESSED"}, ext("b-3")...), nil, nil, nil),
		"bet waiting reference":    append(append([]any{"BET", w, p, 100, "PENDING_REFERENCE"}, ext("b-4")...), nil, nil, nil),
		"failed with rejection":    append(append([]any{"BET", w, p, 100, "FAILED"}, ext("b-5")...), nil, "INSUFFICIENT_FUNDS", nil),
		"negative amount":          append(append([]any{"BET", w, p, -1, "PENDING"}, ext("b-6")...), nil, nil, nil),
	}
	for name, args := range cases {
		_, err := appPool.Exec(ctx, insert, args...)
		if code := pgCode(err); code != codeCheck && code != codeUnique {
			t.Errorf("%s: error = %v, want constraint violation", name, err)
		}
	}
}

func TestTransactionStateMachineInDatabase(t *testing.T) {
	ctx := context.Background()
	out := openWallet(t, "100.00")
	bet := mustProcess(t, operation(t, out.Wallet, "provider-a", "bet-sm", "BET", "10.00", ""))
	processedID := bet.Transaction.ID().String()

	refund := mustProcess(t, operation(t, out.Wallet, "provider-a", "refund-sm", "REFUND", "10.00", "missing-bet"))
	pendingRefID := refund.Transaction.ID().String()

	for name, c := range map[string]struct{ sql, id string }{
		"terminal changes status":   {`UPDATE wager_transactions SET status = 'REJECTED', failure_code = 'INSUFFICIENT_FUNDS' WHERE id = $1`, processedID},
		"terminal touches result":   {`UPDATE wager_transactions SET result_balance_minor = 1 WHERE id = $1`, processedID},
		"back to pending":           {`UPDATE wager_transactions SET status = 'PENDING' WHERE id = $1`, pendingRefID},
		"immutable amount":          {`UPDATE wager_transactions SET amount_minor = 1 WHERE id = $1`, pendingRefID},
		"immutable idempotency key": {`UPDATE wager_transactions SET idempotency_key = 'other' WHERE id = $1`, pendingRefID},
	} {
		if _, err := appPool.Exec(ctx, c.sql, c.id); pgCode(err) != codeIntegrity {
			t.Errorf("%s: error = %v, want trigger rejection", name, err)
		}
	}

	// Transição válida continua permitida: PENDING_REFERENCE -> REJECTED.
	_, err := appPool.Exec(ctx, `UPDATE wager_transactions SET status = 'REJECTED', failure_code = 'REFERENCE_NOT_FOUND' WHERE id = $1`, pendingRefID)
	if err != nil {
		t.Fatalf("valid transition rejected: %v", err)
	}
}

func TestOutboxSnapshotIsImmutable(t *testing.T) {
	ctx := context.Background()
	out := openWallet(t, "100.00")
	id := out.Events[0].ID().String()

	if _, err := appPool.Exec(ctx, `UPDATE outbox_events SET payload = '{}' WHERE event_id = $1`, id); pgCode(err) != codeIntegrity {
		t.Errorf("payload change: error = %v", err)
	}
	if _, err := appPool.Exec(ctx, `UPDATE outbox_events SET published_at = now(), attempts = 1 WHERE event_id = $1`, id); err != nil {
		t.Fatalf("publication bookkeeping rejected: %v", err)
	}
	if _, err := appPool.Exec(ctx, `UPDATE outbox_events SET published_at = NULL WHERE event_id = $1`, id); pgCode(err) != codeIntegrity {
		t.Errorf("unpublish: error = %v", err)
	}
	if _, err := ownerPool.Exec(ctx, `DELETE FROM outbox_events WHERE event_id = $1`, id); pgCode(err) != codeIntegrity {
		t.Errorf("delete: error = %v", err)
	}
}
