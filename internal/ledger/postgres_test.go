//go:build integration

package ledger

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	v1 "cryptoledger/gen/ledger/v1"
	"cryptoledger/internal/hsm"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// Each caller uses a separate pool, just as independent service instances would.
func TestPostgresLostReply(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("LEDGER_TEST_DSN")
	if dsn == "" {
		t.Fatal("set LEDGER_TEST_DSN to a disposable, provisioned PostgreSQL database")
	}
	signer, err := hsm.NewSoftHSM("integration", bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	key := uuid.NewString()
	var services []*Service
	for range 10 {
		store, err := OpenPostgres(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		services = append(services, NewService(store, signer, log))
	}
	svc := services[0]
	for _, a := range []Account{{key + "-player", key, "player", "player_main", "USD", 2}, {key + "-cash", key, "house", "house_hot", "USD", 2}} {
		if err := svc.store.CreateAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	req := deposit(key, 100)
	req.Postings[0].AccountId = key + "-cash"
	req.Postings[1].AccountId = key + "-player"
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan *v1.PostTransactionResponse, len(services))
	for _, s := range services {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := s.PostTransaction(ctx, req)
			if err != nil {
				t.Error(err)
				return
			}
			results <- resp
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var id string
	var fresh, received int
	for resp := range results {
		received++
		if id == "" {
			id = resp.TxId
		}
		if id != resp.TxId {
			t.Fatal("multiple transaction IDs")
		}
		if !resp.Deduplicated {
			fresh++
		}
	}
	if received != 10 || fresh != 1 {
		t.Fatalf("received %d responses, %d first commits", received, fresh)
	}
	db := svc.store.DB()
	var headers, entries, balance int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM transactions WHERE tx_id=$1),(SELECT count(*) FROM ledger_entries WHERE tx_id=$1),(SELECT balance FROM account_balances WHERE account_id=$2)`, id, key+"-player").Scan(&headers, &entries, &balance); err != nil {
		t.Fatal(err)
	}
	if headers != 1 || entries != 2 || balance != 100 {
		t.Fatalf("headers=%d entries=%d balance=%d", headers, entries, balance)
	}
	other := proto.Clone(req).(*v1.PostTransactionRequest)
	other.Postings[0].Amount = -200
	other.Postings[1].Amount = 200
	_, err = svc.PostTransaction(ctx, other)
	wantCode(t, err, codes.FailedPrecondition)
	other.IdempotencyKey = key + "-next"
	mustPost(t, svc, other)
	replay := mustPost(t, svc, req)
	for _, a := range replay.Balances {
		if a.AccountId == key+"-player" && a.Balance != 100 {
			t.Fatal("replay returned a later balance")
		}
	}
	other.IdempotencyKey = key + "-invalid"
	other.Postings[0].Amount++
	_, err = svc.PostTransaction(ctx, other)
	wantCode(t, err, codes.InvalidArgument)
	if _, err = db.ExecContext(ctx, "UPDATE account_balances SET balance=-1 WHERE account_id=$1", key+"-player"); err == nil {
		t.Fatal("database accepted negative player balance")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO ledger_entries(tx_id,account_id,amount,created_at) VALUES ($1,$2,1,$3)", id, key+"-cash", formatTime(svc.now())); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err == nil {
		t.Fatal("unbalanced entry committed")
	}
	if _, err = db.ExecContext(ctx, "DELETE FROM transactions WHERE tx_id=$1", id); err == nil {
		t.Fatal("runtime role can delete history")
	}
	// Distinct keys compete to debit the full remaining balance.
	// Exactly one debit can commit after the row lock is acquired.
	debitResults := make(chan error, len(services))
	for i, s := range services {
		debit := proto.Clone(req).(*v1.PostTransactionRequest)
		debit.IdempotencyKey = fmt.Sprintf("%s-withdraw-%d", key, i)
		debit.Operation = v1.Operation_OPERATION_WITHDRAW
		debit.Postings[0].Amount = 300
		debit.Postings[1].Amount = -300
		go func() { _, err := s.PostTransaction(ctx, debit); debitResults <- err }()
	}
	successes := 0
	for range services {
		err := <-debitResults
		if err == nil {
			successes++
		} else {
			wantCode(t, err, codes.FailedPrecondition)
		}
	}
	if successes != 1 {
		t.Fatalf("%d debits succeeded", successes)
	}
	report, err := svc.VerifyIntegrity(ctx, &v1.VerifyIntegrityRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Ok {
		t.Fatal(fmt.Sprint(report.Problems))
	}
}
