//go:build resilience

// Package resilience testa o binário real de fora (caixa-preta): três
// processos independentes, cada um com suas conexões e memória, sobre um banco
// e filas exclusivos de cada teste. PostgreSQL e LocalStack são alcançados
// pelo Toxiproxy, que simula indisponibilidade; quedas abruptas usam SIGKILL.
package resilience_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssns "github.com/aws/aws-sdk-go-v2/service/sns"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/gbrpscal/wallet/internal/adapters/sns"
	wsqs "github.com/gbrpscal/wallet/internal/adapters/sqs"
	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/platform/config"
	"github.com/gbrpscal/wallet/internal/testutil/pgtest"
)

// instances é o número de processos por cenário (mínimo exigido: 3).
const instances = 3

var (
	binary    string
	sqsClient *awssqs.Client
	snsClient *awssns.Client
)

// TestMain compila o binário uma vez, com -race: uma condição de corrida em
// qualquer processo encerra o processo e aparece no seu log.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wallet-resilience-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "wallet")
	build := exec.Command("go", "build", "-race", "-o", binary, "github.com/gbrpscal/wallet/cmd/wallet")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}
	base := config.Config{AWSRegion: os.Getenv("AWS_REGION"), AWSEndpointURL: os.Getenv("AWS_ENDPOINT_URL")}
	if sqsClient, err = wsqs.NewClient(base); err == nil {
		snsClient, err = sns.NewClient(base)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// testEnv: tempos encurtados para os cenários rodarem em segundos; o código
// é o mesmo da produção, só a configuração muda.
var testEnv = map[string]string{
	"SQS_WAIT_TIME":             "1s",
	"SQS_VISIBILITY_TIMEOUT":    "5s",
	"SQS_RETRY_BACKOFF_BASE":    "1s",
	"SQS_RETRY_BACKOFF_MAX":     "4s",
	"OUTBOX_LEASE":              "3s",
	"OUTBOX_IDLE_INTERVAL":      "200ms",
	"OUTBOX_RETRY_BASE":         "500ms",
	"OUTBOX_RETRY_MAX":          "2s",
	"REFERENCE_WORKER_INTERVAL": "200ms",
	"REFERENCE_BACKOFF_BASE":    "200ms",
	"REFERENCE_BACKOFF_MAX":     "1s",
	"REFERENCE_MAX_ATTEMPTS":    "60",
	"SHUTDOWN_TIMEOUT":          "5s",
	"METRICS_REFRESH_INTERVAL":  "1s",
	"DB_MAX_CONNS":              "10",
	"LOG_LEVEL":                 "info",
}

// cluster é o ambiente de um teste: banco e filas exclusivos e as instâncias.
type cluster struct {
	t         *testing.T
	db        *pgtest.DB
	queueURL  string
	dlqURL    string
	auditURL  string
	env       map[string]string
	nodes     []*instance
	retired   []*instance
	tokenMu   sync.Mutex
	tokens    map[string]string
	httpc     *http.Client
	seenAudit map[string]int
	// versions guarda, na ordem de chegada, as walletVersion dos
	// WalletBalanceChanged de cada carteira.
	versions map[string][]int
}

// newCluster prepara os recursos e sobe n instâncias. extra sobrescreve
// variáveis de todas as instâncias.
func newCluster(t *testing.T, n int, extra map[string]string) *cluster {
	t.Helper()
	ctx := context.Background()
	toxiproxyReset(t)
	db, err := pgtest.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)

	c := &cluster{t: t, db: db, tokens: map[string]string{}, seenAudit: map[string]int{}, versions: map[string][]int{},
		httpc: &http.Client{Timeout: 15 * time.Second}}
	s := suffix()
	queue, dlq := "res-"+s+".fifo", "res-"+s+"-dlq.fifo"
	c.dlqURL = createQueue(t, dlq, nil)
	redrive := fmt.Sprintf(`{"deadLetterTargetArn":"%s","maxReceiveCount":"5"}`, queueArn(t, c.dlqURL))
	c.queueURL = createQueue(t, queue, map[string]string{"VisibilityTimeout": "5", "RedrivePolicy": redrive})
	topic, err := snsClient.CreateTopic(ctx, &awssns.CreateTopicInput{Name: aws.String("res-" + s + ".fifo"),
		Attributes: map[string]string{"FifoTopic": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = snsClient.DeleteTopic(context.Background(), &awssns.DeleteTopicInput{TopicArn: topic.TopicArn})
	})
	c.auditURL = createQueue(t, "res-"+s+"-audit.fifo", nil)
	if _, err := snsClient.Subscribe(ctx, &awssns.SubscribeInput{TopicArn: topic.TopicArn, Protocol: aws.String("sqs"),
		Endpoint: aws.String(queueArn(t, c.auditURL)), Attributes: map[string]string{"RawMessageDelivery": "true"}}); err != nil {
		t.Fatal(err)
	}

	c.env = map[string]string{
		// Banco e AWS pelo Toxiproxy.
		"DATABASE_URL":         viaToxiproxy(t, db.AppURL, "toxiproxy:15432"),
		"AWS_ENDPOINT_URL":     "http://toxiproxy:14566",
		"SQS_WAGER_QUEUE_NAME": queue,
		"SQS_WAGER_DLQ_NAME":   dlq,
		"SQS_EVENTS_DLQ_NAME":  dlq,
		"SNS_EVENTS_TOPIC_ARN": aws.ToString(topic.TopicArn),
	}
	for k, v := range testEnv {
		c.env[k] = v
	}
	for k, v := range extra {
		c.env[k] = v
	}
	for i := 0; i < n; i++ {
		c.nodes = append(c.nodes, c.newInstance(fmt.Sprintf("instance-%d", i), nil))
	}
	t.Cleanup(c.shutdown)
	for _, node := range c.nodes {
		node.start(t)
	}
	return c
}

// shutdown encerra o que ainda roda, procura panic e data race nos logs e,
// se o teste falhou, imprime o fim do log de cada instância.
func (c *cluster) shutdown() {
	for _, n := range c.nodes {
		n.kill(c.t)
	}
	for _, n := range append(c.retired, c.nodes...) {
		raw, _ := os.ReadFile(n.logPath)
		for _, bad := range []string{"WARNING: DATA RACE", "panic:", "\"level\":\"ERROR\",\"msg\":\"panic"} {
			if bytes.Contains(raw, []byte(bad)) {
				c.t.Errorf("%s log contains %q", n.name, bad)
			}
		}
		if c.t.Failed() {
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if len(lines) > 30 {
				lines = lines[len(lines)-30:]
			}
			c.t.Logf("--- last log lines of %s ---\n%s", n.name, strings.Join(lines, "\n"))
		}
	}
}

// instance é um processo independente da aplicação.
type instance struct {
	name     string
	httpAddr string
	metrics  string
	logPath  string
	env      []string
	mu       sync.Mutex
	cmd      *exec.Cmd
	done     chan error
}

// addInstance sobe mais uma instância; overrides vale só para ela.
func (c *cluster) addInstance(name string, overrides map[string]string) *instance {
	c.t.Helper()
	n := c.newInstance(name, overrides)
	c.nodes = append(c.nodes, n)
	n.start(c.t)
	return n
}

// replaceAll derruba todas as instâncias com SIGKILL e sobe outras tantas,
// novas, com overrides aplicado sobre a configuração do cluster.
func (c *cluster) replaceAll(prefix string, overrides map[string]string) {
	c.t.Helper()
	for _, n := range c.nodes {
		n.kill(c.t)
	}
	c.retired = append(c.retired, c.nodes...)
	fresh := make([]*instance, len(c.nodes))
	for i := range fresh {
		fresh[i] = c.newInstance(fmt.Sprintf("%s-%d", prefix, i), overrides)
	}
	c.nodes = fresh
	for _, n := range c.nodes {
		n.start(c.t)
	}
}

func (c *cluster) newInstance(name string, overrides map[string]string) *instance {
	n := &instance{name: name, httpAddr: freeAddr(c.t), metrics: freeAddr(c.t),
		logPath: filepath.Join(c.t.TempDir(), name+".log")}
	env := os.Environ()
	for k, v := range c.env {
		env = append(env, k+"="+v)
	}
	for k, v := range overrides {
		env = append(env, k+"="+v) // o exec usa o último valor de cada chave
	}
	n.env = append(env, "HTTP_ADDR="+n.httpAddr, "METRICS_ADDR="+n.metrics)
	return n
}

func (n *instance) url() string { return "http://" + n.httpAddr }

// start sobe o processo e espera o /health/live. Reiniciar uma instância
// reaproveita portas e acrescenta ao mesmo log.
func (n *instance) start(t *testing.T) {
	t.Helper()
	logFile, err := os.OpenFile(n.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary)
	cmd.Env = n.env
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); logFile.Close() }()
	n.mu.Lock()
	n.cmd, n.done = cmd, done
	n.mu.Unlock()
	waitFor(t, 60*time.Second, n.name+" live", func() bool {
		select {
		case err := <-done:
			done <- err
			t.Fatalf("%s exited during startup: %v", n.name, err)
		default:
		}
		res, err := http.Get(n.url() + "/health/live")
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == http.StatusOK
	})
}

