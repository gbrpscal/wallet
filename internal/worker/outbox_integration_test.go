//go:build integration

package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssns "github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/event"
	"github.com/gbrpscal/wallet/internal/platform/metrics"
	"github.com/gbrpscal/wallet/internal/platform/metrics/metricstest"
	"github.com/gbrpscal/wallet/internal/worker"
)

// TestEventsArePublishedAsCommitted: cada evento confirmado chega à fila
// assinante exatamente como gravado na outbox, com grupo, tipo e versão.
func TestEventsArePublishedAsCommitted(t *testing.T) {
	e := newEnv(t)
	w := e.openWallet(t, "1000.00")
	e.submit(t, w, "bet-1", "BET", "25.00", "")

	stored := map[string]string{}
	rows, err := e.db.App.Query(context.Background(), `SELECT event_id::text, payload::text FROM outbox_events`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, payload string
		_ = rows.Scan(&id, &payload)
		stored[id] = payload
	}
	rows.Close()
	if len(stored) != 4 {
		t.Fatalf("outbox has %d events, want 4 (opening and bet: processed + balance changed)", len(stored))
	}

	drain(t, e, e.relay("publisher-1", e.recorder(), 30*time.Second), 10*time.Second)
	msgs := received(t, e.queueURL, 4, 15*time.Second)
	if len(msgs) != 4 {
		t.Fatalf("received %d events, want 4", len(msgs))
	}
	for _, m := range msgs {
		var env struct {
			EventID     string `json:"eventId"`
			EventType   string `json:"eventType"`
			AggregateID string `json:"aggregateId"`
			Version     int    `json:"version"`
		}
		body := aws.ToString(m.Body)
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatal(err)
		}
		if stored[env.EventID] != body {
			t.Errorf("event %s changed in transit:\n got %s\nwant %s", env.EventID, body, stored[env.EventID])
		}
		if aws.ToString(m.MessageAttributes["eventType"].StringValue) != env.EventType ||
			aws.ToString(m.MessageAttributes["eventVersion"].StringValue) != fmt.Sprint(env.Version) ||
			m.Attributes["MessageGroupId"] != env.AggregateID {
			t.Errorf("event %s: attributes %v, group %s", env.EventID, m.MessageAttributes, m.Attributes["MessageGroupId"])
		}
	}
	if n := e.count(t, `SELECT count(*) FROM outbox_events WHERE published_at IS NOT NULL AND attempts = 1 AND claimed_by IS NULL`); n != 4 {
		t.Fatalf("published and released events = %d", n)
	}
}

// TestNothingIsPublishedBeforeCommit: eventos de uma transação ainda aberta
// são invisíveis ao publisher; só saem depois do commit.
func TestNothingIsPublishedBeforeCommit(t *testing.T) {
	e := newEnv(t)
	rec := e.recorder()
	relay := e.relay("publisher-1", rec, 30*time.Second)

	ev, err := event.NewWagerTransactionProcessed(event.Trace{CorrelationID: "c"}, time.Now(), sampleProcessed(t))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := e.db.App.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := event.Marshal(ev)
	if _, err := tx.Exec(context.Background(), `INSERT INTO outbox_events (event_id, aggregate_id, event_type, payload, occurred_at, next_attempt_at)
		VALUES ($1, $2, $3, CAST($4::text AS json), now(), now())`, ev.ID().String(), ev.AggregateID(), ev.Type(), string(payload)); err != nil {
		t.Fatal(err)
	}
	if n, err := relay.RunOnce(context.Background(), nil); err != nil || n != 0 || rec.total() != 0 {
		t.Fatalf("uncommitted event published: n=%d calls=%d err=%v", n, rec.total(), err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, err := relay.RunOnce(context.Background(), nil); err != nil || n != 1 {
		t.Fatalf("committed event not published: n=%d err=%v", n, err)
	}
}

func sampleProcessed(t *testing.T) event.WagerTransactionProcessedData {
	t.Helper()
	txID, _ := domain.NewID[domain.TransactionID]()
	walletID, _ := domain.NewID[domain.WalletID]()
	playerID, _ := domain.NewID[domain.PlayerID]()
	var d event.WagerTransactionProcessedData
	d.TransactionID, d.WalletID, d.PlayerID, d.Kind = txID, walletID, playerID, "LOSS"
	if err := json.Unmarshal([]byte(`{"amount":"0.00","currency":"BRL"}`), &d.Money); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"amount":"1.00","currency":"BRL"}`), &d.Balance); err != nil {
		t.Fatal(err)
	}
	return d
}

// walletVersions devolve, na ordem recebida, o walletVersion dos
// WalletBalanceChanged da carteira.
func walletVersions(t *testing.T, msgs []string, walletID string) []int {
	t.Helper()
	var versions []int
	for _, body := range msgs {
		var env struct {
			EventType string `json:"eventType"`
			Data      struct {
				WalletID      string `json:"walletId"`
				WalletVersion int    `json:"walletVersion"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(body), &env)
		if env.EventType == "WalletBalanceChanged" && env.Data.WalletID == walletID {
			versions = append(versions, env.Data.WalletVersion)
		}
	}
	return versions
}

