-- Прод-вариант схемы финансового ядра для PostgreSQL 16.
--
-- Демо использует SQLite (internal/ledger/schema.sql), чтобы запускаться без
-- инфраструктуры. Здесь — та же модель, но с типами и правами, как в проде:
--   * numeric(38,18) вместо int64: 18 знаков после запятой, без float;
--   * timestamptz, uuid;
--   * отдельная роль ledger_service без UPDATE/DELETE на журнал;
--   * отложенная проверка SUM(amount)=0 на COMMIT;
--   * триггеры, запрещающие правку истории даже владельцу таблицы.
--
-- Выполнять от роли-владельца схемы (не от ledger_service).

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE SCHEMA IF NOT EXISTS finance;
SET search_path = finance, public;

-- ---------------------------------------------------------------------------
-- Счета и кэш баланса
-- ---------------------------------------------------------------------------

CREATE TABLE accounts (
    id          uuid PRIMARY KEY,
    owner_type  text NOT NULL CHECK (owner_type IN ('player', 'company', 'house', 'bonus')),
    owner_id    uuid NOT NULL,
    currency    text NOT NULL,
    kind        text NOT NULL CHECK (kind IN (
                    'player_main', 'player_locked', 'player_bonus',
                    'house_hot', 'house_equity', 'fee')),
    scale       smallint NOT NULL DEFAULT 18 CHECK (scale BETWEEN 0 AND 18),
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- Кэш для чтения. Источник истины — ledger_entries.
CREATE TABLE account_balances (
    account_id  uuid PRIMARY KEY REFERENCES accounts(id),
    balance     numeric(38, 18) NOT NULL DEFAULT 0,
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Операции и проводки
-- ---------------------------------------------------------------------------

CREATE TABLE transactions (
    tx_id            uuid PRIMARY KEY,
    idempotency_key  text NOT NULL UNIQUE,
    request_hash     bytea NOT NULL,      -- SHA-256 канонического тела запроса
    operation        text NOT NULL,
    reference        text NOT NULL,
    actor_id         uuid NOT NULL,       -- сервис или сотрудник
    note             text NOT NULL DEFAULT '',
    currency         text NOT NULL,
    scale            smallint NOT NULL,
    digest           bytea NOT NULL,      -- что подписал HSM
    key_id           text NOT NULL,
    signature        bytea NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE ledger_entries (
    seq         bigserial PRIMARY KEY,
    tx_id       uuid NOT NULL REFERENCES transactions(tx_id),
    account_id  uuid NOT NULL REFERENCES accounts(id),
    amount      numeric(38, 18) NOT NULL CHECK (amount <> 0),
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ix_entries_tx      ON ledger_entries (tx_id);
CREATE INDEX ix_entries_account ON ledger_entries (account_id, seq DESC);

-- ---------------------------------------------------------------------------
-- Инвариант двойной записи: проверяется на COMMIT, а не на каждой вставке.
-- Так операция может состоять из любого числа проводок, но сумма обязана быть 0.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION assert_transaction_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    total numeric(38, 18);
BEGIN
    SELECT COALESCE(SUM(amount), 0) INTO total
    FROM ledger_entries
    WHERE tx_id = NEW.tx_id;

    IF total <> 0 THEN
        RAISE EXCEPTION 'double-entry violation in tx %: sum = %', NEW.tx_id, total;
    END IF;
    RETURN NEW;
END $$;

CREATE CONSTRAINT TRIGGER trg_entries_balanced
AFTER INSERT ON ledger_entries
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_transaction_balanced();

-- ---------------------------------------------------------------------------
-- Аудит: append-only + хеш-цепочка.
-- prev_hash/row_hash считает сервис (тот же canonical encoder, что и для
-- подписи) внутри транзакции под advisory lock:
--   SELECT pg_advisory_xact_lock(hashtext('finance.audit_log'));
-- Это гарантирует одного писателя и линейную цепочку.
-- ---------------------------------------------------------------------------

CREATE TABLE audit_log (
    seq         bigserial PRIMARY KEY,
    actor_id    uuid NOT NULL,
    action      text NOT NULL,
    target_type text NOT NULL,
    target_id   text NOT NULL,
    payload     jsonb NOT NULL,
    ip          inet,
    user_agent  text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    prev_hash   bytea NOT NULL,
    row_hash    bytea NOT NULL
);

CREATE INDEX ix_audit_created ON audit_log (created_at);

-- ---------------------------------------------------------------------------
-- Запрет изменения истории. Даже если роли случайно выдадут UPDATE,
-- триггер не даст переписать проводку.
-- ---------------------------------------------------------------------------

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

-- ---------------------------------------------------------------------------
-- Баланс игрока не уходит в минус. Приложение проверяет первым, чтобы вернуть
-- понятную gRPC-ошибку; триггер — последняя линия обороны.
-- ---------------------------------------------------------------------------

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

-- ---------------------------------------------------------------------------
-- Права. У сервиса нет UPDATE/DELETE на историю — только INSERT и SELECT.
-- ---------------------------------------------------------------------------

-- Роль сервиса создаётся один раз; повторный запуск скрипта не падает.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ledger_service') THEN
        CREATE ROLE ledger_service LOGIN;
    END IF;
END $$;

REVOKE ALL ON SCHEMA finance FROM PUBLIC;
GRANT USAGE ON SCHEMA finance TO ledger_service;

GRANT SELECT, INSERT ON transactions, ledger_entries, audit_log TO ledger_service;
GRANT SELECT, INSERT, UPDATE ON account_balances TO ledger_service;
GRANT SELECT, INSERT ON accounts TO ledger_service;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA finance TO ledger_service;

-- Явно отзываем то, что могло достаться по наследству от PUBLIC/шаблонов.
REVOKE UPDATE, DELETE, TRUNCATE ON transactions, ledger_entries, audit_log FROM ledger_service;

-- ---------------------------------------------------------------------------
-- Ночная сверка: кэш против журнала. Пусто = всё сходится.
-- В проде результат уходит в Prometheus-метрику и алертит при любом расхождении
-- хотя бы на один младший разряд.
-- ---------------------------------------------------------------------------

-- SELECT a.id,
--        b.balance                       AS cached,
--        COALESCE(SUM(e.amount), 0)      AS journal
-- FROM accounts a
-- LEFT JOIN account_balances b ON b.account_id = a.id
-- LEFT JOIN ledger_entries  e ON e.account_id = a.id
-- GROUP BY a.id, b.balance
-- HAVING b.balance IS DISTINCT FROM COALESCE(SUM(e.amount), 0);
