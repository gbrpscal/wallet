//go:build resilience

package resilience_test

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"
)

// assertLockedNotRetried: com o lock da carteira (D1), processos diferentes
// esperam a vez em vez de colidir; nenhuma escrita chega com versão
// desatualizada. A checagem de versão continua como segunda defesa.
func (c *cluster) assertLockedNotRetried() {
	c.t.Helper()
	if n := c.metric("wallet_concurrency_conflicts_total", `kind="stale_version"`); n != 0 {
		c.t.Errorf("stale-version conflicts across instances = %v; the wallet lock is not serializing writers", n)
	}
}

// TestFiftyIdenticalBetsAcrossInstances: a mesma aposta 50 vezes em paralelo,
// distribuída entre os 3 processos, gera um único débito.
func TestFiftyIdenticalBetsAcrossInstances(t *testing.T) {
	c := newCluster(t, instances, nil)
	w := c.openWallet("1000.00")
	bet := operation{W: w, ExtID: "same-bet", Kind: "BET", Amount: "25.00"}

	const n = 50
	replies := make([]reply, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			replies[i] = c.submitUntilDone(i%instances, bet)
		}(i)
	}
	close(start)
	wg.Wait()

	ids := map[any]bool{}
	replays := 0
	for _, r := range replies {
		if r.Status != http.StatusOK {
			t.Fatalf("reply %d %s", r.Status, r.Raw)
		}
		ids[r.Body["transactionId"]] = true
		if r.Body["idempotentReplay"] == true {
			replays++
		}
	}
	if len(ids) != 1 || replays != n-1 {
		t.Fatalf("distinct transactions %d, replays %d; want 1 and %d", len(ids), replays, n-1)
	}
	if b := c.balance(w); b != "975.00" {
		t.Fatalf("balance = %s", b)
	}
	if d := c.count(`SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.ID); d != 1 {
		t.Fatalf("debits = %d", d)
	}
	if got := c.metric("wallet_duplicates_total", `reason="idempotent_replay"`); got != n-1 {
		t.Errorf("duplicates across instances = %v, want %d", got, n-1)
	}
	c.assertLockedNotRetried()
	c.assertConsistent()
}

// TestTwoBetsOf80AcrossInstances: carteira com 100.00 recebe ao mesmo tempo
// duas apostas de 80.00 em processos diferentes: uma processada, outra
// rejeitada por saldo; reenvios a outras instâncias não mudam o resultado.
func TestTwoBetsOf80AcrossInstances(t *testing.T) {
	c := newCluster(t, instances, nil)
	w := c.openWallet("100.00")
	bets := []operation{
		{W: w, ExtID: "b80-a", Kind: "BET", Amount: "80.00"},
		{W: w, ExtID: "b80-b", Kind: "BET", Amount: "80.00"},
	}
	outcome := map[string]string{}
	for round := 0; round < 3; round++ {
		var mu sync.Mutex
		var wg sync.WaitGroup
		statuses := map[string]int{}
		for i, b := range bets {
			wg.Add(1)
			go func(i int, b operation) {
				defer wg.Done()
				r := c.submitUntilDone(i+round, b) // instâncias diferentes a cada rodada
				status, _ := r.Body["status"].(string)
				mu.Lock()
				defer mu.Unlock()
				statuses[status]++
				if round == 0 {
					outcome[b.ExtID] = status
				} else if outcome[b.ExtID] != status {
					t.Errorf("round %d: %s changed from %s to %s", round, b.ExtID, outcome[b.ExtID], status)
				}
				if status == "REJECTED" && r.Body["failureCode"] != "INSUFFICIENT_FUNDS" {
					t.Errorf("rejection code = %v", r.Body["failureCode"])
				}
			}(i, b)
		}
		wg.Wait()
		if statuses["PROCESSED"] != 1 || statuses["REJECTED"] != 1 {
			t.Fatalf("round %d statuses = %v", round, statuses)
		}
	}
	if b := c.balance(w); b != "20.00" {
		t.Fatalf("balance = %s", b)
	}
	if d := c.count(`SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.ID); d != 1 {
		t.Fatalf("debits = %d", d)
	}
	c.assertLockedNotRetried()
	c.assertConsistent()
}

