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

// TestKillDuringLoad: uma instância cai com SIGKILL no meio de uma rajada por
// HTTP e SQS e volta depois. Os clientes reenviam a outra instância o que
// falhou; as mensagens em andamento voltam para a fila. No fim, cada
// operação foi aplicada exatamente uma vez e nada ficou para trás.
func TestKillDuringLoad(t *testing.T) {
	c := newCluster(t, instances, nil)
	const wallets, httpOps, sqsOps = 10, 200, 200
	ws := make([]wallet, wallets)
	for i := range ws {
		ws[i] = c.openWallet("1000.00")
	}

	// Carga espaçada (~6s), para a queda acontecer no meio dela.
	const pace = 30 * time.Millisecond
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < httpOps; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				op := operation{W: ws[i%wallets], ExtID: fmt.Sprintf("http-%d", i), Kind: "BET", Amount: "1.00"}
				if r := c.submitUntilDone(i, op); r.Body["status"] != "PROCESSED" {
					t.Errorf("%s: %d %s", op.ExtID, r.Status, r.Raw)
				}
			}(i)
			time.Sleep(pace)
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < sqsOps; i++ {
			op := operation{W: ws[i%wallets], ExtID: fmt.Sprintf("sqs-%d", i), Kind: "BET", Amount: "1.00"}
			c.send("msg-"+op.ExtID, "msg-"+op.ExtID, op)
			time.Sleep(pace)
		}
	}()

	time.Sleep(2 * time.Second)
	c.nodes[0].kill(t)
	killedAt := c.count(`SELECT count(*) FROM wallet_ledger_entries WHERE direction = 'DEBIT'`)
	time.Sleep(2 * time.Second)
	c.nodes[0].start(t)
	wg.Wait()
	t.Logf("instance-0 killed after %d of %d debits", killedAt, httpOps+sqsOps)
	if killedAt >= (httpOps+sqsOps)*3/4 {
		t.Fatal("the kill happened too late in the load; the scenario proved little")
	}

	waitFor(t, 90*time.Second, "all messages consumed", func() bool {
		return c.count(`SELECT count(*) FROM inbox_messages WHERE completed_at IS NOT NULL`) == sqsOps &&
			queueDepth(t, c.queueURL) == 0
	})
	if n := queueDepth(t, c.dlqURL); n != 0 {
		t.Fatalf("%d messages in the DLQ", n)
	}
	if n := c.count(`SELECT count(*) FROM wallet_ledger_entries WHERE direction = 'DEBIT'`); n != httpOps+sqsOps {
		t.Fatalf("debits = %d, want %d (one per operation)", n, httpOps+sqsOps)
	}
	perWallet := (httpOps + sqsOps) / wallets
	for _, w := range ws {
		if b, want := c.balance(w), fmt.Sprintf("%d.00", 1000-perWallet); b != want {
			t.Errorf("wallet %s balance = %s, want %s", w.ID, b, want)
		}
	}
	c.assertConsistent()
}

