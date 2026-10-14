//go:build integration

package sqs_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/prometheus/client_golang/prometheus"

	wsqs "github.com/gbrpscal/wallet/internal/adapters/sqs"
	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain/event"
	"github.com/gbrpscal/wallet/internal/domain/wagering"
	"github.com/gbrpscal/wallet/internal/platform/metrics"
	"github.com/gbrpscal/wallet/internal/platform/metrics/metricstest"
)

func TestConsumesOperation(t *testing.T) {
	q := newQueues(t)
	consumer(t, q, nil)
	w := openWallet(t, "1000.00")

	send(t, q, w.id, envelope("msg-123", w, "transaction-123", "BET", "25.00", ""))
	eventually(t, 15*time.Second, "bet processed", func() bool { return balance(t, w) == "975.00" })
	eventually(t, 10*time.Second, "message deleted", func() bool { return queueDepth(t, q.url) == 0 })

	if n := count(t, `SELECT count(*) FROM inbox_messages WHERE message_id = 'msg-123' AND completed_at IS NOT NULL`); n != 1 {
		t.Fatalf("completed inbox rows = %d", n)
	}
	// O correlationId dos eventos é o messageId do envelope.
	if n := count(t, `SELECT count(*) FROM outbox_events o JOIN wager_transactions t ON o.aggregate_id = t.id
		WHERE t.external_transaction_id = 'transaction-123' AND t.wallet_id = $1 AND o.payload::jsonb->>'correlationId' = 'msg-123'`, w.id); n != 1 {
		t.Fatalf("events correlated to the message = %d", n)
	}
}

// TestRedeliveredMessageIsNotReprocessed: o mesmo envelope recebido duas
// vezes (sem a deduplicação do SQS) gera um único débito, pela inbox.
func TestRedeliveredMessageIsNotReprocessed(t *testing.T) {
	q := newQueues(t)
	w := openWallet(t, "100.00")
	body := envelope("msg-dup", w, "dup-1", "BET", "10.00", "")
	send(t, q, w.id, body)
	send(t, q, w.id, body)
	consumer(t, q, nil)

	eventually(t, 15*time.Second, "both deliveries acknowledged", func() bool { return queueDepth(t, q.url) == 0 })
	if balance(t, w) != "90.00" || ledgerEntries(t, w) != 2 {
		t.Fatalf("balance %s, entries %d", balance(t, w), ledgerEntries(t, w))
	}
	if n := count(t, `SELECT count(*) FROM inbox_messages WHERE message_id = 'msg-dup'`); n != 1 {
		t.Fatalf("inbox rows = %d", n)
	}
}

// TestDifferentMessagesSameKey: mensagens distintas com a mesma chave de
// idempotência resultam em um único débito (replay).
func TestDifferentMessagesSameKey(t *testing.T) {
	q := newQueues(t)
	consumer(t, q, nil)
	w := openWallet(t, "100.00")
	send(t, q, w.id, envelope("msg-a", w, "same-key", "BET", "10.00", ""))
	send(t, q, w.id, envelope("msg-b", w, "same-key", "BET", "10.00", ""))

	eventually(t, 15*time.Second, "both messages completed", func() bool {
		return count(t, `SELECT count(*) FROM inbox_messages WHERE message_id IN ('msg-a','msg-b') AND completed_at IS NOT NULL`) == 2
	})
	if balance(t, w) != "90.00" || ledgerEntries(t, w) != 2 {
		t.Fatalf("balance %s, entries %d", balance(t, w), ledgerEntries(t, w))
	}
}

