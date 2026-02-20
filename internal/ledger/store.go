package ledger

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // чистый Go, без cgo: собирается на Windows без gcc
)

//go:embed schema.sql
var schemaSQL string

// Store — доступ к базе журнала.
//
// В проде это PostgreSQL: отдельный кластер finance, отдельная сеть и учётка
// (см. deploy/postgres/schema.sql). Здесь SQLite, чтобы демо запускалось без
// docker-compose; SQL и инварианты те же самые.
type Store struct {
	db        *sql.DB
	postgres  bool
	closePool func()
}

// Open открывает базу по пути и применяет схему (идемпотентно).
func Open(path string) (*Store, error) {
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	// Один writer. В проде за журнал отвечает один сервис, а параллелизм
	// внутри него ограничен транзакциями БД. Для демо это ещё и убирает
	// гонки SQLite между соединениями.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: wal: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	err := s.db.Close()
	if s.closePool != nil {
		s.closePool()
	}
	return err
}

// DB отдаёт низкоуровневый доступ. Нужен тестам и демо, чтобы сыграть роль
// DBA с прямым доступом к базе (в проде таких прав у сервиса нет).
func (s *Store) DB() *sql.DB { return s.db }

// Account — счёт в плане счетов.
type Account struct {
	ID        string
	OwnerID   string
	OwnerType string // player | company | house | bonus
	Kind      string // player_main | player_locked | player_bonus | house_hot | house_equity | fee
	Currency  string
	Scale     int32
}

// CreateAccount заводит счёт и нулевой кэш баланса.
// Проводок здесь нет: счёт без движения денег — это нормально.
func (s *Store) CreateAccount(ctx context.Context, a Account) error {
	if a.ID == "" || a.OwnerID == "" || a.OwnerType == "" || a.Kind == "" || a.Currency == "" {
		return fmt.Errorf("store: account has empty required fields: %+v", a)
	}
	if a.Scale < 0 || a.Scale > 18 {
		return fmt.Errorf("store: account %s has invalid scale %d", a.ID, a.Scale)
	}
	if _, ok := dbToKind[a.Kind]; !ok {
		return fmt.Errorf("store: account %s has unknown kind %q", a.ID, a.Kind)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()

	now := formatTime(time.Now())
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO accounts (id, owner_id, owner_type, kind, currency, scale, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT(id) DO NOTHING`,
		a.ID, a.OwnerID, a.OwnerType, a.Kind, a.Currency, a.Scale, now); err != nil {
		return fmt.Errorf("store: insert account: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO account_balances (account_id, balance) VALUES ($1, 0)
		ON CONFLICT(account_id) DO NOTHING`, a.ID); err != nil {
		return fmt.Errorf("store: insert balance: %w", err)
	}
	return tx.Commit()
}

// formatTime — единый формат хранения времени. Строка, а не unix-число:
// дамп базы читается глазами, а сравнение идёт по байтам.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", s, err)
	}
	return t, nil
}

// isUniqueViolation нужен для гонки двух одинаковых запросов, пришедших
// одновременно в разные процессы: UNIQUE(idempotency_key) решает спор.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
