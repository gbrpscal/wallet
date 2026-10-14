-- Reserva com lease para vários publishers (D10) e ordem por agregado.
--   sequence       ordem de inserção; eventos de uma carteira são inseridos sob
--                  o lock da carteira, então ela segue a ordem real dos commits,
--                  sem depender do relógio das instâncias
--   claimed_by     publisher que reservou o evento
--   claimed_until  fim do lease; vencido, outro publisher assume

-- +goose Up
ALTER TABLE outbox_events
    ADD COLUMN sequence BIGINT GENERATED ALWAYS AS IDENTITY,
    ADD COLUMN claimed_by TEXT CHECK (octet_length(claimed_by) BETWEEN 1 AND 255),
    ADD COLUMN claimed_until TIMESTAMPTZ,
    ADD CONSTRAINT outbox_events_claim_check CHECK ((claimed_by IS NULL) = (claimed_until IS NULL));

ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_sequence_key UNIQUE (sequence);

-- Busca do evento pendente anterior do mesmo agregado.
CREATE INDEX outbox_events_aggregate_pending_idx
    ON outbox_events (aggregate_id, sequence) WHERE published_at IS NULL;

-- A sequência também é imutável.
-- +goose StatementBegin
CREATE FUNCTION outbox_events_sequence_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.sequence IS DISTINCT FROM OLD.sequence THEN
        RAISE EXCEPTION 'outbox_events: sequence of % is immutable', OLD.event_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER outbox_events_sequence_guard_update BEFORE UPDATE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION outbox_events_sequence_guard();

-- +goose Down
DROP TRIGGER outbox_events_sequence_guard_update ON outbox_events;
DROP FUNCTION outbox_events_sequence_guard();
DROP INDEX outbox_events_aggregate_pending_idx;
ALTER TABLE outbox_events
    DROP CONSTRAINT outbox_events_sequence_key,
    DROP CONSTRAINT outbox_events_claim_check,
    DROP COLUMN claimed_until,
    DROP COLUMN claimed_by,
    DROP COLUMN sequence;