// TestHTTPAndSQSShareIdempotency: a mesma operação pelo caso de uso usado no
// HTTP e pela fila gera um único débito, nas duas ordens.
func TestHTTPAndSQSShareIdempotency(t *testing.T) {
	q := newQueues(t)
	consumer(t, q, nil)
	w := openWallet(t, "100.00")
	in := func(extID string) wagering.RequestInput {
		return wagering.RequestInput{ProviderID: "provider-a", ExternalTransactionID: extID, IdempotencyKey: "provider-a:" + extID,
			PlayerID: w.player, WalletID: w.id, RoundID: "round-987", GameID: "fortune-chimp", Kind: "BET", Amount: "10.00", Currency: "BRL"}
	}

	// HTTP primeiro, SQS depois.
	if _, err := svc.SubmitOperation(context.Background(), in("cross-1"), event.Trace{CorrelationID: "http"}); err != nil {
		t.Fatal(err)
	}
	send(t, q, w.id, envelope("msg-cross-1", w, "cross-1", "BET", "10.00", ""))
	eventually(t, 15*time.Second, "sqs replay completed", func() bool {
		return count(t, `SELECT count(*) FROM inbox_messages WHERE message_id = 'msg-cross-1' AND completed_at IS NOT NULL`) == 1
	})

	// SQS primeiro, HTTP depois.
	send(t, q, w.id, envelope("msg-cross-2", w, "cross-2", "BET", "10.00", ""))
	eventually(t, 15*time.Second, "sqs bet processed", func() bool { return balance(t, w) == "80.00" })
	res, err := svc.SubmitOperation(context.Background(), in("cross-2"), event.Trace{CorrelationID: "http"})
	if err != nil || !res.Replay {
		t.Fatalf("http after sqs: replay=%v err=%v", res.Replay, err)
	}
	if balance(t, w) != "80.00" || ledgerEntries(t, w) != 3 {
		t.Fatalf("balance %s, entries %d", balance(t, w), ledgerEntries(t, w))
	}
}

func TestInvalidMessagesGoToDeadLetterQueue(t *testing.T) {
	q := newQueues(t)
	reg := prometheus.NewRegistry()
	consumerWith(t, q, svc, metrics.New(reg))
	w := openWallet(t, "100.00")
	valid := envelope("msg-valid", w, "inv-ok", "BET", "1.00", "")

	cases := map[string]struct{ body, code string }{
		"malformed json": {`{"messageId":`, "INVALID_MESSAGE"},
		"unknown type":   {strings.Replace(valid, "WagerTransactionRequested", "Other", 1), "INVALID_MESSAGE"},
		"missing data":   {`{"messageId":"m","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00Z"}`, "INVALID_MESSAGE"},
		"unknown field":  {strings.Replace(valid, `"type"`, `"extra":1,"type"`, 1), "INVALID_MESSAGE"},
		"numeric amount": {strings.Replace(valid, `"amount":"1.00"`, `"amount":1.00`, 1), "INVALID_AMOUNT"},
		"opening kind":   {strings.Replace(envelope("msg-open", w, "inv-open", "BET", "1.00", ""), `"BET"`, `"OPENING"`, 1), "KIND_NOT_ALLOWED"},
		"unknown wallet": {strings.Replace(envelope("msg-nowallet", w, "inv-nw", "BET", "1.00", ""), w.id, "0192f291-27dd-7d3f-8071-00000000000b", 1), "WALLET_NOT_FOUND"},
		"zero bet":       {envelope("msg-zero", w, "inv-zero", "BET", "0.00", ""), "AMOUNT_MUST_BE_POSITIVE"},
	}
	want := map[string]string{}
	perCode := map[string]float64{}
	for _, c := range cases {
		send(t, q, w.id, c.body)
		want[c.body] = c.code
		perCode[c.code]++
	}
	letters := deadLetters(t, q, len(cases))
	for _, m := range letters {
		body := aws.ToString(m.Body)
		code := aws.ToString(m.MessageAttributes["ErrorCode"].StringValue)
		if want[body] != code {
			t.Errorf("dead letter %q has code %s, want %s", body, code, want[body])
		}
		if aws.ToString(m.MessageAttributes["SourceQueue"].StringValue) != q.name {
			t.Errorf("dead letter without source queue")
		}
	}
	eventually(t, 10*time.Second, "main queue drained", func() bool { return queueDepth(t, q.url) == 0 })
	if balance(t, w) != "100.00" || ledgerEntries(t, w) != 1 {
		t.Fatalf("invalid messages moved money: %s", balance(t, w))
	}
	for code, n := range perCode {
		labels := map[string]string{"queue": q.dlqName, "error_code": code}
		if got := metricstest.Value(t, reg, "wallet_dead_letters_total", labels); got != n {
			t.Errorf("dead letters %v = %v, want %v", labels, got, n)
		}
	}
}

