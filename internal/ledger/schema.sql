-- Схема журнала. Три идеи, которые здесь зашиты:
--
-- 1. Баланса-как-поля не существует. Есть ledger_entries; balance в
--    account_balances — только кэш, который пересчитывается из журнала.
-- 2. Журнал append-only. UPDATE и DELETE запрещены триггерами (в Postgres —
--    ещё и REVOKE на уровне грантов, см. deploy/postgres/schema.sql).
-- 3. Никаких float. Суммы — целые в минимальных единицах валюты (scale счёта).
--    Знак суммы — изменение баланса счёта: плюс зачисление, минус списание.
--    В проде на Postgres это numeric(38,18), здесь int64 + явный scale.

PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS accounts (
    id         TEXT PRIMARY KEY,
    owner_id   TEXT NOT NULL,
    owner_type TEXT NOT NULL CHECK (owner_type IN ('player', 'company', 'house', 'bonus')),
    kind       TEXT NOT NULL CHECK (kind IN (
                   'player_main', 'player_locked', 'player_bonus',
                   'house_hot', 'house_equity', 'fee')),
    currency   TEXT NOT NULL,
    scale      INTEGER NOT NULL CHECK (scale BETWEEN 0 AND 18),
    created_at TEXT NOT NULL
);

-- Кэш баланса. Обновляется в той же транзакции, что и проводки.
-- Ночной сверочный джоб сверяет его с SUM(ledger_entries.amount).
CREATE TABLE IF NOT EXISTS account_balances (
    account_id TEXT PRIMARY KEY REFERENCES accounts(id),
    balance    INTEGER NOT NULL DEFAULT 0
);

-- Одна транзакция = группа проводок + ключ идемпотентности + подпись HSM.
CREATE TABLE IF NOT EXISTS transactions (
    tx_id           TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    request_hash    BLOB NOT NULL, -- SHA-256 тела запроса: ловит переиспользование ключа
    operation       TEXT NOT NULL,
    reference       TEXT NOT NULL,
    actor_id        TEXT NOT NULL,
    note            TEXT NOT NULL DEFAULT '',
    currency        TEXT NOT NULL,
    scale           INTEGER NOT NULL,
    digest          BLOB NOT NULL, -- что именно подписал HSM
    key_id          TEXT NOT NULL,
    signature       BLOB NOT NULL,
    created_at      TEXT NOT NULL
);

-- Сердце системы. Только INSERT.
CREATE TABLE IF NOT EXISTS ledger_entries (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    tx_id      TEXT NOT NULL REFERENCES transactions(tx_id),
    account_id TEXT NOT NULL REFERENCES accounts(id),
    amount     INTEGER NOT NULL CHECK (amount <> 0),
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS ix_entries_tx      ON ledger_entries(tx_id);
CREATE INDEX IF NOT EXISTS ix_entries_account ON ledger_entries(account_id, seq);

-- Аудит действий. Каждая строка хеширует предыдущую: правка рвёт цепочку.
CREATE TABLE IF NOT EXISTS audit_log (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    actor_id   TEXT NOT NULL,
    action     TEXT NOT NULL,
    target     TEXT NOT NULL,
    payload    TEXT NOT NULL, -- JSON «что произошло»
    created_at TEXT NOT NULL,
    prev_hash  BLOB NOT NULL,
    row_hash   BLOB NOT NULL
);

-- ---------------------------------------------------------------------------
-- Append-only: UPDATE/DELETE запрещены на уровне БД, а не «мы не пишем такой код».
-- ---------------------------------------------------------------------------

CREATE TRIGGER IF NOT EXISTS ledger_entries_no_update
BEFORE UPDATE ON ledger_entries
BEGIN
    SELECT RAISE(ABORT, 'ledger_entries is append-only: UPDATE is forbidden');
END;

CREATE TRIGGER IF NOT EXISTS ledger_entries_no_delete
BEFORE DELETE ON ledger_entries
BEGIN
    SELECT RAISE(ABORT, 'ledger_entries is append-only: DELETE is forbidden');
END;

CREATE TRIGGER IF NOT EXISTS transactions_no_update
BEFORE UPDATE ON transactions
BEGIN
    SELECT RAISE(ABORT, 'transactions is append-only: UPDATE is forbidden');
END;

CREATE TRIGGER IF NOT EXISTS transactions_no_delete
BEFORE DELETE ON transactions
BEGIN
    SELECT RAISE(ABORT, 'transactions is append-only: DELETE is forbidden');
END;

CREATE TRIGGER IF NOT EXISTS audit_log_no_update
BEFORE UPDATE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'audit_log is append-only: UPDATE is forbidden');
END;

CREATE TRIGGER IF NOT EXISTS audit_log_no_delete
BEFORE DELETE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'audit_log is append-only: DELETE is forbidden');
END;

-- ---------------------------------------------------------------------------
-- Инвариант «баланс игрока не уходит в минус» продублирован в БД.
-- Приложение проверяет его первым, чтобы вернуть понятную ошибку;
-- триггер — последняя линия обороны, если в коде появится баг.
-- ---------------------------------------------------------------------------

CREATE TRIGGER IF NOT EXISTS account_balances_non_negative_insert
BEFORE INSERT ON account_balances
WHEN NEW.balance < 0
     AND (SELECT kind FROM accounts WHERE id = NEW.account_id) IN
         ('player_main', 'player_locked', 'player_bonus')
BEGIN
    SELECT RAISE(ABORT, 'insufficient funds: player balance cannot be negative');
END;

CREATE TRIGGER IF NOT EXISTS account_balances_non_negative_update
BEFORE UPDATE OF balance ON account_balances
WHEN NEW.balance < 0
     AND (SELECT kind FROM accounts WHERE id = NEW.account_id) IN
         ('player_main', 'player_locked', 'player_bonus')
BEGIN
    SELECT RAISE(ABORT, 'insufficient funds: player balance cannot be negative');
END;
