package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/gbrpscal/wallet/internal/domain"
)

// OutboxRelayConfig configura a publicação dos eventos da outbox (D10).
type OutboxRelayConfig struct {
	// Owner identifica o publisher nas reservas.
	Owner       string
	BatchSize   int
	Lease       time.Duration
	Concurrency int
	// RetryBase e RetryMax: espera de RetryBase·2^(tentativas−1), com teto e
	// jitter de ±20%, sem limite de tentativas.
	RetryBase time.Duration
	RetryMax  time.Duration
}

// OutboxRelay publica, depois do commit, os eventos gravados na outbox. Vários
// relays (em várias instâncias) disputam a outbox por reserva com lease; a
// entrega é at-least-once, sempre com o mesmo eventId.
type OutboxRelay struct {
	uow       UnitOfWork
	publisher EventPublisher
	clock     Clock
	cfg       OutboxRelayConfig
	log       *slog.Logger

	afterClaim   func([]OutboxEvent) error
	afterPublish func(OutboxEvent) error
}

// NewOutboxRelay cria o relay.
func NewOutboxRelay(uow UnitOfWork, publisher EventPublisher, clock Clock, cfg OutboxRelayConfig, log *slog.Logger) *OutboxRelay {
	return &OutboxRelay{uow: uow, publisher: publisher, clock: clock, cfg: cfg,
		log: log.With("component", "outbox-relay", "owner", cfg.Owner)}
}

// SetAfterClaim simula uma queda depois da reserva e antes da publicação: se
// o gancho devolver erro, o lote fica reservado até o lease vencer. Uso
// exclusivo de testes de falha.
func (r *OutboxRelay) SetAfterClaim(fn func([]OutboxEvent) error) { r.afterClaim = fn }

// SetAfterPublish simula uma queda depois da publicação e antes da marcação:
// se o gancho devolver erro, o evento continua reservado e será republicado
// com o mesmo eventId. Uso exclusivo de testes de falha.
func (r *OutboxRelay) SetAfterPublish(fn func(OutboxEvent) error) { r.afterPublish = fn }

// RunOnce reserva um lote e o publica. stopping sinaliza encerramento: os
// eventos ainda não iniciados têm a reserva liberada na hora; os em andamento
// terminam com ctx. Devolve quantos eventos foram publicados.
func (r *OutboxRelay) RunOnce(ctx context.Context, stopping <-chan struct{}) (int, error) {
	now := r.clock.Now()
	var batch []OutboxEvent
	err := r.uow.Do(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		batch, err = tx.Outbox().Claim(ctx, r.cfg.Owner, now, now.Add(r.cfg.Lease), r.cfg.BatchSize)
		return err
	})
	if err != nil || len(batch) == 0 {
		return 0, err
	}
	if r.afterClaim != nil {
		if err := r.afterClaim(batch); err != nil {
			r.log.Warn("interrupted after claim", "events", len(batch), "error", err)
			return 0, nil
		}
	}

	pending := newClaimSet(batch)
	stopRenew := r.renewWhile(pending)
	defer stopRenew()

	// Um evento por agregado no lote (Claim garante): agregados diferentes em
	// paralelo, até Concurrency publicações simultâneas.
	sem := make(chan struct{}, max(1, r.cfg.Concurrency))
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		published int
	)
	for _, e := range batch {
		select {
		case <-stopping:
			r.release(pending.remaining())
			wg.Wait()
			return published, nil
		case sem <- struct{}{}:
		}
		if !pending.start(e.ID) {
			<-sem
			continue
		}
		wg.Add(1)
		go func(e OutboxEvent) {
			defer wg.Done()
			defer func() { <-sem }()
			defer pending.done(e.ID)
			if r.publishOne(ctx, e) {
				mu.Lock()
				published++
				mu.Unlock()
			}
		}(e)
	}
	wg.Wait()
	return published, nil
}

