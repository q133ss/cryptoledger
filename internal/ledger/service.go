package ledger

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	ledgerv1 "cryptoledger/gen/ledger/v1"
	"cryptoledger/internal/hsm"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service реализует LedgerService. Это единственный код, который пишет
// в transactions / ledger_entries / account_balances.
type Service struct {
	ledgerv1.UnimplementedLedgerServiceServer

	store  *Store
	signer hsm.Signer
	now    func() time.Time
	log    *slog.Logger
}

func NewService(store *Store, signer hsm.Signer, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		store:  store,
		signer: signer,
		now:    func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) },
		log:    log,
	}
}

// PostTransaction — точка, где сходятся все инварианты.
//
// The mTLS transport supplies the authenticated actor before this handler runs.
//
// Порядок важен:
//  1. дешёвая валидация до похода в базу;
//  2. одна транзакция БД: проверка идемпотентности, балансов, вставка проводок,
//     обновление кэша балансов и запись аудита;
//  3. подпись HSM до COMMIT: неподписанная операция не станет историей;
//  4. COMMIT — единственный момент, когда деньги реально «переехали».
func (s *Service) PostTransaction(ctx context.Context, req *ledgerv1.PostTransactionRequest) (*ledgerv1.PostTransactionResponse, error) {
	if err := validatePostRequest(req); err != nil {
		return nil, err
	}

	postings := sortedPostings(req.Postings)
	operation := opCode(req.Operation)
	reqHash := requestHash(operation, req.Reference, postings)

	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "begin transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	// --- Идемпотентность -----------------------------------------------------
	if s.store.postgres {
		// Serialize retries before reading balances, including a retry after the first debit exhausts an account.
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(1, hashtext($1))", req.IdempotencyKey); err != nil {
			return nil, status.Errorf(codes.Unavailable, "lock idempotency key: %v", err)
		}
	}
	existing, err := loadTxByKey(ctx, tx, req.IdempotencyKey)
	switch {
	case err == nil:
		// Ключ уже использован. Если тело то же — это ретрай: возвращаем
		// исходный результат, ничего не проводя повторно.
		if !bytes.Equal(existing.RequestHash, reqHash) || existing.ActorID != req.ActorId || existing.Note != req.Note {
			return nil, status.Errorf(codes.FailedPrecondition,
				"idempotency key %q is already used by tx %s with a different payload",
				req.IdempotencyKey, existing.TxID)
		}
		resp, err := s.responseForTx(ctx, tx, existing, true)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, status.Errorf(codes.Internal, "commit replay: %v", err)
		}
		s.log.Info("idempotent replay", "tx_id", existing.TxID, "key", req.IdempotencyKey)
		return resp, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, status.Errorf(codes.Internal, "lookup idempotency key: %v", err)
	}

	// --- Счета и валюта ------------------------------------------------------
	accounts, err := loadAccounts(ctx, tx, postings)
	if err != nil {
		return nil, err
	}
	currency, scale, err := singleCurrency(accounts, postings)
	if err != nil {
		return nil, err
	}

	// --- Балансы: проверка «не в минус» в той же транзакции ------------------
	deltas := make(map[string]int64, len(accounts))
	for _, p := range postings {
		// Дубли счетов отсеяны валидацией, поэтому здесь всегда одна запись
		// на счёт. += оставлен как страховка, если правило когда-нибудь смягчат.
		deltas[p.AccountID] += p.Amount
	}
	newBalances := make(map[string]int64, len(accounts))
	for _, id := range sortedIDs(accounts) {
		current, err := cachedBalance(ctx, tx, id, s.store.postgres)
		if err != nil {
			return nil, err
		}
		next, ok := addInt64(current, deltas[id])
		if !ok {
			return nil, status.Errorf(codes.Internal, "balance overflow on account %s", id)
		}
		if isPlayerKind(accounts[id].Kind) && next < 0 {
			return nil, status.Errorf(codes.FailedPrecondition,
				"insufficient funds on account %s: balance %s, debit %s",
				id, FormatMoney(current, scale, currency), FormatMoney(-deltas[id], scale, currency))
		}
		newBalances[id] = next
	}

	// --- Подпись HSM ---------------------------------------------------------
	now := s.now()
	txID, err := newID("tx_")
	if err != nil {
		return nil, status.Error(codes.Internal, "cannot allocate transaction ID")
	}
	digest := txDigest(txID, req.IdempotencyKey, operation, req.Reference, req.ActorId, req.Note, currency, scale, postings)

	signature, err := s.signer.Sign(ctx, digest)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "hsm sign: %v", err)
	}

	rec := &txRecord{
		TxID:           txID,
		IdempotencyKey: req.IdempotencyKey,
		RequestHash:    reqHash,
		Operation:      operation,
		Reference:      req.Reference,
		ActorID:        req.ActorId,
		Note:           req.Note,
		Currency:       currency,
		Scale:          scale,
		Digest:         digest,
		KeyID:          s.signer.KeyID(),
		Signature:      signature,
		CreatedAt:      now,
	}

	// --- Запись --------------------------------------------------------------
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO transactions
			(tx_id, idempotency_key, request_hash, operation, reference, actor_id,
			 note, currency, scale, digest, key_id, signature, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		rec.TxID, rec.IdempotencyKey, rec.RequestHash, rec.Operation, rec.Reference,
		rec.ActorID, rec.Note, rec.Currency, rec.Scale, rec.Digest, rec.KeyID,
		rec.Signature, formatTime(rec.CreatedAt)); err != nil {
		if isUniqueViolation(err) {
			// Гонка между процессами: ключ успел записать другой инстанс.
			_ = tx.Rollback()
			return s.replayCommitted(ctx, req, reqHash)
		}
		return nil, status.Errorf(codes.Internal, "insert transaction: %v", err)
	}

	for _, p := range postings {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ledger_entries (tx_id, account_id, amount, created_at)
			VALUES ($1, $2, $3, $4)`,
			rec.TxID, p.AccountID, p.Amount, formatTime(rec.CreatedAt)); err != nil {
			return nil, status.Errorf(codes.Internal, "insert entry: %v", err)
		}
	}

	for _, id := range sortedIDs(accounts) {
		if _, err := tx.ExecContext(ctx,
			`UPDATE account_balances SET balance = $1 WHERE account_id = $2`,
			newBalances[id], id); err != nil {
			return nil, status.Errorf(codes.Internal, "update balance %s: %v", id, err)
		}
	}

	auditPayload, err := json.Marshal(newTxAudit(rec, postings))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal audit payload: %v", err)
	}
	if s.store.postgres {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(2, 0)"); err != nil {
			return nil, status.Errorf(codes.Internal, "lock audit chain: %v", err)
		}
	}
	if _, err := appendAudit(ctx, tx, rec.ActorID, "ledger.transaction.posted",
		"transaction:"+rec.TxID, string(auditPayload), formatTime(rec.CreatedAt)); err != nil {
		return nil, status.Errorf(codes.Internal, "append audit: %v", err)
	}

	resp, err := s.responseForTx(ctx, tx, rec, false)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, status.Errorf(codes.Internal, "commit: %v", err)
	}

	s.log.Info("transaction posted",
		"tx_id", rec.TxID, "operation", rec.Operation, "reference", rec.Reference)

	return resp, nil
}

// replayCommitted — путь для гонки: другой процесс уже провёл операцию с этим
// ключом. Читаем её в новой транзакции и отдаём как обычно.
func (s *Service) replayCommitted(ctx context.Context, req *ledgerv1.PostTransactionRequest, reqHash []byte) (*ledgerv1.PostTransactionResponse, error) {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "begin replay: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	key := req.IdempotencyKey
	rec, err := loadTxByKey(ctx, tx, key)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "concurrent retry conflict, retry the request")
	}
	if !bytes.Equal(rec.RequestHash, reqHash) || rec.ActorID != req.ActorId || rec.Note != req.Note {
		return nil, status.Errorf(codes.FailedPrecondition,
			"idempotency key %q is already used with a different payload", key)
	}
	resp, err := s.responseForTx(ctx, tx, rec, true)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, status.Errorf(codes.Internal, "commit replay: %v", err)
	}
	return resp, nil
}

// responseForTx собирает ответ из журнала, а не из запроса: при ретрае клиент
// получает те же tx_id, подпись и балансы, что и в первый раз.
func (s *Service) responseForTx(ctx context.Context, tx *sql.Tx, rec *txRecord, deduplicated bool) (*ledgerv1.PostTransactionResponse, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT account_id, SUM(amount)
		FROM ledger_entries
		WHERE tx_id = $1
		GROUP BY account_id
		ORDER BY account_id`, rec.TxID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list tx entries: %v", err)
	}
	var accountIDs []string
	for rows.Next() {
		var id string
		var sum int64
		if err := rows.Scan(&id, &sum); err != nil {
			rows.Close()
			return nil, status.Errorf(codes.Internal, "scan tx entries: %v", err)
		}
		accountIDs = append(accountIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, status.Errorf(codes.Internal, "iterate tx entries: %v", err)
	}
	rows.Close()

	balances := make([]*ledgerv1.AccountBalance, 0, len(accountIDs))
	for _, id := range accountIDs {
		b, err := accountWithCachedBalance(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if deduplicated {
			err = tx.QueryRowContext(ctx, `SELECT SUM(amount) FROM ledger_entries WHERE account_id = $1 AND seq <=
     (SELECT MAX(seq) FROM ledger_entries WHERE tx_id = $2 AND account_id = $1)`, id, rec.TxID).Scan(&b.Balance)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "read posted balance: %v", err)
			}
		}
		balances = append(balances, b)
	}

	return &ledgerv1.PostTransactionResponse{
		TxId:         rec.TxID,
		Deduplicated: deduplicated,
		CommittedAt:  timestamppb.New(rec.CreatedAt),
		Signature: &ledgerv1.Signature{
			Algorithm: "ed25519",
			KeyId:     rec.KeyID,
			Value:     rec.Signature,
			Digest:    rec.Digest,
		},
		Balances: balances,
	}, nil
}

