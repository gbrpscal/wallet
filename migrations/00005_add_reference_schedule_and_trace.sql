-- Agendamento durável da espera por referência (D4) e rastreio da requisição
-- que originou cada transação, para que eventos emitidos depois pelo worker
-- carreguem o mesmo correlationId.

-- +goose Up
ALTER TABLE wager_transactions
    -- Linhas anteriores a esta migration recebem 'unknown' e a coluna passa a
    -- ser obrigatória para as novas.
    ADD COLUMN correlation_id TEXT NOT NULL DEFAULT 'unknown'
        CHECK (octet_length(correlation_id) BETWEEN 1 AND 255),
    ADD COLUMN causation_id TEXT
        CHECK (octet_length(causation_id) BETWEEN 1 AND 255),
    ADD COLUMN reference_attempts INTEGER NOT NULL DEFAULT 0 CHECK (reference_attempts >= 0),
    ADD COLUMN next_reference_attempt_at TIMESTAMPTZ,
    ADD COLUMN reference_expires_at TIMESTAMPTZ,
    -- Toda transação esperando referência tem próxima tentativa e prazo.
    ADD CONSTRAINT wager_transactions_reference_schedule_check CHECK (
        status <> 'PENDING_REFERENCE'
        OR (next_reference_attempt_at IS NOT NULL AND reference_expires_at IS NOT NULL)
    );
ALTER TABLE wager_transactions ALTER COLUMN correlation_id DROP DEFAULT;

-- Fila de retomada do worker de referências.
CREATE INDEX wager_transactions_due_reference_idx
    ON wager_transactions (next_reference_attempt_at)
    WHERE status = 'PENDING_REFERENCE';

-- Rastreio imutável; o contador de tentativas só cresce.
-- +goose StatementBegin
CREATE FUNCTION wager_transactions_trace_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.correlation_id, NEW.causation_id) IS DISTINCT FROM (OLD.correlation_id, OLD.causation_id) THEN
        RAISE EXCEPTION 'wager_transactions: trace of % is immutable', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.reference_attempts < OLD.reference_attempts THEN
        RAISE EXCEPTION 'wager_transactions: reference attempts of % cannot decrease', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER wager_transactions_trace_guard_update BEFORE UPDATE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_trace_guard();

-- +goose Down
DROP TRIGGER wager_transactions_trace_guard_update ON wager_transactions;
DROP FUNCTION wager_transactions_trace_guard();
DROP INDEX wager_transactions_due_reference_idx;
ALTER TABLE wager_transactions
    DROP CONSTRAINT wager_transactions_reference_schedule_check,
    DROP COLUMN reference_expires_at,
    DROP COLUMN next_reference_attempt_at,
    DROP COLUMN reference_attempts,
    DROP COLUMN causation_id,
    DROP COLUMN correlation_id;
