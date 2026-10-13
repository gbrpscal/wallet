-- Inbox (deduplicação de mensagens recebidas) e outbox (eventos a publicar).

-- +goose Up
CREATE TABLE inbox_messages (
    consumer_name TEXT        NOT NULL CHECK (octet_length(consumer_name) BETWEEN 1 AND 255),
    message_id    TEXT        NOT NULL CHECK (octet_length(message_id) BETWEEN 1 AND 255),
    payload_hash  TEXT        NOT NULL,
    received_at   TIMESTAMPTZ NOT NULL,
    completed_at  TIMESTAMPTZ,
    CONSTRAINT inbox_messages_pkey PRIMARY KEY (consumer_name, message_id),
    CONSTRAINT inbox_messages_timestamps_check CHECK (completed_at IS NULL OR completed_at >= received_at)
);

-- Identidade e hash imutáveis; a conclusão é registrada uma única vez.
-- +goose StatementBegin
CREATE FUNCTION inbox_messages_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.consumer_name, NEW.message_id, NEW.payload_hash, NEW.received_at)
        IS DISTINCT FROM (OLD.consumer_name, OLD.message_id, OLD.payload_hash, OLD.received_at)
        OR (OLD.completed_at IS NOT NULL AND NEW.completed_at IS DISTINCT FROM OLD.completed_at) THEN
        RAISE EXCEPTION 'inbox_messages: message %/% is immutable once completed', OLD.consumer_name, OLD.message_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER inbox_messages_guard_update BEFORE UPDATE ON inbox_messages
    FOR EACH ROW EXECUTE FUNCTION inbox_messages_guard();

CREATE TABLE outbox_events (
    event_id        UUID        PRIMARY KEY,
    aggregate_id    UUID        NOT NULL,
    event_type      TEXT        NOT NULL CHECK (event_type IN (
        'WagerTransactionProcessed', 'WagerTransactionRejected',
        'WalletBalanceChanged', 'WagerTransactionPendingReference')),
    -- json (e não jsonb) preserva o texto exato do snapshot.
    payload         JSON        NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL,
    attempts        INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    published_at    TIMESTAMPTZ,
    last_error      TEXT
);

CREATE INDEX outbox_events_pending_idx ON outbox_events (next_attempt_at) WHERE published_at IS NULL;

-- Snapshot imutável: identidade, tipo, payload e ocorrência nunca mudam; uma
-- vez publicado, o evento não volta a pendente. Eventos não são apagados.
-- +goose StatementBegin
CREATE FUNCTION outbox_events_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'outbox_events: % is not allowed', TG_OP
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.event_id, NEW.aggregate_id, NEW.event_type, NEW.payload::text, NEW.occurred_at)
        IS DISTINCT FROM (OLD.event_id, OLD.aggregate_id, OLD.event_type, OLD.payload::text, OLD.occurred_at)
        OR (OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at) THEN
        RAISE EXCEPTION 'outbox_events: event % is an immutable snapshot', OLD.event_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER outbox_events_guard_update BEFORE UPDATE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION outbox_events_guard();
CREATE TRIGGER outbox_events_guard_delete BEFORE DELETE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION outbox_events_guard();

GRANT SELECT, INSERT, UPDATE ON inbox_messages TO wallet_app;
GRANT SELECT, INSERT, UPDATE ON outbox_events TO wallet_app;

-- +goose Down
DROP TABLE outbox_events;
DROP FUNCTION outbox_events_guard();
DROP TABLE inbox_messages;
DROP FUNCTION inbox_messages_guard();