func (s *Service) GetAccount(ctx context.Context, req *ledgerv1.GetAccountRequest) (*ledgerv1.GetAccountResponse, error) {
	if req.GetAccountId() == "" {
		return nil, status.Error(codes.InvalidArgument, "account_id is required")
	}

	// Читаем баланс из журнала, а не из кэша: это демонстрация главного
	// инварианта — баланс ЕСТЬ сумма проводок. В проде на горячем пути
	// читается account_balances, а сверка с журналом идёт ночью.
	row := s.store.db.QueryRowContext(ctx, `
		SELECT a.id, a.owner_id, a.kind, a.currency, a.scale,
		       COALESCE((SELECT SUM(e.amount) FROM ledger_entries e WHERE e.account_id = a.id), 0)
		FROM accounts a
		WHERE a.id = $1`, req.GetAccountId())

	var id, ownerID, kind, currency string
	var scale int32
	var balance int64
	if err := row.Scan(&id, &ownerID, &kind, &currency, &scale, &balance); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "account %q not found", req.GetAccountId())
		}
		return nil, status.Errorf(codes.Internal, "load account: %v", err)
	}

	return &ledgerv1.GetAccountResponse{Account: &ledgerv1.AccountBalance{
		AccountId: id,
		OwnerId:   ownerID,
		Kind:      kindFromCode(kind),
		Currency:  currency,
		Balance:   balance,
		Scale:     scale,
	}}, nil
}

