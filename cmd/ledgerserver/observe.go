package main

import (
	"context"
	v1 "cryptoledger/gen/ledger/v1"
	"cryptoledger/internal/ledger"
	"fmt"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

type metrics struct{ committed, replayed, rejected, totalNanos, verifyErrors, balanceMismatches atomic.Uint64 }

func (m *metrics) interceptor(log *slog.Logger, limiter *rate.Limiter) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (resp any, err error) {
		start := time.Now()
		id := uuid.NewString()
		grpc.SetHeader(ctx, metadata.Pairs("x-request-id", id))
		defer func() {
			if recover() != nil {
				err = status.Error(codes.Internal, "request failed")
			}
			attrs := []any{"request_id", id, "rpc", info.FullMethod, "code", status.Code(err)}
			if post, ok := req.(*v1.PostTransactionRequest); ok {
				attrs = append(attrs, "actor_id", post.ActorId)
				m.totalNanos.Add(uint64(time.Since(start)))
				if result, ok := resp.(*v1.PostTransactionResponse); ok && result != nil && err == nil {
					attrs = append(attrs, "tx_id", result.TxId)
					if result.Deduplicated {
						m.replayed.Add(1)
					} else {
						m.committed.Add(1)
					}
				} else {
					m.rejected.Add(1)
				}
			}
			if report, ok := resp.(*v1.VerifyIntegrityResponse); ok && report != nil && !report.Ok {
				m.verifyErrors.Add(1)
				for _, p := range report.Problems {
					if strings.HasPrefix(p, "[balance]") {
						m.balanceMismatches.Add(1)
					}
				}
			}
			log.Info("rpc", attrs...)
		}()
		if !limiter.Allow() {
			return nil, status.Error(codes.ResourceExhausted, "request limit exceeded")
		}
		return next(ctx, req)
	}
}
func (m *metrics) routes(store *ledger.Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if store.DB().PingContext(ctx) != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		fmt.Fprintln(w, "ready")
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "ledger_tx_total{result=\"committed\"} %d\nledger_tx_total{result=\"replayed\"} %d\nledger_tx_total{result=\"rejected\"} %d\n", m.committed.Load(), m.replayed.Load(), m.rejected.Load())
		fmt.Fprintf(w, "# TYPE ledger_tx_duration_seconds summary\nledger_tx_duration_seconds_sum %f\nledger_tx_duration_seconds_count %d\n", float64(m.totalNanos.Load())/1e9, m.committed.Load()+m.replayed.Load()+m.rejected.Load())
		fmt.Fprintf(w, "ledger_verify_failures_total %d\nledger_balance_cache_mismatch_total %d\n", m.verifyErrors.Load(), m.balanceMismatches.Load())
	})
	return mux
}