// TestRestartAllInstances: as 3 instâncias caem juntas (SIGKILL) com
// operações concluídas, uma rejeição, reversões esperando a referência e
// eventos ainda não publicados. Três instâncias novas: replays devolvem o
// resultado original, as pendências são retomadas e a outbox é drenada.
func TestRestartAllInstances(t *testing.T) {
	// Primeira geração sem publisher nem worker de referências: os eventos e
	// as pendências ficam para a geração seguinte.
	c := newCluster(t, instances, map[string]string{"OUTBOX_PUBLISHER_ENABLED": "false", "REFERENCE_WORKER_ENABLED": "false"})
	const wallets = 5
	type sent struct {
		op    operation
		reply reply
	}
	var before []sent
	ws := make([]wallet, wallets)
	for i := range ws {
		ws[i] = c.openWallet("100.00")
		for j, op := range []operation{
			{W: ws[i], ExtID: fmt.Sprintf("bet-%d", i), Kind: "BET", Amount: "10.00"},
			{W: ws[i], ExtID: fmt.Sprintf("too-big-%d", i), Kind: "BET", Amount: "500.00"},
			{W: ws[i], ExtID: fmt.Sprintf("refund-%d", i), Kind: "REFUND", Amount: "5.00", Ref: fmt.Sprintf("late-%d", i)},
		} {
			before = append(before, sent{op, c.submitUntilDone(i+j, op)})
		}
	}
	unpublished := c.count(`SELECT count(*) FROM outbox_events WHERE published_at IS NULL`)
	if unpublished == 0 {
		t.Fatal("no unpublished events before the restart; the scenario proved nothing")
	}

	c.replaceAll("restarted", map[string]string{"OUTBOX_PUBLISHER_ENABLED": "true", "REFERENCE_WORKER_ENABLED": "true"})

	for i, s := range before {
		again := c.submitUntilDone(i+1, s.op) // outra instância, outro processo
		if again.Status != s.reply.Status || again.Body["transactionId"] != s.reply.Body["transactionId"] ||
			again.Body["status"] != s.reply.Body["status"] || fmt.Sprint(again.Body["balance"]) != fmt.Sprint(s.reply.Body["balance"]) {
			t.Errorf("%s after restart: %d %s; before: %d %s", s.op.ExtID, again.Status, again.Raw, s.reply.Status, s.reply.Raw)
		}
		if again.Body["idempotentReplay"] != true {
			t.Errorf("%s after restart was not a replay: %s", s.op.ExtID, again.Raw)
		}
	}
	for i, w := range ws {
		c.submitUntilDone(i, operation{W: w, ExtID: fmt.Sprintf("late-%d", i), Kind: "BET", Amount: "5.00"})
	}
	waitFor(t, 60*time.Second, "pending refunds resumed by the new instances", func() bool {
		return c.count(`SELECT count(*) FROM wager_transactions WHERE status = 'PENDING_REFERENCE'`) == 0
	})
	for _, w := range ws {
		if b := c.balance(w); b != "90.00" { // 100 - 10 - 5 + 5; a de 500.00 foi rejeitada
			t.Errorf("wallet %s balance = %s", w.ID, b)
		}
	}
	t.Logf("%d events were waiting in the outbox at the restart", unpublished)
	c.assertConsistent()
}

// TestKillConsumerAfterCommit: SIGKILL sob carga do consumidor até acertar a
// janela entre o commit e a remoção da mensagem. Uma latência de 100ms nas
// respostas do PostgreSQL alarga essa janela: o COMMIT já foi aplicado no
// banco enquanto o processo espera a confirmação, sem código de teste no
// binário. (Atrasar as requisições ao SQS não serve: o Toxiproxy entrega o
// DeleteMessage já recebido mesmo depois que o processo morre.) A evidência é
// a métrica de reentrega barrada pela inbox; a operação continua aplicada uma
// única vez.
func TestKillConsumerAfterCommit(t *testing.T) {
	c := newCluster(t, instances, nil)
	addResponseLatency(t, "postgres", 100)
	const wallets, perRound, maxRounds = 6, 60, 6
	ws := make([]wallet, wallets)
	for i := range ws {
		ws[i] = c.openWallet("1000.00")
	}
	total := 0
	for round := 0; round < maxRounds; round++ {
		for i := 0; i < perRound; i++ {
			op := operation{W: ws[i%wallets], ExtID: fmt.Sprintf("r%d-%d", round, i), Kind: "BET", Amount: "1.00"}
			c.send("msg-"+op.ExtID, "msg-"+op.ExtID, op)
		}
		total += perRound
		victim := c.nodes[round%instances]
		waitFor(t, 60*time.Second, "consumers working", func() bool {
			done := c.count(`SELECT count(*) FROM inbox_messages WHERE completed_at IS NOT NULL`)
			return done >= total-perRound+perRound/4
		})
		victim.kill(t)
		victim.start(t)
		waitFor(t, 90*time.Second, "round consumed", func() bool {
			return c.count(`SELECT count(*) FROM inbox_messages WHERE completed_at IS NOT NULL`) == total &&
				queueDepth(t, c.queueURL) == 0
		})
		if c.metric("wallet_duplicates_total", `reason="inbox_redelivery"`) > 0 {
			t.Logf("window hit in round %d: a committed message was redelivered and stopped by the inbox", round+1)
			break
		}
		if round == maxRounds-1 {
			t.Fatalf("no kill landed between commit and delete in %d rounds", maxRounds)
		}
	}
	if n := queueDepth(t, c.dlqURL); n != 0 {
		t.Fatalf("%d messages in the DLQ", n)
	}
	if n := c.count(`SELECT count(*) FROM wallet_ledger_entries WHERE direction = 'DEBIT'`); n != total {
		t.Fatalf("debits = %d, want %d", n, total)
	}
	c.assertConsistent()
}