// TestOrderPerAggregate: os WalletBalanceChanged de uma carteira saem na
// ordem de walletVersion, mesmo quando um deles falha e é republicado depois.
func TestOrderPerAggregate(t *testing.T) {
	e := newEnv(t)
	w := e.openWallet(t, "100.00")
	for i := 0; i < 5; i++ {
		e.submit(t, w, fmt.Sprintf("bet-%d", i), "BET", "1.00", "")
	}
	// O evento de saldo da versão 3 falha duas vezes antes de sair.
	var v3 string
	_ = e.db.App.QueryRow(context.Background(), `SELECT event_id::text FROM outbox_events
		WHERE event_type = 'WalletBalanceChanged' AND (payload::jsonb->'data'->>'walletVersion')::int = 3`).Scan(&v3)
	rec := e.recorder()
	rec.failIDs[domain.MustParseID[domain.EventID](v3)] = 2

	drain(t, e, e.relay("publisher-1", rec, 30*time.Second), 20*time.Second)
	msgs := received(t, e.queueURL, 12, 20*time.Second)
	bodies := make([]string, len(msgs))
	for i, m := range msgs {
		bodies[i] = aws.ToString(m.Body)
	}
	if got := fmt.Sprint(walletVersions(t, bodies, w.id)); got != "[1 2 3 4 5 6]" {
		t.Fatalf("wallet balance events arrived in order %s", got)
	}
	if n := e.count(t, `SELECT attempts FROM outbox_events WHERE event_id = $1`, v3); n != 3 {
		t.Fatalf("failing event attempts = %d, want 3", n)
	}
}

