//go:build integration

package worker_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssns "github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/gbrpscal/wallet/internal/adapters/postgres"
	"github.com/gbrpscal/wallet/internal/adapters/sns"
	wsqs "github.com/gbrpscal/wallet/internal/adapters/sqs"
	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/event"
	"github.com/gbrpscal/wallet/internal/domain/wagering"
	"github.com/gbrpscal/wallet/internal/platform/config"
	"github.com/gbrpscal/wallet/internal/testutil/pgtest"
)

var (
	snsClient *awssns.Client
	sqsClient *sqs.Client
	quiet     = slog.New(slog.NewJSONHandler(io.Discard, nil))
)

func TestMain(m *testing.M) {
	cfg := config.Config{AWSRegion: os.Getenv("AWS_REGION"), AWSEndpointURL: os.Getenv("AWS_ENDPOINT_URL")}
	var err error
	if snsClient, err = sns.NewClient(cfg); err == nil {
		sqsClient, err = wsqs.NewClient(cfg)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func suffix() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// env é o ambiente isolado de um teste: banco temporário próprio, casos de
// uso sobre ele e um tópico SNS FIFO com fila assinante (entrega raw).
type env struct {
	db       *pgtest.DB
	uow      *postgres.UnitOfWork
	svc      *app.Service
	topicARN string
	queueURL string
}

// fastReference: tentativas rápidas para os testes do worker de referências.
var fastReference = app.ReferencePolicy{MaxAttempts: 20, TTL: time.Minute, BaseDelay: 100 * time.Millisecond, MaxDelay: 200 * time.Millisecond}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db, err := pgtest.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	e := &env{db: db, uow: postgres.NewUnitOfWork(db.App, 5*time.Second)}
	e.svc = app.NewService(e.uow, app.SystemClock{}, app.RetryPolicy{Retries: 3, BaseDelay: 10 * time.Millisecond}, fastReference, quiet)

	s := suffix()
	topic, err := snsClient.CreateTopic(ctx, &awssns.CreateTopicInput{Name: aws.String("events-" + s + ".fifo"),
		Attributes: map[string]string{"FifoTopic": "true", "ContentBasedDeduplication": "false"}})
	if err != nil {
		t.Fatal(err)
	}
	e.topicARN = aws.ToString(topic.TopicArn)
	e.queueURL = createQueue(t, "events-"+s+"-sub.fifo")
	subscribe(t, e.topicARN, e.queueURL)
	t.Cleanup(func() {
		_, _ = snsClient.DeleteTopic(context.Background(), &awssns.DeleteTopicInput{TopicArn: aws.String(e.topicARN)})
	})
	return e
}

func createQueue(t *testing.T, name string) string {
	t.Helper()
	q, err := sqsClient.CreateQueue(context.Background(), &sqs.CreateQueueInput{QueueName: aws.String(name),
		Attributes: map[string]string{"FifoQueue": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	url := aws.ToString(q.QueueUrl)
	t.Cleanup(func() {
		_, _ = sqsClient.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: aws.String(url)})
	})
	return url
}

func queueArn(t *testing.T, url string) string {
	t.Helper()
	a, err := sqsClient.GetQueueAttributes(context.Background(), &sqs.GetQueueAttributesInput{QueueUrl: aws.String(url),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn}})
	if err != nil {
		t.Fatal(err)
	}
	return a.Attributes["QueueArn"]
}

func subscribe(t *testing.T, topicARN, queueURL string) string {
	t.Helper()
	sub, err := snsClient.Subscribe(context.Background(), &awssns.SubscribeInput{TopicArn: aws.String(topicARN),
		Protocol: aws.String("sqs"), Endpoint: aws.String(queueArn(t, queueURL)),
		Attributes: map[string]string{"RawMessageDelivery": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	return aws.ToString(sub.SubscriptionArn)
}

// recorder registra cada chamada ao broker e pode falhar ou atrasar chamadas.
type recorder struct {
	next app.EventPublisher

	mu    sync.Mutex
	calls []app.OutboxEvent
	// failFirst falha as primeiras chamadas (broker indisponível).
	failFirst int
	// delay atrasa cada chamada (broker lento).
	delay time.Duration
	// failIDs falha as chamadas destes eventos enquanto o valor for > 0.
	failIDs map[domain.EventID]int
}

func (r *recorder) Publish(ctx context.Context, e app.OutboxEvent) error {
	r.mu.Lock()
	r.calls = append(r.calls, e)
	fail := len(r.calls) <= r.failFirst
	if n := r.failIDs[e.ID]; n > 0 {
		r.failIDs[e.ID] = n - 1
		fail = true
	}
	delay := r.delay
	r.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if fail {
		return fmt.Errorf("broker unavailable")
	}
	return r.next.Publish(ctx, e)
}

func (r *recorder) callsFor(id domain.EventID) []app.OutboxEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []app.OutboxEvent
	for _, c := range r.calls {
		if c.ID == id {
			out = append(out, c)
		}
	}
	return out
}

func (r *recorder) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (e *env) recorder() *recorder {
	return &recorder{next: sns.NewPublisher(snsClient, e.topicARN), failIDs: map[domain.EventID]int{}}
}

func (e *env) relay(owner string, pub app.EventPublisher, lease time.Duration) *app.OutboxRelay {
	return app.NewOutboxRelay(e.uow, pub, app.SystemClock{}, app.OutboxRelayConfig{
		Owner: owner, BatchSize: 50, Lease: lease, Concurrency: 8,
		RetryBase: 100 * time.Millisecond, RetryMax: 400 * time.Millisecond,
	}, quiet)
}

// drain publica até não sobrar evento pendente (ou o prazo acabar).
func drain(t *testing.T, e *env, relay *app.OutboxRelay, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := relay.RunOnce(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		if e.pending(t) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("outbox still has %d pending events", e.pending(t))
}

func (e *env) pending(t *testing.T) int {
	return e.count(t, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`)
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.App.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// received lê e remove todas as mensagens da fila assinante até ter want
// (a fila é FIFO: remover libera os grupos).
func received(t *testing.T, queueURL string, want int, within time.Duration) []sqstypes.Message {
	t.Helper()
	var got []sqstypes.Message
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) && len(got) < want {
		out, err := sqsClient.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(queueURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
			MessageAttributeNames:       []string{"All"},
			MessageSystemAttributeNames: []sqstypes.MessageSystemAttributeName{sqstypes.MessageSystemAttributeNameMessageGroupId},
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range out.Messages {
			got = append(got, m)
			_, _ = sqsClient.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{QueueUrl: aws.String(queueURL), ReceiptHandle: m.ReceiptHandle})
		}
	}
	return got
}

type wallet struct{ id, player string }

func (e *env) openWallet(t *testing.T, amount string) wallet {
	t.Helper()
	player, _ := domain.NewID[domain.PlayerID]()
	w, err := e.svc.OpenWallet(context.Background(), app.OpenWalletInput{PlayerID: player.String(), Amount: amount, Currency: "BRL"},
		event.Trace{CorrelationID: "open"})
	if err != nil {
		t.Fatal(err)
	}
	return wallet{id: w.ID().String(), player: player.String()}
}

func (e *env) submit(t *testing.T, w wallet, extID, kind, amount, ref string) app.OperationResult {
	t.Helper()
	res, err := e.svc.SubmitOperation(context.Background(), opInput(w, extID, kind, amount, ref), event.Trace{CorrelationID: extID})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func opInput(w wallet, extID, kind, amount, ref string) wagering.RequestInput {
	return wagering.RequestInput{
		ProviderID: "provider-a", ExternalTransactionID: extID, IdempotencyKey: "provider-a:" + extID,
		PlayerID: w.player, WalletID: w.id, RoundID: "round-1", GameID: "game-1",
		Kind: kind, Amount: amount, Currency: "BRL", ReferenceExternalTransactionID: ref,
	}
}
