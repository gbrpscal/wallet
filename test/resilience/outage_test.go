//go:build resilience

package resilience_test

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pacedLoad envia, ao longo de alguns segundos, operações por HTTP (com
// reenvio do cliente) e mensagens SQS, distribuídas entre as carteiras.
func pacedLoad(t *testing.T, c *cluster, ws []wallet, prefix string, httpOps, sqsOps int) *sync.WaitGroup {
	const pace = 30 * time.Millisecond
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < httpOps; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				op := operation{W: ws[i%len(ws)], ExtID: fmt.Sprintf("%s-http-%d", prefix, i), Kind: "BET", Amount: "1.00"}
				if r := c.submitUntilDone(i, op); r.Body["status"] != "PROCESSED" {
					t.Errorf("%s: %d %s", op.ExtID, r.Status, r.Raw)
				}
			}(i)
			time.Sleep(pace)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < sqsOps; i++ {
			op := operation{W: ws[i%len(ws)], ExtID: fmt.Sprintf("%s-sqs-%d", prefix, i), Kind: "BET", Amount: "1.00"}
			c.send("msg-"+op.ExtID, "msg-"+op.ExtID, op)
			time.Sleep(pace)
		}
	}()
	return &wg
}

// readyStatus devolve o status do /health/ready de cada instância.
func (c *cluster) readyStatuses() []int {
	var out []int
	for i := range c.nodes {
		r, err := c.do(i, http.MethodGet, "/health/ready", "", nil)
		if err != nil {
			out = append(out, 0)
			continue
		}
		out = append(out, r.Status)
	}
	return out
}

func allEqual(values []int, want int) bool {
	for _, v := range values {
		if v != want {
			return false
		}
	}
	return true
}

// assertSettled: todas as mensagens consumidas, DLQ vazia e um débito por
// operação (extra conta débitos fora da carga); saldos coerentes com a
// quantidade de operações por carteira.
func (c *cluster) assertSettled(ws []wallet, initial, httpOps, sqsOps, extra int) {
	c.t.Helper()
	waitFor(c.t, 120*time.Second, "all messages consumed", func() bool {
		if n := queueDepth(c.t, c.dlqURL); n != 0 {
			c.t.Fatalf("%d messages in the DLQ", n)
		}
		return c.count(`SELECT count(*) FROM inbox_messages WHERE completed_at IS NOT NULL`) == sqsOps &&
			queueDepth(c.t, c.queueURL) == 0
	})
	if n := c.count(`SELECT count(*) FROM wallet_ledger_entries WHERE direction = 'DEBIT'`); n != httpOps+sqsOps+extra {
		c.t.Fatalf("debits = %d, want %d (one per operation)", n, httpOps+sqsOps+extra)
	}
	perWallet := (httpOps + sqsOps) / len(ws)
	for _, w := range ws {
		if b, want := c.balance(w), fmt.Sprintf("%d.00", initial-perWallet); b != want {
			c.t.Errorf("wallet %s balance = %s, want %s", w.ID, b, want)
		}
	}
	c.assertConsistent()
}

// TestDatabaseOutage: o PostgreSQL fica inalcançável por 5s no meio da carga.
// As requisições falham com 503 sem efeito parcial, a readiness acusa a
// falha e as mensagens voltam para a fila; depois, tudo é aplicado uma vez.
func TestDatabaseOutage(t *testing.T) {
	c := newCluster(t, instances, nil)
	const wallets, httpOps, sqsOps = 10, 150, 150
	ws := make([]wallet, wallets)
	for i := range ws {
		ws[i] = c.openWallet("1000.00")
	}
	probeWallet := c.openWallet("100.00") // fora da carga, para não mudar os saldos esperados
	load := pacedLoad(t, c, ws, "db", httpOps, sqsOps)

	time.Sleep(1500 * time.Millisecond)
	setProxy(t, "postgres", false)
	outageStart := time.Now()
	waitFor(t, 15*time.Second, "readiness failing on every instance", func() bool {
		return allEqual(c.readyStatuses(), http.StatusServiceUnavailable)
	})
	probe := operation{W: probeWallet, ExtID: "db-probe", Kind: "BET", Amount: "1.00"}
	r, err := c.do(0, http.MethodPost, "/wagering/transactions", "provider-a", probe.body(), "Idempotency-Key", probe.key())
	if err != nil || r.Status != http.StatusServiceUnavailable || r.Body["code"] != "UNAVAILABLE" {
		t.Errorf("request during outage = %v %s, want 503 UNAVAILABLE", err, r.Raw)
	}
	time.Sleep(5*time.Second - time.Since(outageStart))
	setProxy(t, "postgres", true)
	waitFor(t, 30*time.Second, "readiness recovered", func() bool {
		return allEqual(c.readyStatuses(), http.StatusOK)
	})

	load.Wait()
	// A sonda recusada durante a queda não deixou nada e pode ser reenviada.
	if n := c.count(`SELECT count(*) FROM wager_transactions WHERE external_transaction_id = 'db-probe'`); n != 0 {
		t.Fatalf("request refused during the outage left %d rows", n)
	}
	if again := c.submitUntilDone(1, probe); again.Body["status"] != "PROCESSED" || again.Body["idempotentReplay"] == true {
		t.Fatalf("probe after recovery = %s", again.Raw)
	}
	if b := c.balance(probeWallet); b != "99.00" {
		t.Errorf("probe wallet balance = %s, want 99.00", b)
	}
	c.assertSettled(ws, 1000, httpOps, sqsOps, 1)
	t.Logf("transient retries across instances: %v", c.metric("wallet_retries_total"))
}

// TestBrokerOutage: o LocalStack (SQS e SNS) fica inalcançável por 6s. A API
// continua operando, a readiness acusa a falha, o consumidor recua e os
// eventos esperam na outbox; ao voltar, a fila e a outbox são drenadas.
func TestBrokerOutage(t *testing.T) {
	c := newCluster(t, instances, nil)
	const wallets, httpOps, sqsOps = 10, 120, 120
	ws := make([]wallet, wallets)
	for i := range ws {
		ws[i] = c.openWallet("1000.00")
	}
	load := pacedLoad(t, c, ws, "mq", httpOps, sqsOps)

	time.Sleep(1500 * time.Millisecond)
	setProxy(t, "sqs", false)
	var processedDuringOutage atomic.Int64
	before := c.count(`SELECT count(*) FROM wager_transactions WHERE external_transaction_id LIKE 'mq-http-%'`)
	waitFor(t, 15*time.Second, "readiness failing on every instance", func() bool {
		return allEqual(c.readyStatuses(), http.StatusServiceUnavailable)
	})
	waitFor(t, 15*time.Second, "publisher failing and events waiting in the outbox", func() bool {
		return c.count(`SELECT count(*) FROM outbox_events WHERE published_at IS NULL AND attempts > 0`) > 0
	})
	time.Sleep(3 * time.Second)
	processedDuringOutage.Store(int64(c.count(`SELECT count(*) FROM wager_transactions WHERE external_transaction_id LIKE 'mq-http-%'`) - before))
	setProxy(t, "sqs", true)
	waitFor(t, 30*time.Second, "readiness recovered", func() bool {
		return allEqual(c.readyStatuses(), http.StatusOK)
	})
	if processedDuringOutage.Load() == 0 {
		t.Error("the API stopped processing while only the broker was down")
	}
	t.Logf("HTTP operations processed while the broker was down: %d", processedDuringOutage.Load())

	load.Wait()
	c.assertSettled(ws, 1000, httpOps, sqsOps, 0)
}