// TestBurstOnOneWalletAcrossInstances: 60 apostas distintas na mesma
// carteira, disparadas juntas pelos 3 processos. Todas são aplicadas, o saldo
// fecha e nenhuma escrita colide: o lock da carteira serializa os processos
// (D1). Sem ele, a colisão apareceria como conflito de versão na carteira ou
// de unicidade na cadeia do ledger (mesma versão gravada duas vezes); como as
// chaves são todas distintas, nenhum conflito de qualquer tipo é esperado.
func TestBurstOnOneWalletAcrossInstances(t *testing.T) {
	c := newCluster(t, instances, nil)
	w := c.openWallet("100.00")
	const n = 60
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r := c.submitUntilDone(i, operation{W: w, ExtID: fmt.Sprintf("burst-%d", i), Kind: "BET", Amount: "1.00"})
			if r.Body["status"] != "PROCESSED" {
				t.Errorf("burst-%d: %d %s", i, r.Status, r.Raw)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if b := c.balance(w); b != "40.00" {
		t.Fatalf("balance = %s", b)
	}
	if d := c.count(`SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.ID); d != n {
		t.Fatalf("debits = %d, want %d", d, n)
	}
	if conflicts := c.metric("wallet_concurrency_conflicts_total"); conflicts != 0 {
		t.Errorf("concurrency conflicts across instances = %v; the wallet lock is not serializing writers", conflicts)
	}
	c.assertConsistent()
}

// TestIndependentWalletsAcrossInstances: muitas carteiras recebendo apostas
// ao mesmo tempo pelos 3 processos; todas terminam com o saldo esperado.
func TestIndependentWalletsAcrossInstances(t *testing.T) {
	c := newCluster(t, instances, nil)
	const wallets, bets = 15, 10
	ws := make([]wallet, wallets)
	for i := range ws {
		ws[i] = c.openWallet("100.00")
	}
	started := time.Now()
	var wg sync.WaitGroup
	for i, w := range ws {
		for j := 0; j < bets; j++ {
			wg.Add(1)
			go func(i, j int, w wallet) {
				defer wg.Done()
				r := c.submitUntilDone(i+j, operation{W: w, ExtID: fmt.Sprintf("ind-%d-%d", i, j), Kind: "BET", Amount: "1.00"})
				if r.Body["status"] != "PROCESSED" {
					t.Errorf("bet %d-%d: %s", i, j, r.Raw)
				}
			}(i, j, w)
		}
	}
	wg.Wait()
	t.Logf("%d operations on %d wallets through %d instances in %v", wallets*bets, wallets, instances, time.Since(started))
	for _, w := range ws {
		if b := c.balance(w); b != "90.00" {
			t.Errorf("wallet %s balance = %s", w.ID, b)
		}
	}
	c.assertConsistent()
}

// TestHTTPAndSQSAcrossInstances: a mesma operação chega por HTTP a uma
// instância e por SQS (duas mensagens diferentes e uma cópia repetida) às
// outras; cada operação gera um único lançamento.
func TestHTTPAndSQSAcrossInstances(t *testing.T) {
	c := newCluster(t, instances, nil)
	const wallets = 10
	ws := make([]wallet, wallets)
	for i := range ws {
		ws[i] = c.openWallet("100.00")
	}
	var wg sync.WaitGroup
	for i, w := range ws {
		op := operation{W: w, ExtID: fmt.Sprintf("cross-%d", i), Kind: "BET", Amount: "10.00"}
		// SQS: duas mensagens com a mesma chave e uma cópia da primeira com
		// outro id de deduplicação do SQS (recebimento repetido de verdade).
		c.send("msg-a-"+op.ExtID, "dedup-a-"+op.ExtID, op)
		c.send("msg-b-"+op.ExtID, "dedup-b-"+op.ExtID, op)
		c.send("msg-a-"+op.ExtID, "dedup-a2-"+op.ExtID, op)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := c.submitUntilDone(i, op)
			if r.Body["status"] != "PROCESSED" {
				t.Errorf("http %s: %s", op.ExtID, r.Raw)
			}
		}(i)
	}
	wg.Wait()

	waitFor(t, 60*time.Second, "all messages consumed", func() bool {
		return c.count(`SELECT count(*) FROM inbox_messages WHERE completed_at IS NOT NULL`) == 2*wallets &&
			queueDepth(t, c.queueURL) == 0
	})
	if n := queueDepth(t, c.dlqURL); n != 0 {
		t.Fatalf("%d messages in the DLQ", n)
	}
	for _, w := range ws {
		if b := c.balance(w); b != "90.00" {
			t.Errorf("wallet %s balance = %s", w.ID, b)
		}
		if d := c.count(`SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.ID); d != 1 {
			t.Errorf("wallet %s debits = %d", w.ID, d)
		}
	}
	// Cada operação: 1 processamento e 3 duplicatas (HTTP ou SQS), somadas
	// entre as instâncias; a cópia repetida é barrada pela inbox.
	dups := c.metric("wallet_duplicates_total")
	if dups != 3*wallets {
		t.Errorf("duplicates across instances = %v, want %d", dups, 3*wallets)
	}
	if got := c.metric("wallet_duplicates_total", `reason="inbox_redelivery"`); got != wallets {
		t.Errorf("inbox redeliveries = %v, want %d", got, wallets)
	}
	c.assertConsistent()
}

