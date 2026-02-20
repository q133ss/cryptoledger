package ledger

import (
	"bytes"
	"context"
	v1 "cryptoledger/gen/ledger/v1"
	"cryptoledger/internal/hsm"
	"fmt"
	"github.com/google/uuid"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func benchmarkService(b *testing.B, dsn string) (*Service, func(int64) *v1.PostTransactionRequest) {
	b.Helper()
	var store *Store
	var err error
	if dsn == "" {
		store, err = Open(filepath.Join(b.TempDir(), "ledger.db"))
	} else {
		store, err = OpenPostgres(context.Background(), dsn)
	}
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { store.Close() })
	signer, err := hsm.NewSoftHSM("integration", bytes.Repeat([]byte{42}, 32))
	if err != nil {
		b.Fatal(err)
	}
	svc := NewService(store, signer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	prefix := uuid.NewString()
	for _, a := range []Account{{prefix + "-cash", "house", "house", "house_hot", "USD", 2}, {prefix + "-user", "user", "player", "player_main", "USD", 2}} {
		if err = store.CreateAccount(context.Background(), a); err != nil {
			b.Fatal(err)
		}
	}
	return svc, func(index int64) *v1.PostTransactionRequest {
		req := deposit(fmt.Sprintf("%s-%d", prefix, index), 1)
		req.Postings[0].AccountId = prefix + "-cash"
		req.Postings[1].AccountId = prefix + "-user"
		return req
	}
}
func BenchmarkPostTransaction(b *testing.B) {
	engines := []struct{ name, dsn string }{{"SQLite", ""}}
	if dsn := os.Getenv("LEDGER_BENCH_DSN"); dsn != "" {
		engines = append(engines, struct{ name, dsn string }{"PostgreSQL", dsn})
	}
	for _, e := range engines {
		for _, mode := range []string{"Serial", "Parallel"} {
			b.Run(e.name+"/"+mode, func(b *testing.B) {
				svc, req := benchmarkService(b, e.dsn)
				var count atomic.Int64
				samples := make([]time.Duration, b.N)
				var once sync.Once
				post := func() {
					i := count.Add(1) - 1
					start := time.Now()
					_, err := svc.PostTransaction(context.Background(), req(i))
					samples[i] = time.Since(start)
					if err != nil {
						once.Do(func() { b.Error(err) })
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				if mode == "Parallel" {
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							post()
						}
					})
				} else {
					for range b.N {
						post()
					}
				}
				b.StopTimer()
				sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
				b.ReportMetric(float64(samples[(b.N-1)*50/100].Microseconds()), "us/p50")
				b.ReportMetric(float64(samples[(b.N-1)*99/100].Microseconds()), "us/p99")
			})
		}
	}
}
func BenchmarkVerifyIntegrity1000(b *testing.B) {
	svc, request := benchmarkService(b, "")
	for i := range 1000 {
		if _, err := svc.PostTransaction(context.Background(), request(int64(i))); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for range b.N {
		v, err := svc.VerifyIntegrity(context.Background(), &v1.VerifyIntegrityRequest{})
		if err != nil || !v.GetOk() {
			b.Fatalf("verify: %v %v", v, err)
		}
	}
}
