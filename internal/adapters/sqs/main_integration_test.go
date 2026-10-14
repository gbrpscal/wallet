//go:build integration

package sqs_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gbrpscal/wallet/internal/adapters/postgres"
	wsqs "github.com/gbrpscal/wallet/internal/adapters/sqs"
	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/event"
	"github.com/gbrpscal/wallet/internal/domain/wagering"
	"github.com/gbrpscal/wallet/internal/platform/config"
	"github.com/gbrpscal/wallet/internal/testutil/pgtest"
)

var (
	client  *sqs.Client
	svc     *app.Service
	appPool *pgxpool.Pool
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	db, err := pgtest.New(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	appPool = db.App
	client, err = wsqs.NewClient(config.Config{AWSRegion: os.Getenv("AWS_REGION"), AWSEndpointURL: os.Getenv("AWS_ENDPOINT_URL")})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	svc = app.NewService(postgres.NewUnitOfWork(db.App, 5*time.Second, app.NopMetrics{}), app.SystemClock{},
		app.RetryPolicy{Retries: 1, BaseDelay: 10 * time.Millisecond}, app.DefaultReferencePolicy, app.NopMetrics{}, log)
	code := m.Run()
	db.Close()
	os.Exit(code)
}

func randomSuffix() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// queues é um par de filas exclusivo de um teste, com redrive após 5 recebimentos.
type queues struct {
	name, dlqName string
	url, dlqURL   string
}

func newQueues(t *testing.T) queues {
	t.Helper()
	ctx := context.Background()
	s := randomSuffix()
	q := queues{name: "test-" + s + ".fifo", dlqName: "test-" + s + "-dlq.fifo"}
	dlq, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(q.dlqName),
		Attributes: map[string]string{"FifoQueue": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	q.dlqURL = aws.ToString(dlq.QueueUrl)
	attrs, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: dlq.QueueUrl,
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn}})
	if err != nil {
		t.Fatal(err)
	}
	redrive := fmt.Sprintf(`{"deadLetterTargetArn":"%s","maxReceiveCount":"5"}`, attrs.Attributes["QueueArn"])
	main, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(q.name),
		Attributes: map[string]string{"FifoQueue": "true", "VisibilityTimeout": "30", "RedrivePolicy": redrive}})
	if err != nil {
		t.Fatal(err)
	}
	q.url = aws.ToString(main.QueueUrl)
	t.Cleanup(func() {
		_, _ = client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: aws.String(q.url)})
		_, _ = client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: aws.String(q.dlqURL)})
	})
	return q
}

// handlerFunc permite injetar falhas em volta do caso de uso real.
type handlerFunc func(ctx context.Context, msg app.InboxMessage, in wagering.RequestInput, trace event.Trace) (app.MessageResult, error)

func (f handlerFunc) HandleMessage(ctx context.Context, msg app.InboxMessage, in wagering.RequestInput, trace event.Trace) (app.MessageResult, error) {
	return f(ctx, msg, in, trace)
}

// consumer inicia um consumidor rápido (long polling de 1s, visibilidade de
// 2s, backoff de 1s a 2s) sobre as filas do teste.
func consumer(t *testing.T, q queues, h wsqs.MessageHandler) *wsqs.Consumer {
	t.Helper()
	return consumerWith(t, q, h, app.NopMetrics{})
}

// consumerWith é o consumer com as métricas informadas.
func consumerWith(t *testing.T, q queues, h wsqs.MessageHandler, m app.Metrics) *wsqs.Consumer {
	t.Helper()
	if h == nil {
		h = svc
	}
	c := wsqs.NewConsumer(client, h, wsqs.ConsumerConfig{
		Name: "test-consumer-" + q.name, QueueName: q.name, DLQName: q.dlqName, Pollers: 2,
		WaitTime: time.Second, Visibility: 2 * time.Second, RetryBase: time.Second, RetryMax: 2 * time.Second,
	}, m, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.Stop(ctx)
	})
	return c
}

type wallet struct{ id, player string }

func openWallet(t *testing.T, amount string) wallet {
	t.Helper()
	player, _ := domain.NewID[domain.PlayerID]()
	w, err := svc.OpenWallet(context.Background(), app.OpenWalletInput{PlayerID: player.String(), Amount: amount, Currency: "BRL"},
		event.Trace{CorrelationID: "open"})
	if err != nil {
		t.Fatal(err)
	}
	return wallet{id: w.ID().String(), player: player.String()}
}

// envelope monta a mensagem no formato do enunciado.
func envelope(messageID string, w wallet, extID, kind, amount, ref string) string {
	data := map[string]any{
		"providerId": "provider-a", "externalTransactionId": extID, "idempotencyKey": "provider-a:" + extID,
		"playerId": w.player, "walletId": w.id, "roundId": "round-987", "gameId": "fortune-chimp",
		"kind": kind, "money": map[string]string{"amount": amount, "currency": "BRL"},
	}
	if ref != "" {
		data["referenceExternalTransactionId"] = ref
	}
	raw, _ := json.Marshal(map[string]any{
		"messageId": messageID, "type": "WagerTransactionRequested",
		"occurredAt": "2026-09-08T12:00:00.000Z", "data": data,
	})
	return string(raw)
}

// send publica com MessageGroupId = walletId e uma MessageDeduplicationId
// própria, para que reenvios cheguem ao consumidor (sem a deduplicação do SQS).
func send(t *testing.T, q queues, group, body string) {
	t.Helper()
	_, err := client.SendMessage(context.Background(), &sqs.SendMessageInput{
		QueueUrl: aws.String(q.url), MessageBody: aws.String(body),
		MessageGroupId: aws.String(group), MessageDeduplicationId: aws.String(randomSuffix()),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := appPool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func balance(t *testing.T, w wallet) string {
	t.Helper()
	got, err := svc.GetWallet(context.Background(), domain.MustParseID[domain.WalletID](w.id))
	if err != nil {
		t.Fatal(err)
	}
	return got.Balance().Amount()
}

func ledgerEntries(t *testing.T, w wallet) int {
	return count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, w.id)
}

// queueDepth soma mensagens visíveis e em voo.
func queueDepth(t *testing.T, url string) int {
	t.Helper()
	out, err := client.GetQueueAttributes(context.Background(), &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(url),
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameApproximateNumberOfMessages,
			types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var visible, inflight int
	fmt.Sscan(out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)], &visible)
	fmt.Sscan(out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessagesNotVisible)], &inflight)
	return visible + inflight
}

// deadLetters lê as mensagens da DLQ com seus atributos.
func deadLetters(t *testing.T, q queues, want int) []types.Message {
	t.Helper()
	var got []types.Message
	eventually(t, 20*time.Second, fmt.Sprintf("%d dead letters", want), func() bool {
		out, err := client.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(q.dlqURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
			MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, out.Messages...)
		// A DLQ também é FIFO: enquanto houver mensagens de um grupo em voo, as
		// seguintes do mesmo grupo não são entregues. Remover libera o grupo.
		for _, m := range out.Messages {
			_, _ = client.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{QueueUrl: aws.String(q.dlqURL), ReceiptHandle: m.ReceiptHandle})
		}
		return len(got) >= want
	})
	return got
}

// counter conta chamadas concorrentes de forma segura.
type counter struct{ n atomic.Int64 }

func (c *counter) inc() int64 { return c.n.Add(1) }
