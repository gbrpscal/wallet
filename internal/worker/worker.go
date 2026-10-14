// Package worker roda os loops em segundo plano (publisher da outbox e worker
// de referências pendentes) ligados ao ciclo de vida do Fx. Cada um pode ser
// desligado por configuração; com várias instâncias, todos rodam em paralelo
// e a coordenação fica no banco (reservas com lease e locks por carteira).
package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"

	"github.com/gbrpscal/wallet/internal/adapters/sns"
	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/platform/config"
)

// Module liga os workers ao ciclo de vida. Eles param antes do pool do
// PostgreSQL (ordem inversa de inicialização).
var Module = fx.Module("worker",
	fx.Invoke(StartOutboxWorker, StartReferenceWorker, StartGaugeRefresher),
)

// OwnerID identifica esta instância nas reservas: hostname mais um sufixo
// aleatório, único mesmo com várias cópias no mesmo host.
func OwnerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "wallet"
	}
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return host + "-" + hex.EncodeToString(b)
}

// Loop executa step repetidamente até Stop. step recebe o contexto de
// trabalho (cancelado só quando o prazo de parada estoura) e um canal que é
// fechado quando o encerramento começa.
type Loop struct {
	name       string
	log        *slog.Logger
	step       func(ctx context.Context, stopping <-chan struct{}) (busy bool)
	idle       time.Duration
	stopping   chan struct{}
	workCtx    context.Context
	cancelWork context.CancelFunc
	done       chan struct{}
	once       sync.Once
}

func newLoop(name string, log *slog.Logger, idle time.Duration, step func(context.Context, <-chan struct{}) bool) *Loop {
	return &Loop{name: name, log: log.With("component", name), idle: idle, step: step}
}

// Start inicia o loop.
func (l *Loop) Start(context.Context) error {
	l.stopping = make(chan struct{})
	l.done = make(chan struct{})
	l.workCtx, l.cancelWork = context.WithCancel(context.Background())
	go func() {
		defer close(l.done)
		for {
			select {
			case <-l.stopping:
				return
			default:
			}
			if l.step(l.workCtx, l.stopping) {
				continue // havia trabalho: consulta de novo na hora
			}
			select {
			case <-l.stopping:
				return
			case <-time.After(l.idle):
			}
		}
	}()
	l.log.Info("worker started")
	return nil
}

// Stop para de iniciar trabalho novo e espera o que está em andamento até o
// prazo de ctx; estourado o prazo, cancela o trabalho em andamento.
func (l *Loop) Stop(ctx context.Context) error {
	if l.stopping == nil {
		return nil
	}
	l.once.Do(func() { close(l.stopping) })
	select {
	case <-l.done:
		l.log.Info("worker stopped")
		return nil
	case <-ctx.Done():
		l.cancelWork()
		<-l.done
		l.log.Warn("worker stopped after cancelling in-flight work")
		return ctx.Err()
	}
}

// NewOutboxLoop cria o loop do publisher sobre um relay.
func NewOutboxLoop(relay *app.OutboxRelay, idle time.Duration, log *slog.Logger) *Loop {
	return newLoop("outbox-publisher", log, idle, func(ctx context.Context, stopping <-chan struct{}) bool {
		n, err := relay.RunOnce(ctx, stopping)
		if err != nil {
			log.Warn("outbox claim failed", "component", "outbox-publisher", "error", err)
			return false
		}
		return n > 0
	})
}

// StartOutboxWorker liga o publisher da outbox, se habilitado.
func StartOutboxWorker(lc fx.Lifecycle, cfg config.Config, uow app.UnitOfWork, clock app.Clock, metrics app.Metrics, log *slog.Logger) error {
	if !cfg.OutboxPublisherEnabled {
		log.Info("outbox publisher disabled")
		return nil
	}
	client, err := sns.NewClient(cfg)
	if err != nil {
		return err
	}
	relay := app.NewOutboxRelay(uow, sns.NewPublisher(client, cfg.SNSEventsTopicARN), clock, app.OutboxRelayConfig{
		Owner: OwnerID(), BatchSize: cfg.OutboxBatchSize, Lease: cfg.OutboxLease,
		Concurrency: cfg.OutboxPublishConcurrency, RetryBase: cfg.OutboxRetryBase, RetryMax: cfg.OutboxRetryMax,
	}, metrics, log)
	l := NewOutboxLoop(relay, cfg.OutboxIdleInterval, log)
	lc.Append(fx.Hook{OnStart: l.Start, OnStop: l.Stop})
	return nil
}

// ReferenceResolver é o que o worker de referências usa do caso de uso.
type ReferenceResolver interface {
	DuePendingReferences(ctx context.Context, limit int) ([]app.PendingReference, error)
	ResolvePendingReference(ctx context.Context, id domain.TransactionID) (app.ResolveStatus, error)
}

