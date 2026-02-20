package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// appendAudit добавляет запись в audit_log В ТОЙ ЖЕ транзакции БД, что и
// проводки. Либо деньги и запись о них появляются вместе, либо не появляется ничего.
func appendAudit(ctx context.Context, tx *sql.Tx, actorID, action, target, payload, createdAt string) ([]byte, error) {
	var prev []byte
	err := tx.QueryRowContext(ctx, `SELECT row_hash FROM audit_log ORDER BY seq DESC LIMIT 1`).Scan(&prev)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Генезис: 32 нулевых байта. Так у первой записи тоже есть prev_hash.
		prev = make([]byte, 32)
	case err != nil:
		return nil, fmt.Errorf("audit: read chain tip: %w", err)
	}

	rowHash := auditRowHash(prev, actorID, action, target, payload, createdAt)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_log (actor_id, action, target, payload, created_at, prev_hash, row_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		actorID, action, target, payload, createdAt, prev, rowHash); err != nil {
		return nil, fmt.Errorf("audit: append: %w", err)
	}
	return rowHash, nil
}

// txAuditPayload — что именно попадает в аудит. Формат стабилен, потому что
// строка целиком участвует в хеше.
type txAuditPayload struct {
	TxID           string         `json:"tx_id"`
	IdempotencyKey string         `json:"idempotency_key"`
	RequestHash    string         `json:"request_hash"`
	Operation      string         `json:"operation"`
	Reference      string         `json:"reference"`
	Currency       string         `json:"currency"`
	Scale          int32          `json:"scale"`
	Postings       []auditPosting `json:"postings"`
	Digest         string         `json:"digest"`
	KeyID          string         `json:"key_id"`
}

type auditPosting struct {
	AccountID string `json:"account_id"`
	Amount    int64  `json:"amount"`
}

func newTxAudit(rec *txRecord, postings []posting) txAuditPayload {
	out := txAuditPayload{
		TxID:           rec.TxID,
		IdempotencyKey: rec.IdempotencyKey,
		RequestHash:    fmt.Sprintf("%x", rec.RequestHash),
		Operation:      rec.Operation,
		Reference:      rec.Reference,
		Currency:       rec.Currency,
		Scale:          rec.Scale,
		Digest:         fmt.Sprintf("%x", rec.Digest),
		KeyID:          rec.KeyID,
	}
	for _, p := range postings {
		out.Postings = append(out.Postings, auditPosting{AccountID: p.AccountID, Amount: p.Amount})
	}
	return out
}
