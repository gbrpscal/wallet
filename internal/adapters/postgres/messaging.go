package postgres

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/event"
)

type outboxRepo struct {
	q pgx.Tx
}

// Append grava cada evento como snapshot: os bytes de event.Marshal são
// armazenados como estão (coluna json) e republicados sem alteração.
func (r outboxRepo) Append(ctx context.Context, events ...event.Event) error {
	for _, e := range events {
		payload, err := event.Marshal(e)
		if err != nil {
			return err
		}
		_, err = r.q.Exec(ctx, `
			INSERT INTO outbox_events (event_id, aggregate_id, event_type, payload, occurred_at, next_attempt_at)
			VALUES ($1, $2, $3, CAST($4::text AS json), $5, $5)`,
			e.ID().String(), e.AggregateID(), e.Type(), string(payload), e.OccurredAt())
		if err != nil {
			return translate(err)
		}
	}
	return nil
}

type inboxRepo struct {
	q pgx.Tx
}

func (r inboxRepo) Register(ctx context.Context, m app.InboxMessage) (app.InboxMessage, bool, error) {
	tag, err := r.q.Exec(ctx, `
		INSERT INTO inbox_messages (consumer_name, message_id, payload_hash, received_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (consumer_name, message_id) DO NOTHING`,
		m.ConsumerName, m.MessageID, m.PayloadHash, m.ReceivedAt)
	if err != nil {
		return app.InboxMessage{}, false, translate(err)
	}
	if tag.RowsAffected() == 1 {
		return m, true, nil
	}
	var existing app.InboxMessage
	err = r.q.QueryRow(ctx, `
		SELECT consumer_name, message_id, payload_hash, received_at, completed_at
		FROM inbox_messages WHERE consumer_name = $1 AND message_id = $2`,
		m.ConsumerName, m.MessageID).
		Scan(&existing.ConsumerName, &existing.MessageID, &existing.PayloadHash, &existing.ReceivedAt, &existing.CompletedAt)
	if err != nil {
		return app.InboxMessage{}, false, translate(err)
	}
	return existing, false, nil
}

func (r inboxRepo) MarkCompleted(ctx context.Context, consumerName, messageID string, at time.Time) error {
	tag, err := r.q.Exec(ctx, `
		UPDATE inbox_messages SET completed_at = $3
		WHERE consumer_name = $1 AND message_id = $2 AND completed_at IS NULL`,
		consumerName, messageID, at)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() != 1 {
		return app.ErrConcurrentUpdate
	}
	return nil
}

// Claim reserva eventos pendentes e vencidos (sem reserva ou com lease
// vencido). Só entra o evento mais antigo de cada agregado, e apenas quando
// não há evento anterior do mesmo agregado ainda não publicado: isso garante a
// ordem por agregado mesmo com vários publishers. FOR UPDATE SKIP LOCKED
// impede que dois publishers reservem o mesmo evento.
func (r outboxRepo) Claim(ctx context.Context, owner string, now, until time.Time, limit int) ([]app.OutboxEvent, error) {
	rows, err := r.q.Query(ctx, `
		WITH candidates AS (
			SELECT e.event_id
			FROM outbox_events e
			WHERE e.published_at IS NULL
			  AND e.next_attempt_at <= $2
			  AND (e.claimed_until IS NULL OR e.claimed_until < $2)
			  AND NOT EXISTS (
			      SELECT 1 FROM outbox_events p
			      WHERE p.aggregate_id = e.aggregate_id
			        AND p.published_at IS NULL
			        AND p.sequence < e.sequence)
			ORDER BY e.sequence
			LIMIT $4
			FOR UPDATE SKIP LOCKED)
		UPDATE outbox_events o
		SET claimed_by = $1, claimed_until = $3
		FROM candidates c
		WHERE o.event_id = c.event_id
		RETURNING o.event_id::text, o.aggregate_id::text, o.event_type, o.payload::text, o.attempts, o.sequence`,
		owner, now, until, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	type claimed struct {
		event    app.OutboxEvent
		sequence int64
	}
	var out []claimed
	for rows.Next() {
		var id, payload string
		var c claimed
		if err := rows.Scan(&id, &c.event.AggregateID, &c.event.EventType, &payload, &c.event.Attempts, &c.sequence); err != nil {
			return nil, translate(err)
		}
		if c.event.ID, err = domain.ParseID[domain.EventID](id); err != nil {
			return nil, inconsistent("outbox event id", err)
		}
		var header struct {
			Version int `json:"version"`
		}
		if err := json.Unmarshal([]byte(payload), &header); err != nil {
			return nil, inconsistent("outbox payload", err)
		}
		c.event.EventVersion = header.Version
		c.event.Payload = []byte(payload)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, translate(err)
	}
	// RETURNING não preserva a ordem; devolve na ordem de sequence.
	sort.Slice(out, func(i, j int) bool { return out[i].sequence < out[j].sequence })
	events := make([]app.OutboxEvent, len(out))
	for i, c := range out {
		events[i] = c.event
	}
	return events, nil
}

func (r outboxRepo) Renew(ctx context.Context, owner string, ids []domain.EventID, until time.Time) error {
	_, err := r.q.Exec(ctx, `
		UPDATE outbox_events SET claimed_until = $2
		WHERE claimed_by = $1 AND published_at IS NULL AND event_id = ANY($3::uuid[])`,
		owner, until, idStrings(ids))
	return translate(err)
}

func (r outboxRepo) MarkPublished(ctx context.Context, id domain.EventID, owner string, at time.Time) (bool, error) {
	tag, err := r.q.Exec(ctx, `
		UPDATE outbox_events
		SET published_at = $3, attempts = attempts + 1, claimed_by = NULL, claimed_until = NULL, last_error = NULL
		WHERE event_id = $1 AND claimed_by = $2 AND published_at IS NULL`,
		id.String(), owner, at)
	if err != nil {
		return false, translate(err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r outboxRepo) MarkFailed(ctx context.Context, id domain.EventID, owner string, next time.Time, lastError string) error {
	_, err := r.q.Exec(ctx, `
		UPDATE outbox_events
		SET attempts = attempts + 1, next_attempt_at = $3, last_error = $4, claimed_by = NULL, claimed_until = NULL
		WHERE event_id = $1 AND claimed_by = $2 AND published_at IS NULL`,
		id.String(), owner, next, lastError)
	return translate(err)
}

func (r outboxRepo) Release(ctx context.Context, owner string, ids []domain.EventID) error {
	_, err := r.q.Exec(ctx, `
		UPDATE outbox_events SET claimed_by = NULL, claimed_until = NULL
		WHERE claimed_by = $1 AND published_at IS NULL AND event_id = ANY($2::uuid[])`,
		owner, idStrings(ids))
	return translate(err)
}

func idStrings(ids []domain.EventID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}