// NewReferenceLoop cria o loop do worker de referências: a cada ciclo busca
// até batch pendências vencidas; carteiras diferentes em paralelo (até
// concurrency), as da mesma carteira em sequência. Um erro numa pendência é
// logado e não conta como tentativa: ela volta no próximo ciclo.
func NewReferenceLoop(r ReferenceResolver, interval time.Duration, batch, concurrency int, log *slog.Logger) *Loop {
	base := log
	log = log.With("component", "reference-worker")
	return newLoop("reference-worker", base, interval, func(ctx context.Context, stopping <-chan struct{}) bool {
		due, err := r.DuePendingReferences(ctx, batch)
		if err != nil {
			log.Warn("listing due pending references failed", "error", err)
			return false
		}
		var order []domain.WalletID
		byWallet := map[domain.WalletID][]domain.TransactionID{}
		for _, p := range due {
			if _, ok := byWallet[p.WalletID]; !ok {
				order = append(order, p.WalletID)
			}
			byWallet[p.WalletID] = append(byWallet[p.WalletID], p.TransactionID)
		}
		sem := make(chan struct{}, max(1, concurrency))
		var wg sync.WaitGroup
		for _, w := range order {
			select {
			case <-stopping:
				wg.Wait()
				return false
			case sem <- struct{}{}:
			}
			wg.Add(1)
			go func(w domain.WalletID, ids []domain.TransactionID) {
				defer wg.Done()
				defer func() { <-sem }()
				for _, id := range ids {
					select {
					case <-stopping:
						return
					default:
					}
					st, err := r.ResolvePendingReference(ctx, id)
					if err != nil {
						log.Warn("resolving pending reference failed; it will be retried",
							"transactionId", id.String(), "walletId", w.String(), "error", err)
						continue
					}
					log.Debug("pending reference attempt", "transactionId", id.String(), "walletId", w.String(), "status", string(st))
				}
			}(w, byWallet[w])
		}
		wg.Wait()
		return false // intervalo fixo entre ciclos
	})
}

// StartReferenceWorker liga o worker de referências pendentes, se habilitado.
func StartReferenceWorker(lc fx.Lifecycle, cfg config.Config, svc *app.Service, log *slog.Logger) {
	if !cfg.ReferenceWorkerEnabled {
		log.Info("reference worker disabled")
		return
	}
	l := NewReferenceLoop(svc, cfg.ReferenceWorkerInterval, cfg.ReferenceWorkerBatch, cfg.ReferenceWorkerConcurrency, log)
	lc.Append(fx.Hook{OnStart: l.Start, OnStop: l.Stop})
}

// NewGaugeLoop atualiza, a cada interval, os gauges globais: eventos
// pendentes na outbox (e a idade do mais antigo) e a profundidade das DLQs.
// Cada instância publica o mesmo valor global; o coletor agrega por instância.
func NewGaugeLoop(uow app.UnitOfWork, client *awssqs.Client, dlqs []string, metrics app.Metrics, clock app.Clock, interval time.Duration, log *slog.Logger) *Loop {
	base := log
	log = log.With("component", "gauge-refresher")
	return newLoop("gauge-refresher", base, interval, func(ctx context.Context, _ <-chan struct{}) bool {
		var pending int
		var oldest time.Time
		err := uow.View(ctx, func(ctx context.Context, tx app.Tx) error {
			var err error
			pending, oldest, err = tx.Outbox().Backlog(ctx)
			return err
		})
		if err != nil {
			log.Warn("reading outbox backlog failed", "error", err)
		} else {
			var age time.Duration
			if !oldest.IsZero() {
				age = clock.Now().Sub(oldest)
			}
			metrics.OutboxBacklog(pending, age)
		}
		for _, name := range dlqs {
			n, err := queueDepth(ctx, client, name)
			if err != nil {
				log.Warn("reading dead-letter queue depth failed", "queue", name, "error", err)
				continue
			}
			metrics.DeadLetterDepth(name, n)
		}
		return false
	})
}

// queueDepth soma as mensagens visíveis e em voo da fila.
func queueDepth(ctx context.Context, client *awssqs.Client, name string) (int, error) {
	url, err := client.GetQueueUrl(ctx, &awssqs.GetQueueUrlInput{QueueName: aws.String(name)})
	if err != nil {
		return 0, err
	}
	out, err := client.GetQueueAttributes(ctx, &awssqs.GetQueueAttributesInput{
		QueueUrl: url.QueueUrl,
		AttributeNames: []sqstypes.QueueAttributeName{
			sqstypes.QueueAttributeNameApproximateNumberOfMessages,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	if err != nil {
		return 0, err
	}
	visible, _ := strconv.Atoi(out.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessages)])
	inflight, _ := strconv.Atoi(out.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible)])
	return visible + inflight, nil
}

// StartGaugeRefresher liga a atualização dos gauges, se as métricas estiverem
// habilitadas.
func StartGaugeRefresher(lc fx.Lifecycle, cfg config.Config, uow app.UnitOfWork, client *awssqs.Client, metrics app.Metrics, clock app.Clock, log *slog.Logger) {
	if !cfg.MetricsEnabled {
		return
	}
	l := NewGaugeLoop(uow, client, []string{cfg.WagerDLQName, cfg.EventsDLQName}, metrics, clock, cfg.MetricsRefreshInterval, log)
	lc.Append(fx.Hook{OnStart: l.Start, OnStop: l.Stop})
}