func (s *Service) ListEntries(ctx context.Context, req *ledgerv1.ListEntriesRequest) (*ledgerv1.ListEntriesResponse, error) {
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	query := `
		SELECT e.seq, e.tx_id, e.account_id, e.amount,
		       t.operation, t.reference, t.idempotency_key, e.created_at
		FROM ledger_entries e
		JOIN transactions t ON t.tx_id = e.tx_id`
	args := []any{}
	if id := req.GetAccountId(); id != "" {
		query += ` WHERE e.account_id = $1`
		args = append(args, id)
	}
	query += fmt.Sprintf(" ORDER BY e.seq DESC LIMIT $%d", len(args)+1)
	args = append(args, limit)

	rows, err := s.store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list entries: %v", err)
	}
	defer rows.Close()

	resp := &ledgerv1.ListEntriesResponse{}
	for rows.Next() {
		var (
			entry   ledgerv1.LedgerEntry
			op      string
			created string
		)
		if err := rows.Scan(&entry.Seq, &entry.TxId, &entry.AccountId, &entry.Amount,
			&op, &entry.Reference, &entry.IdempotencyKey, &created); err != nil {
			return nil, status.Errorf(codes.Internal, "scan entry: %v", err)
		}
		entry.Operation = opFromCode(op)
		at, err := parseTime(created)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "parse entry time: %v", err)
		}
		entry.CreatedAt = timestamppb.New(at)
		resp.Entries = append(resp.Entries, &entry)
	}
	if err := rows.Err(); err != nil {
		return nil, status.Errorf(codes.Internal, "iterate entries: %v", err)
	}
	return resp, nil
}