func TestMessageHashMismatchGoesToDeadLetterQueue(t *testing.T) {
	q := newQueues(t)
	consumer(t, q, nil)
	w := openWallet(t, "100.00")
	send(t, q, w.id, envelope("msg-reused", w, "hash-1", "BET", "10.00", ""))
	eventually(t, 15*time.Second, "first message processed", func() bool { return balance(t, w) == "90.00" })

	send(t, q, w.id, envelope("msg-reused", w, "hash-2", "BET", "20.00", ""))
	letters := deadLetters(t, q, 1)
	if code := aws.ToString(letters[0].MessageAttributes["ErrorCode"].StringValue); code != "MESSAGE_HASH_MISMATCH" {
		t.Fatalf("code = %s", code)
	}
	if balance(t, w) != "90.00" {
		t.Fatalf("reused messageId moved money: %s", balance(t, w))
	}
}

// TestBusinessOutcomesAreAcknowledged: rejeição persistida e espera por
// referência são resultados definitivos do tratamento; a mensagem é removida.
func TestBusinessOutcomesAreAcknowledged(t *testing.T) {
	q := newQueues(t)
	consumer(t, q, nil)
	w := openWallet(t, "10.00")
	send(t, q, w.id, envelope("msg-rejected", w, "out-big", "BET", "50.00", ""))
	send(t, q, w.id, envelope("msg-pending", w, "out-refund", "REFUND", "5.00", "out-missing"))

	eventually(t, 15*time.Second, "both messages completed", func() bool {
		return count(t, `SELECT count(*) FROM inbox_messages WHERE message_id IN ('msg-rejected','msg-pending') AND completed_at IS NOT NULL`) == 2
	})
	eventually(t, 10*time.Second, "queue drained", func() bool { return queueDepth(t, q.url) == 0 })
	if n := count(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND
		((external_transaction_id = 'out-big' AND status = 'REJECTED' AND failure_code = 'INSUFFICIENT_FUNDS')
		 OR (external_transaction_id = 'out-refund' AND status = 'PENDING_REFERENCE'))`, w.id); n != 2 {
		t.Fatalf("persisted outcomes = %d", n)
	}
	if queueDepth(t, q.dlqURL) != 0 {
		t.Fatal("business outcome sent to the dead-letter queue")
	}
}

// TestTransientFailureIsRetried: falhas transitórias não removem a mensagem;
// ela volta com backoff e é processada quando a dependência se recupera.
func TestTransientFailureIsRetried(t *testing.T) {
	q := newQueues(t)
	var calls counter
	reg := prometheus.NewRegistry()
	consumerWith(t, q, handlerFunc(func(ctx context.Context, msg app.InboxMessage, in wagering.RequestInput, tr event.Trace) (app.MessageResult, error) {
		if calls.inc() <= 2 {
			return app.MessageResult{}, &app.TransientError{Err: errors.New("database unavailable")}
		}
		return svc.HandleMessage(ctx, msg, in, tr)
	}), metrics.New(reg))
	w := openWallet(t, "100.00")
	send(t, q, w.id, envelope("msg-transient", w, "tr-1", "BET", "10.00", ""))

	eventually(t, 20*time.Second, "processed after retries", func() bool { return balance(t, w) == "90.00" })
	if calls.n.Load() < 3 || queueDepth(t, q.dlqURL) != 0 {
		t.Fatalf("calls %d, dead letters %d", calls.n.Load(), queueDepth(t, q.dlqURL))
	}
	retries := map[string]string{"component": "sqs_consumer", "reason": "transient"}
	if got := metricstest.Value(t, reg, "wallet_retries_total", retries); got != 2 {
		t.Errorf("consumer retries = %v, want 2", got)
	}
}

// TestExhaustedRetriesGoToDeadLetterQueue: uma falha transitória que não se
// resolve leva a mensagem à DLQ pelo redrive (maxReceiveCount 5).
func TestExhaustedRetriesGoToDeadLetterQueue(t *testing.T) {
	q := newQueues(t)
	var calls counter
	consumer(t, q, handlerFunc(func(context.Context, app.InboxMessage, wagering.RequestInput, event.Trace) (app.MessageResult, error) {
		calls.inc()
		return app.MessageResult{}, &app.TransientError{Err: errors.New("database unavailable")}
	}))
	w := openWallet(t, "100.00")
	send(t, q, w.id, envelope("msg-forever", w, "tr-forever", "BET", "10.00", ""))

	letters := deadLetters(t, q, 1)
	if _, tagged := letters[0].MessageAttributes["ErrorCode"]; tagged {
		t.Fatal("redriven message should come from the SQS redrive, not from the consumer")
	}
	if got := calls.n.Load(); got != 5 {
		t.Fatalf("handler called %d times, want 5 (maxReceiveCount)", got)
	}
	if balance(t, w) != "100.00" {
		t.Fatal("failed message moved money")
	}
}

// TestCrashAfterCommitBeforeDelete: o tratamento é confirmado, mas o
// consumidor cai antes de remover a mensagem. Na reentrega, a inbox reconhece
// a mensagem e ela é removida sem reprocessar.
func TestCrashAfterCommitBeforeDelete(t *testing.T) {
	q := newQueues(t)
	var handled counter
	c := consumer(t, q, handlerFunc(func(ctx context.Context, msg app.InboxMessage, in wagering.RequestInput, tr event.Trace) (app.MessageResult, error) {
		handled.inc()
		return svc.HandleMessage(ctx, msg, in, tr)
	}))
	var crashed counter
	c.SetAfterHandle(func(wsqs.Envelope) error {
		if crashed.inc() == 1 {
			return errors.New("process killed between commit and delete")
		}
		return nil
	})
	w := openWallet(t, "100.00")
	send(t, q, w.id, envelope("msg-crash", w, "crash-1", "BET", "10.00", ""))

	eventually(t, 20*time.Second, "redelivered and deleted", func() bool {
		return handled.n.Load() >= 2 && queueDepth(t, q.url) == 0
	})
	if balance(t, w) != "90.00" || ledgerEntries(t, w) != 2 {
		t.Fatalf("balance %s, entries %d (redelivery reprocessed the bet)", balance(t, w), ledgerEntries(t, w))
	}
}

// TestOrderWithinWallet: mensagens da mesma carteira são tratadas na ordem de
// envio. Com 100.00, a aposta de 80 (primeira) passa e a de 30 é rejeitada.
func TestOrderWithinWallet(t *testing.T) {
	q := newQueues(t)
	w := openWallet(t, "100.00")
	send(t, q, w.id, envelope("msg-first", w, "ord-80", "BET", "80.00", ""))
	send(t, q, w.id, envelope("msg-second", w, "ord-30", "BET", "30.00", ""))
	consumer(t, q, nil)

	eventually(t, 15*time.Second, "both processed", func() bool {
		return count(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND kind = 'BET'`, w.id) == 2
	})
	if n := count(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND
		((external_transaction_id = 'ord-80' AND status = 'PROCESSED') OR (external_transaction_id = 'ord-30' AND status = 'REJECTED'))`, w.id); n != 2 {
		t.Fatal("messages of the same wallet were not processed in order")
	}
}

// TestGracefulShutdown: ao parar, a mensagem em processamento termina e é
// removida; as recebidas e não iniciadas voltam para a fila na hora.
func TestGracefulShutdown(t *testing.T) {
	q := newQueues(t)
	w := openWallet(t, "100.00")
	for _, ext := range []string{"gs-1", "gs-2", "gs-3"} {
		send(t, q, w.id, envelope("msg-"+ext, w, ext, "BET", "1.00", ""))
	}
	started := make(chan struct{}, 1)
	c := consumer(t, q, handlerFunc(func(ctx context.Context, msg app.InboxMessage, in wagering.RequestInput, tr event.Trace) (app.MessageResult, error) {
		select {
		case started <- struct{}{}:
			time.Sleep(1500 * time.Millisecond)
		default:
		}
		return svc.HandleMessage(ctx, msg, in, tr)
	}))
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// As três mensagens são da mesma carteira e chegam no mesmo lote: a
	// primeira termina; as outras duas não começam.
	if processed := count(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND kind = 'BET'`, w.id); processed != 1 {
		t.Fatalf("processed = %d, want exactly the in-flight message", processed)
	}
	// As duas restantes estão visíveis de novo, sem esperar o visibility timeout.
	out, err := client.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
		QueueUrl: aws.String(q.url), MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
	})
	if err != nil || len(out.Messages) != 2 {
		t.Fatalf("released messages visible = %d, want 2 (%v)", len(out.Messages), err)
	}
}