// publishOne publica e marca o evento. Falha no broker agenda nova tentativa
// com backoff; nada é descartado.
func (r *OutboxRelay) publishOne(ctx context.Context, e OutboxEvent) bool {
	log := r.log.With("eventId", e.ID.String(), "eventType", e.EventType, "aggregateId", e.AggregateID)
	if err := r.publisher.Publish(ctx, e); err != nil {
		attempt := e.Attempts + 1
		next := r.clock.Now().Add(r.backoff(attempt))
		msg := err.Error()
		if len(msg) > 500 {
			msg = msg[:500]
		}
		if ferr := r.uow.Do(ctx, func(ctx context.Context, tx Tx) error {
			return tx.Outbox().MarkFailed(ctx, e.ID, r.cfg.Owner, next, msg)
		}); ferr != nil {
			log.Warn("could not record publish failure; the lease will expire", "error", ferr)
		}
		log.Warn("event publish failed", "attempt", attempt, "nextAttemptAt", next.UTC().Format(time.RFC3339), "error", err)
		return false
	}
	if r.afterPublish != nil {
		if err := r.afterPublish(e); err != nil {
			log.Warn("interrupted after publish, before marking", "error", err)
			return false
		}
	}
	var owned bool
	err := r.uow.Do(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		owned, err = tx.Outbox().MarkPublished(ctx, e.ID, r.cfg.Owner, domain.Timestamp(r.clock.Now()))
		return err
	})
	switch {
	case err != nil:
		log.Warn("event published but not marked; it will be republished with the same eventId", "error", err)
		return false
	case !owned:
		log.Warn("event published after the lease was taken over; duplicates share the eventId")
		return false
	}
	log.Debug("event published")
	return true
}

func (r *OutboxRelay) backoff(attempt int) time.Duration {
	d := r.cfg.RetryBase
	for i := 1; i < attempt && d < r.cfg.RetryMax; i++ {
		d *= 2
	}
	if d > r.cfg.RetryMax {
		d = r.cfg.RetryMax
	}
	return jitter(d)
}

// renewWhile estende o lease dos eventos ainda não concluídos a cada terço do
// lease, para que um lote lento não seja assumido por outro publisher.
func (r *OutboxRelay) renewWhile(set *claimSet) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	interval := r.cfg.Lease / 3
	if interval <= 0 {
		interval = time.Second
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ids := set.unfinished()
				if len(ids) == 0 {
					continue
				}
				until := r.clock.Now().Add(r.cfg.Lease)
				if err := r.uow.Do(ctx, func(ctx context.Context, tx Tx) error {
					return tx.Outbox().Renew(ctx, r.cfg.Owner, ids, until)
				}); err != nil && ctx.Err() == nil {
					r.log.Warn("lease renewal failed", "events", len(ids), "error", err)
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (r *OutboxRelay) release(ids []domain.EventID) {
	if len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.uow.Do(ctx, func(ctx context.Context, tx Tx) error {
		return tx.Outbox().Release(ctx, r.cfg.Owner, ids)
	}); err != nil {
		r.log.Warn("could not release claims; they will expire with the lease", "events", len(ids), "error", err)
	}
}

// claimSet acompanha os eventos de um lote: não iniciados, em andamento e concluídos.
type claimSet struct {
	mu    sync.Mutex
	state map[domain.EventID]int // 0 não iniciado, 1 em andamento, 2 concluído
	order []domain.EventID
}

func newClaimSet(batch []OutboxEvent) *claimSet {
	s := &claimSet{state: make(map[domain.EventID]int, len(batch))}
	for _, e := range batch {
		s.state[e.ID] = 0
		s.order = append(s.order, e.ID)
	}
	return s
}

func (s *claimSet) start(id domain.EventID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state[id] != 0 {
		return false
	}
	s.state[id] = 1
	return true
}

func (s *claimSet) done(id domain.EventID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state[id] = 2
}

// unfinished: não iniciados e em andamento (precisam de lease).
func (s *claimSet) unfinished() []domain.EventID {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []domain.EventID
	for _, id := range s.order {
		if s.state[id] != 2 {
			ids = append(ids, id)
		}
	}
	return ids
}

// remaining: não iniciados; marca-os como concluídos para não serem iniciados depois.
func (s *claimSet) remaining() []domain.EventID {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []domain.EventID
	for _, id := range s.order {
		if s.state[id] == 0 {
			s.state[id] = 2
			ids = append(ids, id)
		}
	}
	return ids
}
