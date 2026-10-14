package sqs

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/event"
	"github.com/gbrpscal/wallet/internal/domain/wagering"
)

// MessageHandler é o caso de uso que trata cada mensagem.
type MessageHandler interface {
	HandleMessage(ctx context.Context, msg app.InboxMessage, in wagering.RequestInput, trace event.Trace) (app.MessageResult, error)
}

// ConsumerConfig configura o consumidor (D9).
type ConsumerConfig struct {
	// Name identifica o consumidor na inbox (consumerName).
	Name      string
	QueueName string
	DLQName   string
	Pollers   int
	WaitTime  time.Duration
	// Visibility é o visibility timeout de cada recebimento; enquanto a
	// mensagem está em processamento, ele é renovado (heartbeat).
	Visibility time.Duration
	// RetryBase e RetryMax definem o backoff de falhas transitórias:
	// RetryBase·2^(recebimentos−1), com teto RetryMax.
	RetryBase time.Duration
	RetryMax  time.Duration
}

// Consumer lê wager-transactions.fifo. Uma mensagem só é removida depois do
// commit do seu tratamento; falhas transitórias voltam com backoff; erros
// permanentes vão para a DLQ; após o maxReceiveCount da fila, o redrive do
// SQS move a mensagem para a DLQ.
type Consumer struct {
	client  *sqs.Client
	handler MessageHandler
	cfg     ConsumerConfig
	log     *slog.Logger

	queueURL string
	dlqURL   string

	pollCtx    context.Context
	stopPoll   context.CancelFunc
	workCtx    context.Context
	cancelWork context.CancelFunc
	wg         sync.WaitGroup

	// afterHandle roda depois do commit e antes da remoção; os testes o usam
	// para simular uma queda nesse intervalo.
	afterHandle func(env Envelope) error
}

// NewConsumer cria o consumidor; Start inicia a leitura.
func NewConsumer(client *sqs.Client, handler MessageHandler, cfg ConsumerConfig, log *slog.Logger) *Consumer {
	return &Consumer{client: client, handler: handler, cfg: cfg, log: log.With("component", "sqs-consumer", "consumer", cfg.Name)}
}

// SetAfterHandle instala um gancho entre o commit e a remoção da mensagem.
// Uso exclusivo de testes de falha.
func (c *Consumer) SetAfterHandle(fn func(env Envelope) error) { c.afterHandle = fn }

// Start resolve as URLs das filas e inicia os pollers.
func (c *Consumer) Start(ctx context.Context) error {
	var err error
	if c.queueURL, err = c.resolve(ctx, c.cfg.QueueName); err != nil {
		return err
	}
	if c.dlqURL, err = c.resolve(ctx, c.cfg.DLQName); err != nil {
		return err
	}
	c.pollCtx, c.stopPoll = context.WithCancel(context.Background())
	c.workCtx, c.cancelWork = context.WithCancel(context.Background())
	for i := 0; i < c.cfg.Pollers; i++ {
		c.wg.Add(1)
		go c.poll()
	}
	c.log.Info("sqs consumer started", "queue", c.cfg.QueueName, "pollers", c.cfg.Pollers)
	return nil
}

func (c *Consumer) resolve(ctx context.Context, name string) (string, error) {
	out, err := c.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.QueueUrl), nil
}

// Stop para de buscar mensagens imediatamente. As que estão em processamento
// terminam até o prazo de ctx; as recebidas e ainda não iniciadas voltam para
// a fila na hora (visibilidade 0). Estourado o prazo, o processamento é
// cancelado: a transação SQL é desfeita e a mensagem volta pelo timeout.
func (c *Consumer) Stop(ctx context.Context) error {
	if c.stopPoll == nil {
		return nil
	}
	c.stopPoll()
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
		c.log.Info("sqs consumer stopped")
		return nil
	case <-ctx.Done():
		c.cancelWork()
		<-done
		c.log.Warn("sqs consumer stopped after cancelling in-flight work")
		return ctx.Err()
	}
}

func (c *Consumer) poll() {
	defer c.wg.Done()
	for c.pollCtx.Err() == nil {
		out, err := c.client.ReceiveMessage(c.pollCtx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(c.queueURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     int32(c.cfg.WaitTime / time.Second),
			VisibilityTimeout:   int32(c.cfg.Visibility / time.Second),
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{
				types.MessageSystemAttributeNameApproximateReceiveCount,
				types.MessageSystemAttributeNameMessageGroupId,
			},
		})
		if err != nil {
			if c.pollCtx.Err() != nil {
				return
			}
			c.log.Warn("receive failed", "error", err)
			select {
			case <-c.pollCtx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		c.processBatch(out.Messages)
	}
}

// processBatch trata grupos (carteiras) em paralelo e as mensagens de um
// mesmo grupo em ordem, preservando a ordem FIFO por carteira.
func (c *Consumer) processBatch(msgs []types.Message) {
	var order []string
	groups := map[string][]types.Message{}
	for _, m := range msgs {
		g := m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)]
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], m)
	}
	var wg sync.WaitGroup
	for _, g := range order {
		wg.Add(1)
		go func(group []types.Message) {
			defer wg.Done()
			c.processGroup(group)
		}(groups[g])
	}
	wg.Wait()
}

func (c *Consumer) processGroup(group []types.Message) {
	for i, m := range group {
		if c.pollCtx.Err() != nil {
			// Encerrando: devolve o que ainda não começou, para outra instância.
			c.changeVisibility(group[i:], 0)
			return
		}
		if delay, retry := c.process(m); retry {
			// As seguintes do mesmo grupo voltam junto com a que falhou. A ordem já
			// é garantida pelo SQS FIFO (nenhuma mensagem do grupo é entregue
			// enquanto outra está em voo); reagendá-las aqui só evita que esperem
			// o visibility timeout inteiro em vez do backoff.
			c.changeVisibility(group[i:], delay)
			return
		}
	}
}