func (n *instance) running() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cmd == nil {
		return false
	}
	select {
	case err := <-n.done:
		n.done <- err
		return false
	default:
		return true
	}
}

// kill derruba o processo com SIGKILL: sem shutdown, sem liberar nada.
func (n *instance) kill(t *testing.T) {
	n.mu.Lock()
	cmd, done := n.cmd, n.done
	n.cmd = nil
	n.mu.Unlock()
	if cmd == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGKILL)
	<-done
}

// terminate envia SIGTERM e exige saída com código 0 dentro do prazo.
func (n *instance) terminate(t *testing.T) {
	t.Helper()
	n.mu.Lock()
	cmd, done := n.cmd, n.done
	n.cmd = nil
	n.mu.Unlock()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s exited with %v after SIGTERM", n.name, err)
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("%s did not exit after SIGTERM", n.name)
	}
}

// metric soma, em todas as instâncias vivas, as séries de name cujos labels
// contêm todos os trechos de labels (ex.: `reason="inbox_redelivery"`).
func (c *cluster) metric(name string, labels ...string) float64 {
	var total float64
	for _, n := range c.nodes {
		if !n.running() {
			continue
		}
		res, err := http.Get("http://" + n.metrics + "/metrics")
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	lines:
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, name+"{") && !strings.HasPrefix(line, name+" ") {
				continue
			}
			for _, l := range labels {
				if !strings.Contains(line, l) {
					continue lines
				}
			}
			if v, err := strconv.ParseFloat(line[strings.LastIndex(line, " ")+1:], 64); err == nil {
				total += v
			}
		}
		res.Body.Close()
	}
	return total
}

