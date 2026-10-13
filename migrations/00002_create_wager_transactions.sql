-- Transações de wagering. Operações externas carregam os metadados do
-- provedor; a abertura interna (OPENING) não carrega nenhum. As constraints
-- espelham as regras do domínio para que o banco as garanta sozinho.

-- +goose Up
CREATE TABLE wager_transactions (
    id                                UUID        PRIMARY KEY,
    kind                              TEXT        NOT NULL
        CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    wallet_id                         UUID        NOT NULL REFERENCES wallets (id),
    player_id                         UUID        NOT NULL,
    amount_minor                      BIGINT      NOT NULL CHECK (amount_minor >= 0),
    currency                          CHAR(3)     NOT NULL CHECK (currency IN ('BRL', 'USD', 'EUR')),
    status                            TEXT        NOT NULL
        CHECK (status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),
    provider_id                       TEXT        CHECK (octet_length(provider_id) BETWEEN 1 AND 255),
    external_transaction_id           TEXT        CHECK (octet_length(external_transaction_id) BETWEEN 1 AND 255),
    idempotency_key                   TEXT        CHECK (octet_length(idempotency_key) BETWEEN 1 AND 255),
    payload_hash                      TEXT        CHECK (payload_hash ~ '^v[0-9]+:[0-9a-f]{64}$'),
    round_id                          TEXT        CHECK (octet_length(round_id) BETWEEN 1 AND 255),
    game_id                           TEXT        CHECK (octet_length(game_id) BETWEEN 1 AND 255),
    reference_external_transaction_id TEXT        CHECK (octet_length(reference_external_transaction_id) BETWEEN 1 AND 255),
    reference_transaction_id          UUID,
    failure_code                      TEXT,
    result_balance_minor              BIGINT      CHECK (result_balance_minor >= 0),
    created_at                        TIMESTAMPTZ NOT NULL,
    updated_at                        TIMESTAMPTZ NOT NULL,

    -- Alvo das chaves estrangeiras compostas (ledger e referências).
    CONSTRAINT wager_transactions_id_wallet_currency_key UNIQUE (id, wallet_id, currency),
    CONSTRAINT wager_transactions_id_wallet_key UNIQUE (id, wallet_id),
    -- A referência resolvida pertence à mesma carteira.
    CONSTRAINT wager_transactions_reference_fkey FOREIGN KEY (reference_transaction_id, wallet_id)
        REFERENCES wager_transactions (id, wallet_id),

    -- Operação interna vs. externa: impede metadados externos no OPENING e
    -- exige todos eles nas operações externas.
    CONSTRAINT wager_transactions_origin_check CHECK (
        (kind = 'OPENING'
            AND provider_id IS NULL AND external_transaction_id IS NULL AND idempotency_key IS NULL
            AND payload_hash IS NULL AND round_id IS NULL AND game_id IS NULL
            AND reference_external_transaction_id IS NULL AND reference_transaction_id IS NULL
            AND status = 'PROCESSED' AND amount_minor > 0)
        OR
        (kind <> 'OPENING'
            AND provider_id IS NOT NULL AND external_transaction_id IS NOT NULL
            AND idempotency_key IS NOT NULL AND payload_hash IS NOT NULL
            AND round_id IS NOT NULL AND game_id IS NOT NULL)
    ),
    -- Zero só em LOSS; os demais tipos exigem valor positivo.
    CONSTRAINT wager_transactions_amount_check CHECK (
        (kind = 'LOSS' AND amount_minor = 0) OR (kind <> 'LOSS' AND amount_minor > 0)
    ),
    -- Reversões exigem referência; BET e LOSS não aceitam.
    CONSTRAINT wager_transactions_reference_check CHECK (
        (kind IN ('REFUND', 'ROLLBACK') AND reference_external_transaction_id IS NOT NULL)
        OR (kind IN ('BET', 'LOSS') AND reference_external_transaction_id IS NULL)
        OR kind IN ('WIN', 'OPENING')
    ),
    -- Resultado coerente com o estado.
    CONSTRAINT wager_transactions_result_check CHECK (
        (status IN ('PENDING', 'PENDING_REFERENCE')
            AND failure_code IS NULL AND result_balance_minor IS NULL AND reference_transaction_id IS NULL)
        OR (status = 'PROCESSED' AND failure_code IS NULL AND result_balance_minor IS NOT NULL)
        OR (status = 'REJECTED' AND failure_code IN (
            'INSUFFICIENT_FUNDS', 'REVERSAL_INSUFFICIENT_FUNDS', 'CURRENCY_MISMATCH',
            'WALLET_PLAYER_MISMATCH', 'REFERENCE_NOT_FOUND', 'REFERENCE_NOT_PROCESSED',
            'REFERENCE_MISMATCH', 'REFERENCE_AMOUNT_MISMATCH', 'REFERENCE_KIND_NOT_REVERSIBLE',
            'ALREADY_REVERSED'))
        OR (status = 'FAILED' AND failure_code = 'INCONSISTENT_STATE' AND result_balance_minor IS NULL)
    ),
    -- Só reversões esperam referência; reversão processada tem a referência resolvida;
    -- BET, LOSS e OPENING nunca têm referência resolvida.
    CONSTRAINT wager_transactions_pending_reference_check CHECK (
        status <> 'PENDING_REFERENCE' OR kind IN ('REFUND', 'ROLLBACK')
    ),
    CONSTRAINT wager_transactions_resolved_reference_check CHECK (
        (kind IN ('REFUND', 'ROLLBACK') AND (status <> 'PROCESSED' OR reference_transaction_id IS NOT NULL))
        OR (kind IN ('BET', 'LOSS', 'OPENING') AND reference_transaction_id IS NULL)
        OR kind = 'WIN'
    ),
    CONSTRAINT wager_transactions_timestamps_check CHECK (updated_at >= created_at)
);