// process trata uma mensagem. retry=true significa falha transitória: a
// mensagem não foi removida e deve voltar depois de delay.
func (c *Consumer) process(m types.Message) (delay time.Duration, retry bool) {
	body := aws.ToString(m.Body)
	receives, _ := strconv.Atoi(m.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)])
	log := c.log.With("sqsMessageId", aws.ToString(m.MessageId), "receiveCount", receives)

	stopHeartbeat := c.heartbeat(m)
	defer stopHeartbeat()

	env, err := ParseEnvelope(body)
	if err != nil {
		return c.deadLetter(log, m, err)
	}
	log = log.With("messageId", env.MessageID, "walletId", env.Data.WalletID, "providerId", env.Data.ProviderID)
	trace := event.Trace{CorrelationID: env.MessageID, CausationID: env.MessageID}
	inbox := app.InboxMessage{ConsumerName: c.cfg.Name, MessageID: env.MessageID, PayloadHash: BodyHash(body)}

	res, err := c.handler.HandleMessage(c.workCtx, inbox, env.RequestInput(), trace)
	if err != nil {
		if permanent(err) {
			return c.deadLetter(log, m, err)
		}
		d := c.backoff(receives)
		log.Warn("transient failure, message will be retried", "retryIn", d.String(), "error", err)
		return d, true
	}
	if c.afterHandle != nil {
		if err := c.afterHandle(env); err != nil {
			log.Warn("interrupted after commit, before delete", "error", err)
			return c.cfg.Visibility, true
		}
	}
	c.delete(log, m)
	if res.Duplicate {
		log.Info("duplicate message acknowledged without reprocessing")
		return 0, false
	}
	t := res.Operation.Transaction
	log.Info("message processed", "transactionId", t.ID().String(), "status", string(t.Status()),
		"failureCode", string(t.FailureCode()), "idempotentReplay", res.Operation.Replay)
	return 0, false
}

// permanent: erros que nenhuma nova tentativa resolve (entrada inválida,
// recurso inexistente, conflito, invariante violada).
func permanent(err error) bool {
	kind, ok := domain.KindOf(err)
	return ok && kind != domain.KindRejection
}

func (c *Consumer) backoff(receives int) time.Duration {
	d := c.cfg.RetryBase
	for i := 1; i < receives && d < c.cfg.RetryMax; i++ {
		d *= 2
	}
	if d > c.cfg.RetryMax {
		d = c.cfg.RetryMax
	}
	return d
}

// deadLetter envia a mensagem para a DLQ com o motivo e a remove da fila. Se
// o envio falhar, ela não é removida e volta como falha transitória.
func (c *Consumer) deadLetter(log *slog.Logger, m types.Message, cause error) (time.Duration, bool) {
	code := string(domain.CodeInvalidRequest)
	if dc, ok := domain.CodeOf(cause); ok {
		code = string(dc)
	}
	detail := cause.Error()
	if len(detail) > 256 {
		detail = detail[:256]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(c.dlqURL),
		MessageBody:            m.Body,
		MessageGroupId:         aws.String(groupOf(m)),
		MessageDeduplicationId: m.MessageId,
		MessageAttributes: map[string]types.MessageAttributeValue{
			"ErrorCode":         {DataType: aws.String("String"), StringValue: aws.String(code)},
			"ErrorDetail":       {DataType: aws.String("String"), StringValue: aws.String(detail)},
			"SourceQueue":       {DataType: aws.String("String"), StringValue: aws.String(c.cfg.QueueName)},
			"OriginalMessageId": {DataType: aws.String("String"), StringValue: m.MessageId},
		},
	})
	if err != nil {
		log.Error("dead-letter send failed, message will be retried", "error", err)
		return c.backoff(1), true
	}
	log.Warn("message sent to dead-letter queue", "errorCode", code, "error", cause)
	c.delete(log, m)
	return 0, false
}

func groupOf(m types.Message) string {
	if g := m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)]; g != "" {
		return g
	}
	return "unknown"
}

// delete remove a mensagem; se falhar, ela reaparece e a inbox impede o
// reprocessamento.
func (c *Consumer) delete(log *slog.Logger, m types.Message) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl: aws.String(c.queueURL), ReceiptHandle: m.ReceiptHandle,
	}); err != nil {
		log.Warn("delete failed; the message will be redelivered and deduplicated by the inbox", "error", err)
	}
}

func (c *Consumer) changeVisibility(msgs []types.Message, d time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, m := range msgs {
		if _, err := c.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
			QueueUrl: aws.String(c.queueURL), ReceiptHandle: m.ReceiptHandle,
			VisibilityTimeout: int32(d / time.Second),
		}); err != nil {
			c.log.Warn("change visibility failed", "sqsMessageId", aws.ToString(m.MessageId), "error", err)
		}
	}
}

// heartbeat renova a visibilidade enquanto a mensagem é processada, para que
// ela não reapareça para outra instância no meio do tratamento.
func (c *Consumer) heartbeat(m types.Message) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	interval := c.cfg.Visibility / 3
	if interval <= 0 {
		interval = time.Second
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, err := c.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
					QueueUrl: aws.String(c.queueURL), ReceiptHandle: m.ReceiptHandle,
					VisibilityTimeout: int32(c.cfg.Visibility / time.Second),
				})
				if err != nil && !errors.Is(err, context.Canceled) {
					c.log.Warn("visibility heartbeat failed", "sqsMessageId", aws.ToString(m.MessageId), "error", err)
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}