// TestWorkersAcrossInstances: os 3 publishers disputam a mesma outbox e os 3
// workers de referência, as mesmas pendências. Cada evento chega uma única
// vez e na ordem de cada carteira; cada reversão é concluída uma única vez.
func TestWorkersAcrossInstances(t *testing.T) {
	c := newCluster(t, instances, nil)
	const wallets = 8
	ws := make([]wallet, wallets)
	for i := range ws {
		ws[i] = c.openWallet("100.00")
	}
	// Reversões antes das apostas: ficam pendentes e são resolvidas pelos
	// workers de qualquer instância quando a aposta chega.
	for i, w := range ws {
		r := c.submitUntilDone(i, operation{W: w, ExtID: fmt.Sprintf("refund-%d", i), Kind: "REFUND", Amount: "5.00", Ref: fmt.Sprintf("bet-%d", i)})
		if r.Status != http.StatusAccepted {
			t.Fatalf("refund before bet: %d %s", r.Status, r.Raw)
		}
	}
	var wg sync.WaitGroup
	for i, w := range ws {
		for j := 0; j < 5; j++ {
			wg.Add(1)
			go func(i, j int, w wallet) {
				defer wg.Done()
				ext := fmt.Sprintf("extra-%d-%d", i, j)
				if j == 0 {
					ext = fmt.Sprintf("bet-%d", i)
				}
				c.submitUntilDone(i+j, operation{W: w, ExtID: ext, Kind: "BET", Amount: "5.00"})
			}(i, j, w)
		}
	}
	wg.Wait()

	waitFor(t, 60*time.Second, "pending references resolved", func() bool {
		return c.count(`SELECT count(*) FROM wager_transactions WHERE status = 'PENDING_REFERENCE'`) == 0
	})
	if n := c.count(`SELECT count(*) FROM wager_transactions WHERE kind = 'REFUND' AND status = 'PROCESSED'`); n != wallets {
		t.Fatalf("processed refunds = %d, want %d", n, wallets)
	}
	for _, w := range ws {
		if b := c.balance(w); b != "80.00" { // 100 - 5×5 + 5
			t.Errorf("wallet %s balance = %s", w.ID, b)
		}
	}
	c.assertConsistent()
	for walletID, versions := range c.versions {
		if !sort.IntsAreSorted(versions) {
			t.Errorf("wallet %s events out of order: %v", walletID, versions)
		}
	}
}