-- Idempotência isolada por provedor.
CREATE UNIQUE INDEX wager_transactions_provider_key_uq
    ON wager_transactions (provider_id, idempotency_key) WHERE kind <> 'OPENING';
-- A mesma operação do provedor não pode ser registrada com outra chave.
CREATE UNIQUE INDEX wager_transactions_provider_external_uq
    ON wager_transactions (provider_id, external_transaction_id) WHERE kind <> 'OPENING';
-- No máximo um crédito inicial por carteira.
CREATE UNIQUE INDEX wager_transactions_opening_uq
    ON wager_transactions (wallet_id) WHERE kind = 'OPENING';
-- No máximo uma reversão bem-sucedida por transação referenciada (D5).
CREATE UNIQUE INDEX wager_transactions_one_reversal_uq
    ON wager_transactions (reference_transaction_id)
    WHERE status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK');

-- Máquina de estados no banco: estados terminais não mudam, só transições
-- válidas são aceitas, colunas de identidade e de payload são imutáveis e
-- nenhuma transação é apagada.
-- +goose StatementBegin
CREATE FUNCTION wager_transactions_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'wager_transactions: % is not allowed', TG_OP
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status IN ('PROCESSED', 'REJECTED', 'FAILED') THEN
        RAISE EXCEPTION 'wager_transactions: transaction % is terminal (%)', OLD.id, OLD.status
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT (
        (OLD.status = 'PENDING'
            AND NEW.status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED'))
        OR (OLD.status = 'PENDING_REFERENCE'
            AND NEW.status IN ('PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED'))
    ) THEN
        RAISE EXCEPTION 'wager_transactions: transition % -> % is not allowed for %', OLD.status, NEW.status, OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.id, NEW.kind, NEW.wallet_id, NEW.player_id, NEW.amount_minor, NEW.currency,
        NEW.provider_id, NEW.external_transaction_id, NEW.idempotency_key, NEW.payload_hash,
        NEW.round_id, NEW.game_id, NEW.reference_external_transaction_id, NEW.created_at)
        IS DISTINCT FROM
       (OLD.id, OLD.kind, OLD.wallet_id, OLD.player_id, OLD.amount_minor, OLD.currency,
        OLD.provider_id, OLD.external_transaction_id, OLD.idempotency_key, OLD.payload_hash,
        OLD.round_id, OLD.game_id, OLD.reference_external_transaction_id, OLD.created_at) THEN
        RAISE EXCEPTION 'wager_transactions: immutable columns of % cannot change', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'wager_transactions: updated_at of % cannot move backwards', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER wager_transactions_guard_update BEFORE UPDATE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_guard();
CREATE TRIGGER wager_transactions_guard_delete BEFORE DELETE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_guard();
CREATE TRIGGER wager_transactions_guard_truncate BEFORE TRUNCATE ON wager_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION wager_transactions_guard();

GRANT SELECT, INSERT, UPDATE ON wager_transactions TO wallet_app;

-- +goose Down
DROP TABLE wager_transactions;
DROP FUNCTION wager_transactions_guard();