// TestKillPublisherHoldingClaims: o único publisher cai com SIGKILL enquanto
// tem eventos reservados. Outro publisher assume depois do lease; cada
// evento chega uma única vez (mesmo eventId) e na ordem de cada carteira.
func TestKillPublisherHoldingClaims(t *testing.T) {
	c := newCluster(t, instances, map[string]string{"OUTBOX_PUBLISHER_ENABLED": "false"})
	const wallets, bets = 20, 10
	ws := make([]wallet, wallets)
	for i := range ws {
		ws[i] = c.openWallet("100.00")
	}
	var wg sync.WaitGroup
	for i, w := range ws {
		wg.Add(1)
		go func(i int, w wallet) {
			defer wg.Done()
			for j := 0; j < bets; j++ {
				c.submitUntilDone(i+j, operation{W: w, ExtID: fmt.Sprintf("p-%d-%d", i, j), Kind: "BET", Amount: "1.00"})
			}
		}(i, w)
	}
	wg.Wait()

	publisher := c.addInstance("publisher-a", map[string]string{"OUTBOX_PUBLISHER_ENABLED": "true"})
	waitFor(t, 30*time.Second, "publisher-a holding claims", func() bool {
		return c.count(`SELECT count(*) FROM outbox_events WHERE claimed_by IS NOT NULL AND published_at IS NULL`) > 0
	})
	publisher.kill(t)
	stranded := c.count(`SELECT count(*) FROM outbox_events WHERE claimed_by IS NOT NULL AND claimed_until > now() AND published_at IS NULL`)
	if stranded == 0 {
		t.Fatal("publisher-a died without holding claims; the scenario proved nothing")
	}
	t.Logf("publisher-a killed holding %d claimed events", stranded)

	c.addInstance("publisher-b", map[string]string{"OUTBOX_PUBLISHER_ENABLED": "true"})
	c.assertConsistent()
	for walletID, versions := range c.versions {
		if !sort.IntsAreSorted(versions) {
			t.Errorf("wallet %s events out of order: %v", walletID, versions)
		}
	}
	if r, err := c.do(0, http.MethodGet, "/health/ready", "", nil); err != nil || r.Status != http.StatusOK {
		t.Errorf("readiness after recovery: %v %v", r.Status, err)
	}
}

// TestGracefulTermination: SIGTERM em uma instância sob carga. O processo
// conclui ou devolve o trabalho em andamento e sai com código 0 dentro do
// SHUTDOWN_TIMEOUT; as outras instâncias seguem e nada se perde.
func TestGracefulTermination(t *testing.T) {
	c := newCluster(t, instances, nil)
	const wallets, httpOps, sqsOps = 10, 100, 100
	ws := make([]wallet, wallets)
	for i := range ws {
		ws[i] = c.openWallet("1000.00")
	}
	load := pacedLoad(t, c, ws, "term", httpOps, sqsOps)
	time.Sleep(1500 * time.Millisecond)
	started := time.Now()
	c.nodes[0].terminate(t)
	t.Logf("instance-0 exited with code 0 in %v", time.Since(started))
	load.Wait()
	c.assertSettled(ws, 1000, httpOps, sqsOps, 0)
}