// TestBrokerUnavailableIsRetriedWithBackoff: falha no broker registra a
// tentativa e o erro, agenda nova tentativa e nada se perde.
func TestBrokerUnavailableIsRetriedWithBackoff(t *testing.T) {
	e := newEnv(t)
	e.openWallet(t, "10.00")
	rec := e.recorder()
	rec.failFirst = 2
	reg := prometheus.NewRegistry()
	relay := e.relayWith("publisher-1", rec, 30*time.Second, metrics.New(reg))

	if _, err := relay.RunOnce(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL AND attempts = 1
		AND last_error LIKE '%broker unavailable%' AND next_attempt_at > now() AND claimed_by IS NULL`); n != 2 {
		t.Fatalf("failed events with recorded attempt and backoff = %d, want 2", n)
	}
	// Antes do backoff vencer, nada é reservado de novo.
	if n, _ := relay.RunOnce(context.Background(), nil); n != 0 {
		t.Fatalf("event retried before its backoff: %d", n)
	}
	drain(t, e, relay, 10*time.Second)
	if got := received(t, e.queueURL, 2, 10*time.Second); len(got) != 2 {
		t.Fatalf("received %d events after recovery, want 2", len(got))
	}
	retries := map[string]string{"component": "outbox_publisher", "reason": "broker_error"}
	if got := metricstest.Value(t, reg, "wallet_retries_total", retries); got != 2 {
		t.Errorf("publisher retries = %v, want 2", got)
	}
	if got := metricstest.Value(t, reg, "wallet_outbox_publish_delay_seconds", map[string]string{}); got != 2 {
		t.Errorf("publish delay samples = %v, want 2 (one per published event)", got)
	}
}

// TestTwoPublishersCompete: dois publishers concorrentes sobre a mesma outbox
// publicam cada evento uma única vez.
func TestTwoPublishersCompete(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		w := e.openWallet(t, "100.00")
		for j := 0; j < 4; j++ {
			e.submit(t, w, fmt.Sprintf("w%d-bet-%d", i, j), "BET", "1.00", "")
		}
	}
	total := e.count(t, `SELECT count(*) FROM outbox_events`)
	rec := e.recorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for _, owner := range []string{"publisher-a", "publisher-b"} {
		wg.Add(1)
		go func(r *app.OutboxRelay) {
			defer wg.Done()
			for ctx.Err() == nil {
				_, _ = r.RunOnce(ctx, nil)
			}
		}(e.relay(owner, rec, 30*time.Second))
	}
	deadline := time.Now().Add(30 * time.Second)
	for e.pending(t) > 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	wg.Wait()

	if rec.total() != total {
		t.Fatalf("broker calls = %d for %d events: some event was published twice", rec.total(), total)
	}
	if got := received(t, e.queueURL, total, 20*time.Second); len(got) != total {
		t.Fatalf("received %d of %d events", len(got), total)
	}
	if owners := e.count(t, `SELECT count(DISTINCT claimed_by) FROM outbox_events`); owners != 0 {
		t.Fatalf("claims left behind: %d", owners)
	}
}

// TestAbandonedClaimIsRecovered: um publisher cai depois de reservar; os
// eventos ficam presos até o lease vencer e então outro publisher os publica.
func TestAbandonedClaimIsRecovered(t *testing.T) {
	e := newEnv(t)
	e.openWallet(t, "10.00")
	crashed := e.relay("publisher-a", e.recorder(), time.Second)
	crashed.SetAfterClaim(func([]app.OutboxEvent) error { return errors.New("process killed after claim") })
	if _, err := crashed.RunOnce(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM outbox_events WHERE claimed_by = 'publisher-a'`); n != 2 {
		t.Fatalf("claimed by the crashed publisher = %d", n)
	}

	rescuer := e.relay("publisher-b", e.recorder(), 30*time.Second)
	if n, _ := rescuer.RunOnce(context.Background(), nil); n != 0 {
		t.Fatalf("events taken over before the lease expired: %d", n)
	}
	time.Sleep(1200 * time.Millisecond)
	if n, _ := rescuer.RunOnce(context.Background(), nil); n != 2 {
		t.Fatalf("events recovered after the lease = %d, want 2", n)
	}
	if got := received(t, e.queueURL, 2, 10*time.Second); len(got) != 2 {
		t.Fatalf("received %d events", len(got))
	}
}

