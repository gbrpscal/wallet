//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssns "github.com/aws/aws-sdk-go-v2/service/sns"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/fx/fxtest"

	"github.com/gbrpscal/wallet/internal/adapters/sns"
	wsqs "github.com/gbrpscal/wallet/internal/adapters/sqs"
	"github.com/gbrpscal/wallet/internal/bootstrap"
	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/platform/config"
	"github.com/gbrpscal/wallet/internal/testutil/pgtest"
)

// hookRecorder guarda, em ordem, quem registrou cada hook de stop executado.
type hookRecorder struct {
	mu      sync.Mutex
	stopped []string
	errs    []error
}

func (r *hookRecorder) LogEvent(e fxevent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch ev := e.(type) {
	case *fxevent.OnStopExecuted:
		r.stopped = append(r.stopped, ev.CallerName)
		if ev.Err != nil {
			r.errs = append(r.errs, fmt.Errorf("%s: %w", ev.CallerName, ev.Err))
		}
	case *fxevent.OnStartExecuted:
		if ev.Err != nil {
			r.errs = append(r.errs, fmt.Errorf("%s: %w", ev.CallerName, ev.Err))
		}
	}
}

// position devolve o índice do primeiro hook cujo registrador contém name.
func (r *hookRecorder) position(t *testing.T, name string) int {
	t.Helper()
	for i, c := range r.stopped {
		if strings.Contains(c, name) {
			return i
		}
	}
	t.Fatalf("no stop hook registered by %s in %v", name, r.stopped)
	return -1
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

// TestCompositionLifecycle sobe a composição real (a mesma do main) sobre
// recursos exclusivos do teste, faz uma operação de ponta a ponta e confere o
// encerramento: workers, servidor HTTP, consumidor e, por último, o pool;
// nenhum hook com erro e os recursos de fato liberados.
func TestCompositionLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := pgtest.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)

	base := config.Config{AWSRegion: os.Getenv("AWS_REGION"), AWSEndpointURL: os.Getenv("AWS_ENDPOINT_URL")}
	sqsClient, err := wsqs.NewClient(base)
	if err != nil {
		t.Fatal(err)
	}
	snsClient, err := sns.NewClient(base)
	if err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(db.Name, "wallet_test_", "")
	queueURL := createQueue(t, sqsClient, "fx-"+suffix+".fifo")
	createQueue(t, sqsClient, "fx-"+suffix+"-dlq.fifo")
	topic, err := snsClient.CreateTopic(ctx, &awssns.CreateTopicInput{Name: aws.String("fx-" + suffix + ".fifo"),
		Attributes: map[string]string{"FifoTopic": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = snsClient.DeleteTopic(context.Background(), &awssns.DeleteTopicInput{TopicArn: topic.TopicArn})
	})

	httpAddr := freeAddr(t)
	for k, v := range map[string]string{
		"DATABASE_URL":             db.AppURL,
		"HTTP_ADDR":                httpAddr,
		"METRICS_ADDR":             freeAddr(t),
		"SQS_WAGER_QUEUE_NAME":     "fx-" + suffix + ".fifo",
		"SQS_WAGER_DLQ_NAME":       "fx-" + suffix + "-dlq.fifo",
		"SQS_EVENTS_DLQ_NAME":      "fx-" + suffix + "-dlq.fifo",
		"SNS_EVENTS_TOPIC_ARN":     aws.ToString(topic.TopicArn),
		"SQS_WAIT_TIME":            "1s",
		"SHUTDOWN_TIMEOUT":         "10s",
		"METRICS_REFRESH_INTERVAL": "1s",
		"LOG_LEVEL":                "error",
	} {
		t.Setenv(k, v)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	rec := &hookRecorder{}
	var pool *pgxpool.Pool
	fxApp := fxtest.New(t,
		bootstrap.Options(cfg),
		fx.WithLogger(func() fxevent.Logger { return rec }),
		fx.Populate(&pool),
	)
	fxApp.RequireStart()

	// Operação real: abertura pela API; os eventos saem pelo publisher.
	openWallet(t, "http://"+httpAddr)
	waitFor(t, "outbox events published by the worker", func() bool {
		var pending, published int
		err := db.App.QueryRow(ctx, `SELECT count(*) FILTER (WHERE published_at IS NULL),
			count(*) FILTER (WHERE published_at IS NOT NULL) FROM outbox_events`).Scan(&pending, &published)
		if err != nil {
			t.Fatal(err)
		}
		return pending == 0 && published == 2 // banco exclusivo: só os 2 eventos da abertura
	})

	fxApp.RequireStop()

	if len(rec.errs) > 0 {
		t.Fatalf("lifecycle hooks failed: %v", rec.errs)
	}
	// Ordem inversa do registro: quem usa as dependências para antes delas.
	outbox := rec.position(t, "worker.StartOutboxWorker")
	reference := rec.position(t, "worker.StartReferenceWorker")
	gauges := rec.position(t, "worker.StartGaugeRefresher")
	server := rec.position(t, "httpapi.StartServer")
	consumer := rec.position(t, "sqs.StartConsumer")
	poolClose := rec.position(t, "postgres.NewPool")
	for name, i := range map[string]int{"outbox worker": outbox, "reference worker": reference, "gauge refresher": gauges} {
		if i > server {
			t.Errorf("%s stopped after the HTTP server: %v", name, rec.stopped)
		}
	}
	if !(server < consumer && consumer < poolClose) {
		t.Errorf("stop order = %v; want HTTP server, then consumer, then pool", rec.stopped)
	}

	// Recursos liberados: pool fechado, porta HTTP livre, consumidor parado.
	if _, err := pool.Acquire(ctx); err == nil {
		t.Error("pool still accepts connections after stop")
	}
	if conn, err := net.DialTimeout("tcp", httpAddr, time.Second); err == nil {
		conn.Close()
		t.Error("HTTP server still listening after stop")
	}
	// Um long polling aberto antes do stop termina no broker em até
	// SQS_WAIT_TIME (1s) mesmo com o cliente cancelado; o envio vem depois.
	time.Sleep(2 * time.Second)
	if _, err := sqsClient.SendMessage(ctx, &awssqs.SendMessageInput{QueueUrl: aws.String(queueURL),
		MessageBody: aws.String("{}"), MessageGroupId: aws.String("g"), MessageDeduplicationId: aws.String("after-stop")}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	attrs, err := sqsClient.GetQueueAttributes(ctx, &awssqs.GetQueueAttributesInput{QueueUrl: aws.String(queueURL),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameApproximateNumberOfMessages,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible}})
	if err != nil {
		t.Fatal(err)
	}
	visible := attrs.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessages)]
	inflight := attrs.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible)]
	if visible != "1" || inflight != "0" {
		t.Errorf("message sent after stop: visible %s, in flight %s; want 1 and 0 (consumer still receiving)", visible, inflight)
	}
}

func createQueue(t *testing.T, client *awssqs.Client, name string) string {
	t.Helper()
	q, err := client.CreateQueue(context.Background(), &awssqs.CreateQueueInput{QueueName: aws.String(name),
		Attributes: map[string]string{"FifoQueue": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = client.DeleteQueue(context.Background(), &awssqs.DeleteQueueInput{QueueUrl: q.QueueUrl})
	})
	return aws.ToString(q.QueueUrl)
}

func openWallet(t *testing.T, baseURL string) {
	t.Helper()
	player, _ := domain.NewID[domain.PlayerID]()
	body := fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":"100.00","currency":"BRL"}}`, player)
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/wallets", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token(t, "wallet-backoffice"))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("open wallet: status %d", res.StatusCode)
	}
}

func token(t *testing.T, client string) string {
	t.Helper()
	form := url.Values{"grant_type": {"client_credentials"}}
	req, _ := http.NewRequest(http.MethodPost, os.Getenv("OIDC_TOKEN_URL"), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(client, client+"-secret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
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

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