// --- Cliente HTTP ---------------------------------------------------------

type reply struct {
	Status int
	Body   map[string]any
	Raw    string
}

func (c *cluster) token(client string) string {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if tok, ok := c.tokens[client]; ok {
		return tok
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	req, _ := http.NewRequest(http.MethodPost, os.Getenv("OIDC_TOKEN_URL"), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(client, client+"-secret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("token: %v", err)
	}
	defer res.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil || body.AccessToken == "" {
		c.t.Fatalf("token for %s: status %d, %v", client, res.StatusCode, err)
	}
	c.tokens[client] = body.AccessToken
	return body.AccessToken
}

// do faz uma requisição a uma instância; erro de rede volta como erro.
func (c *cluster) do(node int, method, path, client string, body any, headers ...string) (reply, error) {
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, c.nodes[node].url()+path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if client != "" {
		req.Header.Set("Authorization", "Bearer "+c.token(client))
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := c.httpc.Do(req)
	if err != nil {
		return reply{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	r := reply{Status: res.StatusCode, Raw: string(raw)}
	_ = json.Unmarshal(raw, &r.Body)
	return r, nil
}

// submitUntilDone envia a operação começando pela instância node e, como um
// cliente real, reenvia a mesma chave a outra instância enquanto houver erro
// de rede ou 5xx. Devolve a primeira resposta definitiva.
func (c *cluster) submitUntilDone(node int, op operation) reply {
	deadline := time.Now().Add(60 * time.Second)
	for attempt := 0; ; attempt++ {
		target := (node + attempt) % len(c.nodes)
		if c.nodes[target].running() {
			r, err := c.do(target, http.MethodPost, "/wagering/transactions", "provider-a", op.body(), "Idempotency-Key", op.key())
			if err == nil && r.Status < 500 {
				return r
			}
		}
		if time.Now().After(deadline) {
			c.t.Errorf("operation %s never completed", op.ExtID)
			return reply{}
		}
		time.Sleep(200 * time.Millisecond)
	}
}

type wallet struct{ ID, Player string }

func (c *cluster) openWallet(amount string) wallet {
	c.t.Helper()
	player := newID(c.t)
	for attempt := 0; ; attempt++ {
		r, err := c.do(attempt%len(c.nodes), http.MethodPost, "/wallets", "wallet-backoffice",
			map[string]any{"playerId": player, "initialBalance": map[string]string{"amount": amount, "currency": "BRL"}})
		if err == nil && r.Status == http.StatusCreated {
			return wallet{ID: r.Body["id"].(string), Player: player}
		}
		if err == nil && r.Status == http.StatusConflict {
			// A abertura anterior foi confirmada, mas a resposta se perdeu.
			var id string
			if err := c.db.App.QueryRow(context.Background(), `SELECT id::text FROM wallets WHERE player_id = $1`, player).Scan(&id); err == nil {
				return wallet{ID: id, Player: player}
			}
		}
		if attempt > 100 {
			c.t.Fatalf("open wallet: %v %v", err, r.Raw)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// operation é uma operação de provedor, enviada por HTTP ou SQS.
type operation struct {
	W      wallet
	ExtID  string
	Kind   string
	Amount string
	Ref    string
}

func (o operation) key() string { return "provider-a:" + o.ExtID }

func (o operation) body() map[string]any {
	b := map[string]any{
		"providerId": "provider-a", "externalTransactionId": o.ExtID, "playerId": o.W.Player, "walletId": o.W.ID,
		"roundId": "round-1", "gameId": "game-1", "kind": o.Kind,
		"money": map[string]string{"amount": o.Amount, "currency": "BRL"},
	}
	if o.Ref != "" {
		b["referenceExternalTransactionId"] = o.Ref
	}
	return b
}

// --- SQS -------------------------------------------------------------------

// send publica a operação na fila de entrada (direto no LocalStack, sem o
// Toxiproxy). dedup diferente do messageId faz o SQS entregar cópias, para
// exercitar a deduplicação da aplicação.
func (c *cluster) send(messageID, dedup string, op operation) {
	c.t.Helper()
	data := op.body()
	data["idempotencyKey"] = op.key()
	raw, _ := json.Marshal(map[string]any{"messageId": messageID, "type": "WagerTransactionRequested",
		"occurredAt": "2026-09-08T12:00:00.000Z", "data": data}) // fixo: cópias idênticas byte a byte
	if _, err := sqsClient.SendMessage(context.Background(), &awssqs.SendMessageInput{QueueUrl: aws.String(c.queueURL),
		MessageBody: aws.String(string(raw)), MessageGroupId: aws.String(op.W.ID),
		MessageDeduplicationId: aws.String(dedup)}); err != nil {
		c.t.Fatal(err)
	}
}

func queueDepth(t *testing.T, url string) int {
	t.Helper()
	out, err := sqsClient.GetQueueAttributes(context.Background(), &awssqs.GetQueueAttributesInput{QueueUrl: aws.String(url),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameApproximateNumberOfMessages,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible}})
	if err != nil {
		t.Fatal(err)
	}
	visible, _ := strconv.Atoi(out.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessages)])
	inflight, _ := strconv.Atoi(out.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible)])
	return visible + inflight
}

// drainAudit lê e remove o que chegou à fila assinante dos eventos,
// acumulando quantas vezes cada eventId foi entregue.
func (c *cluster) drainAudit() {
	for {
		out, err := sqsClient.ReceiveMessage(context.Background(), &awssqs.ReceiveMessageInput{QueueUrl: aws.String(c.auditURL),
			MaxNumberOfMessages: 10, WaitTimeSeconds: 1})
		if err != nil {
			c.t.Fatal(err)
		}
		if len(out.Messages) == 0 {
			return
		}
		for _, m := range out.Messages {
			var env struct {
				EventID   string `json:"eventId"`
				EventType string `json:"eventType"`
				Data      struct {
					WalletID      string `json:"walletId"`
					WalletVersion int    `json:"walletVersion"`
				} `json:"data"`
			}
			_ = json.Unmarshal([]byte(aws.ToString(m.Body)), &env)
			c.seenAudit[env.EventID]++
			if env.EventType == "WalletBalanceChanged" {
				c.versions[env.Data.WalletID] = append(c.versions[env.Data.WalletID], env.Data.WalletVersion)
			}
			_, _ = sqsClient.DeleteMessage(context.Background(), &awssqs.DeleteMessageInput{QueueUrl: aws.String(c.auditURL),
				ReceiptHandle: m.ReceiptHandle})
		}
	}
}

// --- Verificações no banco ----------------------------------------------------

func (c *cluster) count(sql string, args ...any) int {
	c.t.Helper()
	var n int
	if err := c.db.App.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		c.t.Fatal(err)
	}
	return n
}

func (c *cluster) balance(w wallet) string {
	c.t.Helper()
	var minor int64
	if err := c.db.App.QueryRow(context.Background(), `SELECT balance_minor FROM wallets WHERE id = $1`, w.ID).Scan(&minor); err != nil {
		c.t.Fatal(err)
	}
	return fmt.Sprintf("%d.%02d", minor/100, minor%100)
}

// assertConsistent: saldo de cada carteira igual ao ledger, nenhuma
// transação com mais de um lançamento e, ao fim, toda a outbox publicada e
// entregue exatamente uma vez à fila assinante.
func (c *cluster) assertConsistent() {
	c.t.Helper()
	if n := c.count(`SELECT count(*) FROM wallets w WHERE w.balance_minor <> COALESCE((SELECT sum(CASE WHEN l.direction = 'CREDIT'
		THEN l.amount_minor ELSE -l.amount_minor END) FROM wallet_ledger_entries l WHERE l.wallet_id = w.id), 0)`); n != 0 {
		c.t.Errorf("%d wallets diverge from their ledger", n)
	}
	if n := c.count(`SELECT count(*) FROM (SELECT transaction_id FROM wallet_ledger_entries GROUP BY transaction_id HAVING count(*) > 1) d`); n != 0 {
		c.t.Errorf("%d transactions have more than one ledger entry", n)
	}
	waitFor(c.t, 60*time.Second, "outbox fully published", func() bool {
		return c.count(`SELECT count(*) FROM outbox_events WHERE published_at IS NULL`) == 0
	})
	total := c.count(`SELECT count(*) FROM outbox_events`)
	waitFor(c.t, 60*time.Second, "every event delivered to the subscriber", func() bool {
		c.drainAudit()
		return len(c.seenAudit) >= total
	})
	for id, n := range c.seenAudit {
		if n != 1 {
			c.t.Errorf("event %s delivered %d times", id, n)
		}
	}
	if len(c.seenAudit) != total {
		c.t.Errorf("subscriber received %d distinct events, outbox has %d", len(c.seenAudit), total)
	}
}

// --- Toxiproxy ---------------------------------------------------------------

func toxiproxyURL() string {
	if u := os.Getenv("TOXIPROXY_URL"); u != "" {
		return u
	}
	return "http://toxiproxy:8474"
}

// setProxy liga ou desliga um proxy. Desligado, o Toxiproxy recusa conexões
// novas e derruba as abertas.
func setProxy(t *testing.T, name string, enabled bool) {
	t.Helper()
	body := fmt.Sprintf(`{"enabled":%t}`, enabled)
	res, err := http.Post(toxiproxyURL()+"/proxies/"+name, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("toxiproxy %s enabled=%t: status %d", name, enabled, res.StatusCode)
	}
}

// addResponseLatency atrasa as respostas que voltam por um proxy (removida
// pelo reset ao fim do teste). As requisições chegam ao destino sem atraso.
func addResponseLatency(t *testing.T, proxy string, ms int) {
	t.Helper()
	body := fmt.Sprintf(`{"name":"latency","type":"latency","stream":"downstream","attributes":{"latency":%d}}`, ms)
	res, err := http.Post(toxiproxyURL()+"/proxies/"+proxy+"/toxics", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("toxiproxy latency on %s: status %d", proxy, res.StatusCode)
	}
}

// toxiproxyReset religa todos os proxies e remove toxics de testes anteriores.
func toxiproxyReset(t *testing.T) {
	t.Helper()
	res, err := http.Post(toxiproxyURL()+"/reset", "application/json", nil)
	if err != nil {
		t.Fatalf("toxiproxy unreachable (is the Compose stack up?): %v", err)
	}
	res.Body.Close()
	t.Cleanup(func() {
		if res, err := http.Post(toxiproxyURL()+"/reset", "application/json", nil); err == nil {
			res.Body.Close()
		}
	})
}

// viaToxiproxy troca host e porta da URL do banco pelo proxy.
func viaToxiproxy(t *testing.T, raw, hostPort string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = hostPort
	return u.String()
}

// --- Utilidades ----------------------------------------------------------------

func createQueue(t *testing.T, name string, attrs map[string]string) string {
	t.Helper()
	all := map[string]string{"FifoQueue": "true"}
	for k, v := range attrs {
		all[k] = v
	}
	q, err := sqsClient.CreateQueue(context.Background(), &awssqs.CreateQueueInput{QueueName: aws.String(name), Attributes: all})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = sqsClient.DeleteQueue(context.Background(), &awssqs.DeleteQueueInput{QueueUrl: q.QueueUrl})
	})
	return aws.ToString(q.QueueUrl)
}

func queueArn(t *testing.T, url string) string {
	t.Helper()
	out, err := sqsClient.GetQueueAttributes(context.Background(), &awssqs.GetQueueAttributesInput{QueueUrl: aws.String(url),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn}})
	if err != nil {
		t.Fatal(err)
	}
	return out.Attributes[string(sqstypes.QueueAttributeNameQueueArn)]
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func suffix() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newID gera um id de jogador no formato aceito pela API.
func newID(t *testing.T) string {
	t.Helper()
	id, err := domain.NewID[domain.PlayerID]()
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