// TestRepublishAfterCrashKeepsEventID: um publisher cai depois de publicar e
// antes de marcar; outro republica com o mesmo eventId e os mesmos bytes, e o
// broker entrega uma única cópia.
func TestRepublishAfterCrashKeepsEventID(t *testing.T) {
	e := newEnv(t)
	e.openWallet(t, "10.00")
	rec := e.recorder()
	crashed := e.relay("publisher-a", rec, time.Second)
	crashed.SetAfterPublish(func(app.OutboxEvent) error { return errors.New("process killed after publish") })
	if _, err := crashed.RunOnce(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if e.pending(t) != 2 {
		t.Fatal("events marked as published despite the crash")
	}

	time.Sleep(1200 * time.Millisecond)
	if n, _ := e.relay("publisher-b", rec, 30*time.Second).RunOnce(context.Background(), nil); n != 2 {
		t.Fatalf("republished = %d, want 2", n)
	}
	rows, _ := e.db.App.Query(context.Background(), `SELECT event_id::text FROM outbox_events`)
	defer rows.Close()
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		calls := rec.callsFor(domain.MustParseID[domain.EventID](id))
		if len(calls) != 2 || string(calls[0].Payload) != string(calls[1].Payload) {
			t.Fatalf("event %s: %d broker calls; republication must reuse the same eventId and bytes", id, len(calls))
		}
	}
	if got := received(t, e.queueURL, 4, 5*time.Second); len(got) != 2 {
		t.Fatalf("subscriber received %d copies, want 2 (broker deduplicates by eventId)", len(got))
	}
}

// TestLeaseIsRenewedDuringSlowPublish: um lote mais lento que o lease não é
// assumido por outro publisher enquanto o dono está vivo.
func TestLeaseIsRenewedDuringSlowPublish(t *testing.T) {
	e := newEnv(t)
	e.openWallet(t, "10.00")
	slow := e.recorder()
	slow.delay = 2500 * time.Millisecond
	fast := e.recorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = e.relay("publisher-slow", slow, time.Second).RunOnce(context.Background(), nil)
	}()
	// O outro publisher só começa depois que o lento reservou o lote.
	for slow.total() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, _ = e.relay("publisher-fast", fast, time.Second).RunOnce(context.Background(), nil)
		time.Sleep(200 * time.Millisecond)
	}
	<-done
	if fast.total() != 0 || slow.total() != 2 {
		t.Fatalf("slow owner published %d, another publisher took %d while the owner was renewing its lease", slow.total(), fast.total())
	}
	if e.pending(t) != 0 {
		t.Fatalf("pending after slow publish = %d", e.pending(t))
	}
}

// TestStopReleasesUnstartedClaims: ao encerrar, eventos reservados e não
// iniciados são liberados na hora; o que está em andamento termina.
func TestStopReleasesUnstartedClaims(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 3; i++ {
		e.openWallet(t, "10.00")
	}
	slow := e.recorder()
	slow.delay = time.Second
	relay := app.NewOutboxRelay(e.uow, slow, app.SystemClock{}, app.OutboxRelayConfig{
		Owner: "publisher-stopping", BatchSize: 50, Lease: 30 * time.Second, Concurrency: 1,
		RetryBase: 100 * time.Millisecond, RetryMax: 400 * time.Millisecond,
	}, app.NopMetrics{}, quiet)
	stopping := make(chan struct{})
	go func() {
		for slow.total() == 0 {
			time.Sleep(10 * time.Millisecond)
		}
		close(stopping)
	}()
	n, err := relay.RunOnce(context.Background(), stopping)
	if err != nil || n != 1 {
		t.Fatalf("published while stopping = %d (%v), want only the in-flight event", n, err)
	}
	if left := e.count(t, `SELECT count(*) FROM outbox_events WHERE claimed_by IS NOT NULL`); left != 0 {
		t.Fatalf("claims kept after stop = %d; they should be released at once", left)
	}
	if e.pending(t) != 5 {
		t.Fatalf("pending = %d, want 5 of 6", e.pending(t))
	}
}

