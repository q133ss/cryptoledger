package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"

	ledgerv1 "cryptoledger/gen/ledger/v1"
	"cryptoledger/internal/hsm"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// VerifyIntegrity — то, что в проде запускается по расписанию (раз в час/ночь)
// и алертит, если что-то разошлось. Проверяет четыре независимых слоя:
//
//  1. Журнал: сумма проводок каждой операции равна нулю.
//  2. Подписи: канонический digest, пересчитанный из строк БД, совпадает с
//     тем, что подписал HSM.
//  3. Балансы: кэш account_balances совпадает с SUM(ledger_entries.amount).
//  4. Аудит: хеш-цепочка audit_log не разорвана.
//
// Каждая находка помечена тегом [audit], [signature], [double-entry], [balance],
// чтобы алерт сразу показывал, какой слой сломался.
func (s *Service) VerifyIntegrity(ctx context.Context, _ *ledgerv1.VerifyIntegrityRequest) (*ledgerv1.VerifyIntegrityResponse, error) {
	opts := &sql.TxOptions{ReadOnly: true}
	if s.store.postgres {
		opts.Isolation = sql.LevelRepeatableRead
	}
	tx, err := s.store.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "begin verification: %v", err)
	}
	defer tx.Rollback()
	resp := &ledgerv1.VerifyIntegrityResponse{}
	var problems []string

	add := func(format string, args ...any) {
		if len(problems) < 50 {
			problems = append(problems, fmt.Sprintf(format, args...))
		}
	}

	// --- 1. Хеш-цепочка аудита ----------------------------------------------
	type auditRow struct {
		Seq       int64
		ActorID   string
		Action    string
		Target    string
		Payload   string
		CreatedAt string
		PrevHash  []byte
		RowHash   []byte
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT seq, actor_id, action, target, payload, created_at, prev_hash, row_hash
		FROM audit_log ORDER BY seq`)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read audit log: %v", err)
	}
	var auditRows []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.Seq, &r.ActorID, &r.Action, &r.Target, &r.Payload,
			&r.CreatedAt, &r.PrevHash, &r.RowHash); err != nil {
			rows.Close()
			return nil, status.Errorf(codes.Internal, "scan audit log: %v", err)
		}
		auditRows = append(auditRows, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, status.Errorf(codes.Internal, "iterate audit log: %v", err)
	}
	rows.Close()

	// Генезис цепочки — 32 нулевых байта.
	audited := make(map[string]int)
	prev := make([]byte, 32)
	for _, r := range auditRows {
		resp.AuditRecordsChecked++
		if r.Action == "ledger.transaction.posted" {
			audited[strings.TrimPrefix(r.Target, "transaction:")]++
		}

		if !bytes.Equal(r.PrevHash, prev) {
			add("[audit] seq %d: prev_hash не совпадает с хешем предыдущей записи — строку удалили или вставили", r.Seq)
		}
		// Хеш пересчитываем из полей, а не доверяем сохранённому row_hash.
		want := auditRowHash(r.PrevHash, r.ActorID, r.Action, r.Target, r.Payload, r.CreatedAt)
		if !bytes.Equal(want, r.RowHash) {
			add("[audit] seq %d: row_hash не совпадает с содержимым записи — запись правили задним числом", r.Seq)
		}
		prev = r.RowHash
	}

	// --- 2. Проводки и подписи ----------------------------------------------
	type txRow struct {
		TxID           string
		IdempotencyKey string
		Operation      string
		Reference      string
		ActorID        string
		Note           string
		Currency       string
		Scale          int32
		Digest         []byte
		KeyID          string
		Signature      []byte
	}

	rows, err = tx.QueryContext(ctx, `
		SELECT tx_id, idempotency_key, operation, reference, actor_id, note,
		       currency, scale, digest, key_id, signature
		FROM transactions ORDER BY created_at, tx_id`)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read transactions: %v", err)
	}
	var txs []txRow
	for rows.Next() {
		var t txRow
		var scale int64
		if err := rows.Scan(&t.TxID, &t.IdempotencyKey, &t.Operation, &t.Reference, &t.ActorID,
			&t.Note, &t.Currency, &scale, &t.Digest, &t.KeyID, &t.Signature); err != nil {
			rows.Close()
			return nil, status.Errorf(codes.Internal, "scan transaction: %v", err)
		}
		t.Scale = int32(scale)
		txs = append(txs, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, status.Errorf(codes.Internal, "iterate transactions: %v", err)
	}
	rows.Close()

	publicKey := s.signer.PublicKey()
	for _, t := range txs {
		resp.TransactionsChecked++
		if audited[t.TxID] != 1 {
			add("[audit] tx %s: expected one posting event, found %d", t.TxID, audited[t.TxID])
		}
		delete(audited, t.TxID)

		entryRows, err := tx.QueryContext(ctx, `
			SELECT account_id, amount FROM ledger_entries
			WHERE tx_id = $1 ORDER BY account_id, seq`, t.TxID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "read entries of %s: %v", t.TxID, err)
		}
		var postings []posting
		var sum int64
		for entryRows.Next() {
			var p posting
			if err := entryRows.Scan(&p.AccountID, &p.Amount); err != nil {
				entryRows.Close()
				return nil, status.Errorf(codes.Internal, "scan entry of %s: %v", t.TxID, err)
			}
			postings = append(postings, p)
			var ok bool
			sum, ok = addInt64(sum, p.Amount)
			if !ok {
				entryRows.Close()
				return nil, status.Errorf(codes.Internal, "sum overflow in %s", t.TxID)
			}
		}
		if err := entryRows.Err(); err != nil {
			entryRows.Close()
			return nil, status.Errorf(codes.Internal, "iterate entries of %s: %v", t.TxID, err)
		}
		entryRows.Close()

		resp.EntriesChecked += int64(len(postings))

		if len(postings) < 2 {
			add("[double-entry] tx %s: в операции %d проводок, должно быть минимум 2", t.TxID, len(postings))
		}
		if sum != 0 {
			add("[double-entry] tx %s: сумма проводок = %d, деньги появились или исчезли", t.TxID, sum)
		}

		// Пересчитываем то, что подписывал HSM. Любая правка суммы, счёта,
		// ссылки или примечания меняет digest.
		want := txDigest(t.TxID, t.IdempotencyKey, t.Operation, t.Reference,
			t.ActorID, t.Note, t.Currency, t.Scale, postings)
		if !bytes.Equal(want, t.Digest) {
			add("[signature] tx %s: digest не совпадает с подписанным — данные операции изменили", t.TxID)
		}
		if t.KeyID != s.signer.KeyID() {
			add("[signature] tx %s: неизвестный key_id %q (ротация ключей не настроена)", t.TxID, t.KeyID)
		}
		if !hsm.Verify(publicKey, t.Digest, t.Signature) {
			add("[signature] tx %s: подпись HSM неверна", t.TxID)
		}
	}

	// --- 3. Кэш балансов против журнала --------------------------------------
	rows, err = tx.QueryContext(ctx, `
		SELECT a.id,
		       COALESCE(b.balance, 0),
		       COALESCE((SELECT SUM(e.amount) FROM ledger_entries e WHERE e.account_id = a.id), 0)
		FROM accounts a
		LEFT JOIN account_balances b ON b.account_id = a.id
		ORDER BY a.id`)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read balances: %v", err)
	}
	type balanceRow struct {
		ID      string
		Cached  int64
		Journal int64
	}
	var balanceRows []balanceRow
	for rows.Next() {
		var b balanceRow
		if err := rows.Scan(&b.ID, &b.Cached, &b.Journal); err != nil {
			rows.Close()
			return nil, status.Errorf(codes.Internal, "scan balance: %v", err)
		}
		balanceRows = append(balanceRows, b)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, status.Errorf(codes.Internal, "iterate balances: %v", err)
	}
	rows.Close()

	for _, b := range balanceRows {
		if b.Cached != b.Journal {
			add("[balance] account %s: кэш %d, журнал %d — расхождение", b.ID, b.Cached, b.Journal)
		}
	}

	for id := range audited {
		add("[audit] posting event references missing transaction %s", id)
	}
	resp.Problems = problems
	resp.Ok = len(problems) == 0
	return resp, nil
}