// --- валидация ---------------------------------------------------------------

func validatePostRequest(req *ledgerv1.PostTransactionRequest) error {
	if req == nil {
		return status.Error(codes.InvalidArgument, "request is nil")
	}
	if n := len(req.GetIdempotencyKey()); n == 0 || n > 128 {
		return status.Error(codes.InvalidArgument, "idempotency_key must be 1..128 characters")
	}
	if n := len(req.GetReference()); n == 0 || n > 256 {
		return status.Error(codes.InvalidArgument, "reference must be 1..256 characters")
	}
	if n := len(req.GetActorId()); n == 0 || n > 128 {
		return status.Error(codes.InvalidArgument, "actor_id must be 1..128 characters")
	}
	if len(req.GetNote()) > 1024 {
		return status.Error(codes.InvalidArgument, "note is too long (max 1024)")
	}
	if req.GetOperation() == ledgerv1.Operation_OPERATION_UNSPECIFIED || opCode(req.GetOperation()) == "" {
		return status.Error(codes.InvalidArgument, "operation is required")
	}
	if len(req.GetPostings()) < 2 {
		return status.Error(codes.InvalidArgument, "double-entry requires at least two postings")
	}
	if len(req.GetPostings()) > 64 {
		return status.Error(codes.InvalidArgument, "too many postings (max 64)")
	}

	seen := make(map[string]struct{}, len(req.GetPostings()))
	var sum int64
	for _, p := range req.GetPostings() {
		if p.GetAccountId() == "" {
			return status.Error(codes.InvalidArgument, "posting account_id is required")
		}
		if _, ok := seen[p.GetAccountId()]; ok {
			return status.Errorf(codes.InvalidArgument, "account %q appears in more than one posting", p.GetAccountId())
		}
		seen[p.GetAccountId()] = struct{}{}

		amount := p.GetAmount()
		if amount == 0 {
			return status.Errorf(codes.InvalidArgument, "posting for %q has zero amount", p.GetAccountId())
		}
		if amount > maxAbsAmount || amount < -maxAbsAmount {
			return status.Errorf(codes.InvalidArgument, "posting amount for %q is out of range", p.GetAccountId())
		}

		var ok bool
		sum, ok = addInt64(sum, amount)
		if !ok {
			return status.Error(codes.InvalidArgument, "postings sum overflows int64")
		}
	}
	if sum != 0 {
		return status.Errorf(codes.InvalidArgument,
			"double-entry violation: postings sum to %d, must be 0", sum)
	}
	return nil
}

func loadAccounts(ctx context.Context, tx *sql.Tx, postings []posting) (map[string]Account, error) {
	out := make(map[string]Account, len(postings))
	for _, p := range postings {
		if _, ok := out[p.AccountID]; ok {
			continue
		}
		var a Account
		err := tx.QueryRowContext(ctx, `
			SELECT id, owner_id, owner_type, kind, currency, scale
			FROM accounts WHERE id = $1`, p.AccountID).
			Scan(&a.ID, &a.OwnerID, &a.OwnerType, &a.Kind, &a.Currency, &a.Scale)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "account %q not found", p.AccountID)
		}
		if err != nil {
			return nil, status.Errorf(codes.Internal, "load account %q: %v", p.AccountID, err)
		}
		out[a.ID] = a
	}
	return out, nil
}

