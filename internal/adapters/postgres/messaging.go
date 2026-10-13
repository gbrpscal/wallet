package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gbrpscal/wallet/internal/app"
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
