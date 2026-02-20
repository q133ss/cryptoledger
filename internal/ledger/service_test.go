package ledger

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	ledgerv1 "cryptoledger/gen/ledger/v1"
	"cryptoledger/internal/hsm"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testUnit = int64(1_000_000)

const (
	testPlayerMain = "player_main_1"
	testHouseHot   = "house_hot"
	testHouseEq    = "house_equity"
)

func newTestEnv(t *testing.T) (*Service, *Store) {
	t.Helper()

	store, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	signer, err := hsm.NewSoftHSM("test-hsm:v1", bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("hsm: %v", err)
	}

	svc := NewService(store, signer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	for _, a := range []Account{
		{ID: testHouseHot, OwnerID: "house", OwnerType: "house", Kind: "house_hot", Currency: "USDT", Scale: 6},
		{ID: testHouseEq, OwnerID: "house", OwnerType: "house", Kind: "house_equity", Currency: "USDT", Scale: 6},
		{ID: testPlayerMain, OwnerID: "player", OwnerType: "player", Kind: "player_main", Currency: "USDT", Scale: 6},
	} {
		if err := store.CreateAccount(ctx, a); err != nil {
			t.Fatalf("create account %s: %v", a.ID, err)
		}
	}

	fund := &ledgerv1.PostTransactionRequest{
		IdempotencyKey: "test:funding",
		Reference:      "seed",
		Operation:      ledgerv1.Operation_OPERATION_ADJUST,
		ActorId:        "test",
		Postings: []*ledgerv1.Posting{
			{AccountId: testHouseHot, Amount: 1000 * testUnit},
			{AccountId: testHouseEq, Amount: -1000 * testUnit},
		},
	}
	if _, err := svc.PostTransaction(ctx, fund); err != nil {
		t.Fatalf("funding: %v", err)
	}
	return svc, store
}

func deposit(key string, amount int64) *ledgerv1.PostTransactionRequest {
	return &ledgerv1.PostTransactionRequest{
		IdempotencyKey: key,
		Reference:      "deposit:" + key,
		Operation:      ledgerv1.Operation_OPERATION_DEPOSIT,
		ActorId:        "deposit-watcher",
		Postings: []*ledgerv1.Posting{
			{AccountId: testHouseHot, Amount: -amount},
			{AccountId: testPlayerMain, Amount: amount},
		},
	}
}

func mustPost(t *testing.T, svc *Service, req *ledgerv1.PostTransactionRequest) *ledgerv1.PostTransactionResponse {
	t.Helper()
	resp, err := svc.PostTransaction(context.Background(), req)
	if err != nil {
		t.Fatalf("post %s: %v", req.IdempotencyKey, err)
	}
	return resp
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got nil", want)
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected grpc status, got %v", err)
	}
	if st.Code() != want {
		t.Fatalf("expected %s, got %s: %s", want, st.Code(), st.Message())
	}
}

func playerBalance(t *testing.T, svc *Service) int64 {
	t.Helper()
	resp, err := svc.GetAccount(context.Background(), &ledgerv1.GetAccountRequest{AccountId: testPlayerMain})
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	return resp.Account.Balance
}

