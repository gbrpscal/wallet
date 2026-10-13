-- Carteiras. O par (player_id, currency) identifica uma única carteira.
-- Saldo em unidades mínimas (centavos), nunca negativo.

-- +goose Up
GRANT USAGE ON SCHEMA public TO wallet_app;

CREATE TABLE wallets (
    id            UUID        PRIMARY KEY,
    player_id     UUID        NOT NULL,
    currency      CHAR(3)     NOT NULL CHECK (currency IN ('BRL', 'USD', 'EUR')),
    balance_minor BIGINT      NOT NULL CHECK (balance_minor >= 0),
    version       BIGINT      NOT NULL CHECK (version >= 1),
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL,
    CONSTRAINT wallets_player_currency_key UNIQUE (player_id, currency),
    -- Alvo das chaves estrangeiras compostas que garantem a moeda do lançamento.
    CONSTRAINT wallets_id_currency_key UNIQUE (id, currency),
    CONSTRAINT wallets_timestamps_check CHECK (updated_at >= created_at)
);

-- Identidade imutável; a versão só muda junto com o saldo, sempre +1;
-- carteiras não são apagadas.
-- +goose StatementBegin
CREATE FUNCTION wallets_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'wallets: % is not allowed', TG_OP
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.id, NEW.player_id, NEW.currency, NEW.created_at)
        IS DISTINCT FROM (OLD.id, OLD.player_id, OLD.currency, OLD.created_at) THEN
        RAISE EXCEPTION 'wallets: identity of wallet % is immutable', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.balance_minor <> OLD.balance_minor THEN
        IF NEW.version <> OLD.version + 1 THEN
            RAISE EXCEPTION 'wallets: balance change of wallet % must increment version by one', OLD.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    ELSIF NEW.version <> OLD.version THEN
        RAISE EXCEPTION 'wallets: version of wallet % changes only with its balance', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'wallets: updated_at of wallet % cannot move backwards', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER wallets_guard_update BEFORE UPDATE ON wallets
    FOR EACH ROW EXECUTE FUNCTION wallets_guard();
CREATE TRIGGER wallets_guard_delete BEFORE DELETE ON wallets
    FOR EACH ROW EXECUTE FUNCTION wallets_guard();
CREATE TRIGGER wallets_guard_truncate BEFORE TRUNCATE ON wallets
    FOR EACH STATEMENT EXECUTE FUNCTION wallets_guard();

GRANT SELECT, INSERT, UPDATE ON wallets TO wallet_app;

-- +goose Down
DROP TABLE wallets;
DROP FUNCTION wallets_guard();
REVOKE USAGE ON SCHEMA public FROM wallet_app;