// TestStopDeadlineCancelsInFlight: se o prazo de parada estoura, o
// processamento é cancelado, nada é confirmado e a mensagem volta à fila.
func TestStopDeadlineCancelsInFlight(t *testing.T) {
	q := newQueues(t)
	w := openWallet(t, "100.00")
	started := make(chan struct{}, 1)
	c := consumer(t, q, handlerFunc(func(ctx context.Context, msg app.InboxMessage, in wagering.RequestInput, tr event.Trace) (app.MessageResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return svc.HandleMessage(ctx, msg, in, tr)
	}))
	send(t, q, w.id, envelope("msg-stuck", w, "stuck-1", "BET", "10.00", ""))
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := c.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop error = %v, want deadline exceeded", err)
	}
	if balance(t, w) != "100.00" {
		t.Fatal("cancelled processing committed money")
	}
	if n := count(t, `SELECT count(*) FROM inbox_messages WHERE message_id = 'msg-stuck'`); n != 0 {
		t.Fatalf("cancelled processing left %d inbox rows", n)
	}
	eventually(t, 10*time.Second, "message back in the queue", func() bool { return queueDepth(t, q.url) == 1 })
}

// TestHeartbeatKeepsMessageInvisible: um processamento mais longo que o
// visibility timeout (5s contra 2s) não deixa a mensagem reaparecer para o
// outro poller no meio do tratamento.
func TestHeartbeatKeepsMessageInvisible(t *testing.T) {
	q := newQueues(t)
	var calls counter
	consumer(t, q, handlerFunc(func(ctx context.Context, msg app.InboxMessage, in wagering.RequestInput, tr event.Trace) (app.MessageResult, error) {
		calls.inc()
		time.Sleep(5 * time.Second)
		return svc.HandleMessage(ctx, msg, in, tr)
	}))
	w := openWallet(t, "100.00")
	send(t, q, w.id, envelope("msg-slow", w, "slow-1", "BET", "10.00", ""))

	eventually(t, 20*time.Second, "slow message deleted", func() bool {
		return balance(t, w) == "90.00" && queueDepth(t, q.url) == 0
	})
	if got := calls.n.Load(); got != 1 {
		t.Fatalf("handler called %d times; the message reappeared while being processed", got)
	}
}