func TestDoubleEntryMovesMoney(t *testing.T) {
	svc, _ := newTestEnv(t)
	mustPost(t, svc, deposit("d1", 250*testUnit))

	if got := playerBalance(t, svc); got != 250*testUnit {
		t.Fatalf("player balance = %d, want %d", got, 250*testUnit)
	}

	check, err := svc.VerifyIntegrity(context.Background(), &ledgerv1.VerifyIntegrityRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !check.Ok {
		t.Fatalf("integrity broken: %v", check.Problems)
	}
	if check.EntriesChecked != 4 { // 2 фондирование + 2 депозит
		t.Fatalf("entries checked = %d, want 4", check.EntriesChecked)
	}
}

func TestIdempotentReplayDoesNotDoubleSpend(t *testing.T) {
	svc, store := newTestEnv(t)
	req := deposit("d2", 250*testUnit)
	first := mustPost(t, svc, req)

	// Ретрай в другом порядке проводок — для клиента это тот же запрос.
	replay := deposit("d2", 250*testUnit)
	replay.Postings[0], replay.Postings[1] = replay.Postings[1], replay.Postings[0]
	second := mustPost(t, svc, replay)

	if second.TxId != first.TxId {
		t.Fatalf("replay created new tx %s, want %s", second.TxId, first.TxId)
	}
	if !second.Deduplicated {
		t.Fatal("replay is not marked deduplicated")
	}
	if got := playerBalance(t, svc); got != 250*testUnit {
		t.Fatalf("balance after replay = %d, want %d", got, 250*testUnit)
	}

	var count int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM ledger_entries WHERE tx_id = ?`, first.TxId).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("entries for tx = %d, want 2", count)
	}
}

func TestIdempotencyKeyReuseWithDifferentPayload(t *testing.T) {
	svc, _ := newTestEnv(t)
	mustPost(t, svc, deposit("d3", 250*testUnit))

	_, err := svc.PostTransaction(context.Background(), deposit("d3", 251*testUnit))
	wantCode(t, err, codes.FailedPrecondition)

	if got := playerBalance(t, svc); got != 250*testUnit {
		t.Fatalf("balance changed after rejected conflict: %d", got)
	}
}

func TestUnbalancedPostingRejected(t *testing.T) {
	svc, store := newTestEnv(t)

	_, err := svc.PostTransaction(context.Background(), &ledgerv1.PostTransactionRequest{
		IdempotencyKey: "broken",
		Reference:      "broken",
		Operation:      ledgerv1.Operation_OPERATION_DEPOSIT,
		ActorId:        "test",
		Postings: []*ledgerv1.Posting{
			{AccountId: testHouseHot, Amount: 100 * testUnit},
			{AccountId: testPlayerMain, Amount: -90 * testUnit},
		},
	})
	wantCode(t, err, codes.InvalidArgument)

	var count int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM ledger_entries WHERE tx_id <> ''`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 { // только фондирование
		t.Fatalf("entries = %d, want 2 (unbalanced must not be written)", count)
	}
}

func TestInsufficientFundsRejected(t *testing.T) {
	svc, _ := newTestEnv(t)
	mustPost(t, svc, deposit("d4", 100*testUnit))

	_, err := svc.PostTransaction(context.Background(), &ledgerv1.PostTransactionRequest{
		IdempotencyKey: "bet-too-big",
		Reference:      "game_round:1",
		Operation:      ledgerv1.Operation_OPERATION_BET,
		ActorId:        "game",
		Postings: []*ledgerv1.Posting{
			{AccountId: testPlayerMain, Amount: -150 * testUnit},
			{AccountId: testHouseEq, Amount: 150 * testUnit},
		},
	})
	wantCode(t, err, codes.FailedPrecondition)

	if got := playerBalance(t, svc); got != 100*testUnit {
		t.Fatalf("balance = %d, want 100", got)
	}
}

func TestBalanceTriggerIsLastLineOfDefense(t *testing.T) {
	_, store := newTestEnv(t)

	// Даже если приложение пропустит проверку, БД не даст увести счёт игрока в минус.
	_, err := store.DB().Exec(`UPDATE account_balances SET balance = -1 WHERE account_id = ?`, testPlayerMain)
	if err == nil {
		t.Fatal("database allowed negative player balance")
	}
	if !strings.Contains(err.Error(), "insufficient funds") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAppendOnlyTriggers(t *testing.T) {
	_, store := newTestEnv(t)

	if _, err := store.DB().Exec(`UPDATE ledger_entries SET amount = amount + 1 WHERE seq = 1`); err == nil {
		t.Fatal("UPDATE on ledger_entries was allowed")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("unexpected UPDATE error: %v", err)
	}

	if _, err := store.DB().Exec(`DELETE FROM ledger_entries WHERE seq = 1`); err == nil {
		t.Fatal("DELETE on ledger_entries was allowed")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("unexpected DELETE error: %v", err)
	}

	if _, err := store.DB().Exec(`UPDATE audit_log SET payload = 'x'`); err == nil {
		t.Fatal("UPDATE on audit_log was allowed")
	}
}

func TestTamperingIsDetected(t *testing.T) {
	svc, store := newTestEnv(t)
	mustPost(t, svc, deposit("d5", 250*testUnit))

	// Атакующий с правами на БД обходит триггеры.
	for _, q := range []string{
		`DROP TRIGGER ledger_entries_no_update`,
		`UPDATE ledger_entries SET amount = amount + 50 * 1000000 WHERE seq = 1`,
		`DROP TRIGGER audit_log_no_update`,
		`UPDATE audit_log SET payload = payload || ' ' WHERE seq = (SELECT MAX(seq) FROM audit_log)`,
	} {
		if _, err := store.DB().Exec(q); err != nil {
			t.Fatalf("attack step %q: %v", q, err)
		}
	}

	check, err := svc.VerifyIntegrity(context.Background(), &ledgerv1.VerifyIntegrityRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if check.Ok {
		t.Fatal("tampering was not detected")
	}

	all := strings.Join(check.Problems, "\n")
	for _, tag := range []string{"[double-entry]", "[signature]", "[balance]", "[audit]"} {
		if !strings.Contains(all, tag) {
			t.Fatalf("no %s finding in:\n%s", tag, all)
		}
	}
}

func TestConcurrentRetriesCreateOneTransaction(t *testing.T) {
	svc, store := newTestEnv(t)
	req := deposit("d6", 250*testUnit)

	const workers = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		txIDs   = map[string]struct{}{}
		failure error
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := svc.PostTransaction(context.Background(), req)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failure = err
				return
			}
			txIDs[resp.TxId] = struct{}{}
		}()
	}
	wg.Wait()

	if failure != nil {
		t.Fatalf("concurrent retry failed: %v", failure)
	}
	if len(txIDs) != 1 {
		t.Fatalf("%d transactions created, want 1: %v", len(txIDs), txIDs)
	}
	var count int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM ledger_entries WHERE account_id = ?`, testPlayerMain).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("player entries = %d, want 1", count)
	}
	if got := playerBalance(t, svc); got != 250*testUnit {
		t.Fatalf("balance = %d, want %d", got, 250*testUnit)
	}
}

func TestDuplicateAccountInOneTransactionRejected(t *testing.T) {
	svc, _ := newTestEnv(t)

	_, err := svc.PostTransaction(context.Background(), &ledgerv1.PostTransactionRequest{
		IdempotencyKey: "dup-account",
		Reference:      "x",
		Operation:      ledgerv1.Operation_OPERATION_ADJUST,
		ActorId:        "test",
		Postings: []*ledgerv1.Posting{
			{AccountId: testHouseHot, Amount: 10},
			{AccountId: testHouseHot, Amount: -10},
			{AccountId: testPlayerMain, Amount: 0},
		},
	})
	wantCode(t, err, codes.InvalidArgument)
}

func TestAuditChainLinksRecords(t *testing.T) {
	svc, store := newTestEnv(t)
	mustPost(t, svc, deposit("d7", 10*testUnit))

	rows, err := store.DB().Query(`SELECT seq, prev_hash, row_hash FROM audit_log ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var prev []byte
	first := true
	count := 0
	for rows.Next() {
		var seq int64
		var prevHash, rowHash []byte
		if err := rows.Scan(&seq, &prevHash, &rowHash); err != nil {
			t.Fatal(err)
		}
		if first {
			if !bytes.Equal(prevHash, make([]byte, 32)) {
				t.Fatalf("first prev_hash = %x, want 32 zero bytes", prevHash)
			}
			first = false
		} else if !bytes.Equal(prevHash, prev) {
			t.Fatalf("audit seq %d prev_hash does not link to previous row_hash", seq)
		}
		prev = rowHash
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 2 { // фондирование + депозит
		t.Fatalf("audit records = %d, want 2", count)
	}
}

func TestConcurrentDifferentKeysAreSerialized(t *testing.T) {
	svc, _ := newTestEnv(t)

	// 10 разных операций одновременно: все должны пройти, баланс — точная сумма.
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.PostTransaction(context.Background(), deposit(fmt.Sprintf("parallel-%d", i), testUnit))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("parallel post: %v", err)
		}
	}
	if got := playerBalance(t, svc); got != 10*testUnit {
		t.Fatalf("balance = %d, want %d", got, 10*testUnit)
	}
}