// TestSubscriptionDeadLetters: se o SNS não consegue entregar à fila
// assinante, o evento vai para a DLQ da assinatura, sem se perder.
func TestSubscriptionDeadLetters(t *testing.T) {
	e := newEnv(t)
	s := suffix()
	dlqURL := createQueue(t, "events-"+s+"-dlq.fifo")
	goneURL := createQueue(t, "events-"+s+"-gone.fifo")
	sub := subscribe(t, e.topicARN, goneURL)
	if _, err := snsClient.SetSubscriptionAttributes(context.Background(), &awssns.SetSubscriptionAttributesInput{
		SubscriptionArn: aws.String(sub), AttributeName: aws.String("RedrivePolicy"),
		AttributeValue: aws.String(`{"deadLetterTargetArn":"` + queueArn(t, dlqURL) + `"}`),
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = sqsClient.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: aws.String(goneURL)})

	e.openWallet(t, "10.00")
	drain(t, e, e.relay("publisher-1", e.recorder(), 30*time.Second), 10*time.Second)
	if got := received(t, dlqURL, 2, 15*time.Second); len(got) != 2 {
		t.Fatalf("dead-lettered events = %d, want 2", len(got))
	}
	if got := received(t, e.queueURL, 2, 10*time.Second); len(got) != 2 {
		t.Fatalf("healthy subscription received %d events, want 2", len(got))
	}
}

// TestOutboxLoopLifecycle: o loop do publisher sobe, publica e para.
func TestOutboxLoopLifecycle(t *testing.T) {
	e := newEnv(t)
	e.openWallet(t, "10.00")
	loop := worker.NewOutboxLoop(e.relay("publisher-loop", e.recorder(), 30*time.Second), 100*time.Millisecond, quiet)
	if err := loop.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for e.pending(t) > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := loop.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if e.pending(t) != 0 {
		t.Fatalf("loop left %d pending events", e.pending(t))
	}
}

// TestGaugeLoopReportsBacklogAndDeadLetters: o refresher lê a outbox e a
// profundidade das DLQs; uma fila inexistente não derruba as demais leituras.
func TestGaugeLoopReportsBacklogAndDeadLetters(t *testing.T) {
	e := newEnv(t)
	e.openWallet(t, "10.00") // 2 eventos pendentes
	dlqName := "gauge-" + suffix() + "-dlq.fifo"
	dlqURL := createQueue(t, dlqName)
	if _, err := sqsClient.SendMessage(context.Background(), &sqs.SendMessageInput{QueueUrl: aws.String(dlqURL),
		MessageBody: aws.String("{}"), MessageGroupId: aws.String("g"), MessageDeduplicationId: aws.String("d")}); err != nil {
		t.Fatal(err)
	}

	reg := prometheus.NewRegistry()
	var logs bytes.Buffer
	var logMu sync.Mutex
	logger := slog.New(slog.NewJSONHandler(lockedWriter{&logMu, &logs}, nil))
	loop := worker.NewGaugeLoop(e.uow, sqsClient, []string{"missing-" + suffix() + ".fifo", dlqName},
		metrics.New(reg), app.SystemClock{}, 50*time.Millisecond, logger)
	if err := loop.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = loop.Stop(ctx)
	}()
	gauge := func(name string, labels map[string]string) float64 {
		return metricstest.Value(t, reg, name, labels)
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timeout waiting for %s", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitFor("backlog and dead-letter depth", func() bool {
		return gauge("wallet_outbox_pending_events", map[string]string{}) == 2 &&
			gauge("wallet_outbox_oldest_pending_age_seconds", map[string]string{}) > 0 &&
			gauge("wallet_dead_letter_queue_messages", map[string]string{"queue": dlqName}) == 1
	})

	drain(t, e, e.relay("publisher-gauge", e.recorder(), 30*time.Second), 10*time.Second)
	waitFor("empty backlog", func() bool {
		return gauge("wallet_outbox_pending_events", map[string]string{}) == 0 &&
			gauge("wallet_outbox_oldest_pending_age_seconds", map[string]string{}) == 0
	})

	logMu.Lock()
	defer logMu.Unlock()
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if n := strings.Count(line, `"component"`); n != 1 {
			t.Errorf("component appears %d times: %s", n, line)
		}
	}
	if !strings.Contains(logs.String(), "reading dead-letter queue depth failed") {
		t.Error("missing queue was not logged")
	}
}

// lockedWriter serializa escritas no buffer de log do teste.
type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