func singleCurrency(accounts map[string]Account, postings []posting) (string, int32, error) {
	first := accounts[postings[0].AccountID]
	for _, p := range postings[1:] {
		a := accounts[p.AccountID]
		if a.Currency != first.Currency || a.Scale != first.Scale {
			return "", 0, status.Errorf(codes.InvalidArgument,
				"accounts %s (%s, scale %d) and %s (%s, scale %d) use different currencies",
				first.ID, first.Currency, first.Scale, a.ID, a.Currency, a.Scale)
		}
	}
	return first.Currency, first.Scale, nil
}

func isPlayerKind(kind string) bool {
	switch kind {
	case "player_main", "player_locked", "player_bonus":
		return true
	}
	return false
}

func sortedIDs(accounts map[string]Account) []string {
	ids := make([]string, 0, len(accounts))
	for id := range accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// --- чтение ------------------------------------------------------------------

const txColumns = `tx_id, idempotency_key, request_hash, operation, reference,
	actor_id, note, currency, scale, digest, key_id, signature, created_at`

type txRecord struct {
	TxID           string
	IdempotencyKey string
	RequestHash    []byte
	Operation      string
	Reference      string
	ActorID        string
	Note           string
	Currency       string
	Scale          int32
	Digest         []byte
	KeyID          string
	Signature      []byte
	CreatedAt      time.Time
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTxRow(sc rowScanner) (*txRecord, error) {
	var (
		r         txRecord
		scale     int64
		createdAt string
	)
	err := sc.Scan(&r.TxID, &r.IdempotencyKey, &r.RequestHash, &r.Operation, &r.Reference,
		&r.ActorID, &r.Note, &r.Currency, &scale, &r.Digest, &r.KeyID, &r.Signature, &createdAt)
	if err != nil {
		return nil, err
	}
	at, err := parseTime(createdAt)
	if err != nil {
		return nil, err
	}
	r.Scale = int32(scale)
	r.CreatedAt = at
	return &r, nil
}

func loadTxByKey(ctx context.Context, tx *sql.Tx, key string) (*txRecord, error) {
	return scanTxRow(tx.QueryRowContext(ctx,
		`SELECT `+txColumns+` FROM transactions WHERE idempotency_key = $1`, key))
}

func cachedBalance(ctx context.Context, tx *sql.Tx, accountID string, lock bool) (int64, error) {
	var balance int64
	query := "SELECT balance FROM account_balances WHERE account_id = $1"
	if lock {
		query += " FOR UPDATE"
	}
	err := tx.QueryRowContext(ctx, query, accountID).Scan(&balance)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, status.Errorf(codes.Internal, "balance row for account %s is missing", accountID)
	}
	if err != nil {
		return 0, status.Errorf(codes.Internal, "load balance %s: %v", accountID, err)
	}
	return balance, nil
}

func accountWithCachedBalance(ctx context.Context, tx *sql.Tx, accountID string) (*ledgerv1.AccountBalance, error) {
	var (
		id, ownerID, kind, currency string
		scale                       int32
		balance                     int64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT a.id, a.owner_id, a.kind, a.currency, a.scale, b.balance
		FROM accounts a
		JOIN account_balances b ON b.account_id = a.id
		WHERE a.id = $1`, accountID).
		Scan(&id, &ownerID, &kind, &currency, &scale, &balance)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Errorf(codes.NotFound, "account %q not found", accountID)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load account %s: %v", accountID, err)
	}
	return &ledgerv1.AccountBalance{
		AccountId: id,
		OwnerId:   ownerID,
		Kind:      kindFromCode(kind),
		Currency:  currency,
		Balance:   balance,
		Scale:     scale,
	}, nil
}

// newID — идентификатор операции. CSPRNG, а не счётчик: последовательность
// угадывается и позволяет предугадывать id соседних операций.
func newID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Отказ CSPRNG — аварийная ситуация: продолжать финансовые операции нельзя.
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}
