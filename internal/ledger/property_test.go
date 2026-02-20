package ledger

import (
	"bytes"
	"context"
	v1 "cryptoledger/gen/ledger/v1"
	"fmt"
	"testing"
)

func FuzzCanonicalPostingOrder(f *testing.F) {
	f.Add("ref", int64(10))
	f.Fuzz(func(t *testing.T, ref string, amount int64) {
		a := deposit("key", amount)
		p := sortedPostings(a.Postings)
		a.Postings[0], a.Postings[1] = a.Postings[1], a.Postings[0]
		if !bytes.Equal(requestHash("deposit", ref, p), requestHash("deposit", ref, sortedPostings(a.Postings))) {
			t.Fatal("order changed request hash")
		}
	})
}
func TestBalanceFollowsJournal(t *testing.T) {
	svc, _ := newTestEnv(t)
	var balance int64
	for i := 1; i <= 32; i++ {
		amount := int64(i * i)
		req := deposit(fmt.Sprint(i), amount)
		if i%3 == 0 {
			req.Postings[0].Amount++
			if _, err := svc.PostTransaction(context.Background(), req); err == nil {
				t.Fatal("unbalanced post accepted")
			}
		} else {
			mustPost(t, svc, req)
			balance += amount
		}
		if got := playerBalance(t, svc); got != balance {
			t.Fatalf("step %d: got %d want %d", i, got, balance)
		}
	}
	report, err := svc.VerifyIntegrity(context.Background(), &v1.VerifyIntegrityRequest{})
	if err != nil || !report.GetOk() {
		t.Fatalf("verify: %v %v", report, err)
	}
}
