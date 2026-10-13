-- Ledger append-only. Cada lançamento grava a versão de carteira que produziu
-- (wallet_version), o que encadeia os lançamentos e permite ao banco conferir,
-- no COMMIT, que saldo e versão da carteira batem com o último lançamento.

-- +goose Up
CREATE TABLE wallet_ledger_entries (
    id                   UUID        PRIMARY KEY,
    wallet_id            UUID        NOT NULL,
    transaction_id       UUID        NOT NULL,
    direction            TEXT        NOT NULL CHECK (direction IN ('DEBIT', 'CREDIT')),
    amount_minor         BIGINT      NOT NULL CHECK (amount_minor > 0),
    currency             CHAR(3)     NOT NULL,
    balance_before_minor BIGINT      NOT NULL CHECK (balance_before_minor >= 0),
    balance_after_minor  BIGINT      NOT NULL CHECK (balance_after_minor >= 0),
    wallet_version       BIGINT      NOT NULL CHECK (wallet_version >= 1),
    created_at           TIMESTAMPTZ NOT NULL,

    CONSTRAINT wallet_ledger_entries_wallet_transaction_key UNIQUE (wallet_id, transaction_id),
    CONSTRAINT wallet_ledger_entries_wallet_version_key UNIQUE (wallet_id, wallet_version),
    -- A moeda do lançamento é a da carteira e a da transação.
    CONSTRAINT wallet_ledger_entries_wallet_fkey FOREIGN KEY (wallet_id, currency)
        REFERENCES wallets (id, currency),
    CONSTRAINT wallet_ledger_entries_transaction_fkey FOREIGN KEY (transaction_id, wallet_id, currency)
        REFERENCES wager_transactions (id, wallet_id, currency),
    CONSTRAINT wallet_ledger_entries_balance_check CHECK (
        (direction = 'CREDIT' AND balance_after_minor = balance_before_minor + amount_minor)
        OR (direction = 'DEBIT' AND balance_after_minor = balance_before_minor - amount_minor)
    )
);

-- Imutabilidade: nenhum lançamento é alterado ou apagado.
-- +goose StatementBegin
CREATE FUNCTION wallet_ledger_entries_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'wallet_ledger_entries is append-only: % is not allowed', TG_OP
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER wallet_ledger_entries_no_update BEFORE UPDATE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION wallet_ledger_entries_immutable();
CREATE TRIGGER wallet_ledger_entries_no_delete BEFORE DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION wallet_ledger_entries_immutable();
CREATE TRIGGER wallet_ledger_entries_no_truncate BEFORE TRUNCATE ON wallet_ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION wallet_ledger_entries_immutable();

-- Cadeia: o primeiro lançamento parte de saldo zero (versão 1 na abertura com
-- saldo, 2 na primeira movimentação de carteira aberta com zero); os
-- seguintes continuam exatamente do anterior. O valor é o da transação, LOSS
-- nunca gera lançamento e a direção é coerente com o tipo.
-- +goose StatementBegin
CREATE FUNCTION wallet_ledger_entries_chain() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    prev RECORD;
    tx   RECORD;
BEGIN
    SELECT kind, amount_minor INTO tx FROM wager_transactions WHERE id = NEW.transaction_id;
    IF tx.amount_minor <> NEW.amount_minor THEN
        RAISE EXCEPTION 'ledger: amount of entry % differs from transaction %', NEW.id, NEW.transaction_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF tx.kind = 'LOSS'
        OR (tx.kind IN ('OPENING', 'WIN', 'REFUND') AND NEW.direction <> 'CREDIT')
        OR (tx.kind = 'BET' AND NEW.direction <> 'DEBIT') THEN
        RAISE EXCEPTION 'ledger: direction % is not valid for % transaction %', NEW.direction, tx.kind, NEW.transaction_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    SELECT balance_after_minor, wallet_version INTO prev
    FROM wallet_ledger_entries
    WHERE wallet_id = NEW.wallet_id
    ORDER BY wallet_version DESC
    LIMIT 1;
    IF FOUND THEN
        IF NEW.wallet_version <> prev.wallet_version + 1
            OR NEW.balance_before_minor <> prev.balance_after_minor THEN
            RAISE EXCEPTION 'ledger: entry % breaks the chain of wallet % (expected version % from balance %)',
                NEW.id, NEW.wallet_id, prev.wallet_version + 1, prev.balance_after_minor
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    ELSIF NEW.balance_before_minor <> 0 OR NEW.wallet_version NOT IN (1, 2) THEN
        RAISE EXCEPTION 'ledger: first entry % of wallet % must start from zero', NEW.id, NEW.wallet_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER wallet_ledger_entries_chain BEFORE INSERT ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION wallet_ledger_entries_chain();

-- Conferência diferida para o COMMIT: o saldo e a versão confirmados de cada
-- carteira tocada na transação são os do seu último lançamento (ou zero na
-- versão 1, sem lançamento). Lê o estado atual da linha, não NEW, porque a
-- carteira pode ter mudado várias vezes na mesma transação.
-- +goose StatementBegin
CREATE FUNCTION wallet_ledger_consistency() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    target UUID;
    w      RECORD;
    last   RECORD;
BEGIN
    IF TG_TABLE_NAME = 'wallets' THEN
        target := NEW.id;
    ELSE
        target := NEW.wallet_id;
    END IF;

    SELECT balance_minor, version INTO w FROM wallets WHERE id = target;
    SELECT balance_after_minor, wallet_version INTO last
    FROM wallet_ledger_entries
    WHERE wallet_id = target
    ORDER BY wallet_version DESC
    LIMIT 1;
    IF NOT FOUND THEN
        IF w.version <> 1 OR w.balance_minor <> 0 THEN
            RAISE EXCEPTION 'wallet % has balance % at version % without ledger entries', target, w.balance_minor, w.version
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    ELSIF last.wallet_version <> w.version OR last.balance_after_minor <> w.balance_minor THEN
        RAISE EXCEPTION 'wallet % (balance %, version %) does not match its last ledger entry (balance %, version %)',
            target, w.balance_minor, w.version, last.balance_after_minor, last.wallet_version
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER wallets_ledger_consistency
    AFTER INSERT OR UPDATE ON wallets
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wallet_ledger_consistency();
CREATE CONSTRAINT TRIGGER wallet_ledger_entries_consistency
    AFTER INSERT ON wallet_ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wallet_ledger_consistency();

-- A aplicação só lê e acrescenta.
GRANT SELECT, INSERT ON wallet_ledger_entries TO wallet_app;

-- +goose Down
DROP TRIGGER wallets_ledger_consistency ON wallets;
DROP TABLE wallet_ledger_entries;
DROP FUNCTION wallet_ledger_consistency();
DROP FUNCTION wallet_ledger_entries_chain();
DROP FUNCTION wallet_ledger_entries_immutable();
