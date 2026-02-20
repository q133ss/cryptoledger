BEGIN;

CREATE SCHEMA IF NOT EXISTS finance;
SET search_path = finance, pg_catalog;


CREATE TABLE accounts (
    id          text PRIMARY KEY,
    owner_type  text NOT NULL CHECK (owner_type IN ('player', 'company', 'house', 'bonus')),
    owner_id    text NOT NULL,
    currency    text NOT NULL,
    kind        text NOT NULL CHECK (kind IN (
                    'player_main', 'player_locked', 'player_bonus',
                    'house_hot', 'house_equity', 'fee')),
    scale       smallint NOT NULL DEFAULT 18 CHECK (scale BETWEEN 0 AND 18),
    created_at  text NOT NULL
);

CREATE TABLE account_balances (
    account_id  text PRIMARY KEY REFERENCES accounts(id),
    balance     bigint NOT NULL DEFAULT 0,
    updated_at  timestamptz NOT NULL DEFAULT now()
);


CREATE TABLE transactions (
    tx_id            text PRIMARY KEY,
    idempotency_key  text NOT NULL UNIQUE,
    request_hash     bytea NOT NULL,      -- SHA-256 канонического тела запроса
    operation        text NOT NULL,
    reference        text NOT NULL,
    actor_id         text NOT NULL,       -- сервис или сотрудник
    note             text NOT NULL DEFAULT '',
    currency         text NOT NULL,
    scale            smallint NOT NULL,
    digest           bytea NOT NULL,      -- что подписал HSM
    key_id           text NOT NULL,
    signature        bytea NOT NULL,
    created_at       text NOT NULL
);

CREATE TABLE ledger_entries (
    seq         bigserial PRIMARY KEY,
    tx_id       text NOT NULL REFERENCES transactions(tx_id),
    account_id  text NOT NULL REFERENCES accounts(id),
    amount      bigint NOT NULL CHECK (amount <> 0),
    created_at  text NOT NULL
);

CREATE INDEX ix_entries_tx      ON ledger_entries (tx_id);
CREATE INDEX ix_entries_account ON ledger_entries (account_id, seq DESC);


CREATE OR REPLACE FUNCTION assert_transaction_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    total numeric;
BEGIN
    SELECT COALESCE(SUM(amount), 0) INTO total
    FROM ledger_entries
    WHERE tx_id = NEW.tx_id;

    IF total <> 0 OR (SELECT count(*) FROM ledger_entries WHERE tx_id = NEW.tx_id) < 2 THEN
        RAISE EXCEPTION 'double-entry violation in tx %: sum = %', NEW.tx_id, total;
    END IF;
    RETURN NEW;
END $$;

CREATE CONSTRAINT TRIGGER trg_entries_balanced
AFTER INSERT ON ledger_entries
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_transaction_balanced();


CREATE CONSTRAINT TRIGGER trg_header_balanced AFTER INSERT ON transactions
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION assert_transaction_balanced();

CREATE TABLE audit_log (
    seq         bigserial PRIMARY KEY,
    actor_id    text NOT NULL,
    action      text NOT NULL,
    target      text NOT NULL,
    payload     text NOT NULL,
    created_at  text NOT NULL,
    prev_hash   bytea NOT NULL,
    row_hash    bytea NOT NULL
);

CREATE INDEX ix_audit_created ON audit_log (created_at);


CREATE OR REPLACE FUNCTION forbid_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is forbidden', TG_TABLE_NAME, TG_OP;
END $$;

CREATE TRIGGER trg_entries_no_update
BEFORE UPDATE ON ledger_entries
FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TRIGGER trg_entries_no_delete
BEFORE DELETE ON ledger_entries
FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TRIGGER trg_tx_no_update
BEFORE UPDATE ON transactions
FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TRIGGER trg_tx_no_delete
BEFORE DELETE ON transactions
FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TRIGGER trg_audit_no_update
BEFORE UPDATE ON audit_log
FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TRIGGER trg_audit_no_delete
BEFORE DELETE ON audit_log
FOR EACH ROW EXECUTE FUNCTION forbid_mutation();


CREATE OR REPLACE FUNCTION assert_player_balance_non_negative() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    account_kind text;
BEGIN
    IF NEW.balance >= 0 THEN
        RETURN NEW;
    END IF;

    SELECT kind INTO account_kind FROM accounts WHERE id = NEW.account_id;
    IF account_kind IN ('player_main', 'player_locked', 'player_bonus') THEN
        RAISE EXCEPTION 'insufficient funds: player balance cannot be negative';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER trg_balance_non_negative_insert
BEFORE INSERT ON account_balances
FOR EACH ROW EXECUTE FUNCTION assert_player_balance_non_negative();

CREATE TRIGGER trg_balance_non_negative_update
BEFORE UPDATE ON account_balances
FOR EACH ROW EXECUTE FUNCTION assert_player_balance_non_negative();


DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ledger_service') THEN
        CREATE ROLE ledger_service NOLOGIN;
    END IF;
END $$;

REVOKE ALL ON SCHEMA finance FROM PUBLIC;
GRANT USAGE ON SCHEMA finance TO ledger_service;

GRANT SELECT, INSERT ON transactions, ledger_entries, audit_log TO ledger_service;
GRANT SELECT, INSERT, UPDATE ON account_balances TO ledger_service;
GRANT SELECT, INSERT ON accounts TO ledger_service;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA finance TO ledger_service;

REVOKE UPDATE, DELETE, TRUNCATE ON transactions, ledger_entries, audit_log FROM ledger_service;
COMMIT;
