// ledgerctl exercises the public gRPC API and provides two local operator commands.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	v1 "cryptoledger/gen/ledger/v1"
	"cryptoledger/internal/ledger"
	"cryptoledger/internal/mtls"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ledgerctl", flag.ContinueOnError)
	addr := fs.String("addr", "localhost:50051", "gRPC server")
	certDir := fs.String("cert-dir", ".ledger/certs", "client certificate directory")
	dbPath := fs.String("db", "ledger.db", "SQLite path for local operator commands")
	if err := fs.Parse(args); err != nil {
		return err
	}
	args = fs.Args()
	if len(args) >= 2 && (args[0] == "account" && args[1] == "create" || args[0] == "audit" && args[1] == "tail") {
		return operator(ctx, *dbPath, args)
	}
	f := mtls.Files{CAPEM: filepath.Join(*certDir, "ca.pem"), ClientCert: filepath.Join(*certDir, "client.crt"), ClientKey: filepath.Join(*certDir, "client.key")}
	cfg, err := mtls.ClientTLS(f)
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := v1.NewLedgerServiceClient(conn)
	if len(args) == 0 {
		return errors.New("expected demo, verify, account get/create, audit tail or post")
	}
	switch args[0] {
	case "verify":
		v, err := client.VerifyIntegrity(ctx, &v1.VerifyIntegrityRequest{})
		if err = show(v, err); err != nil {
			return err
		}
		if !v.Ok {
			return errors.New("verification failed")
		}
		return nil
	case "account":
		if len(args) != 3 || args[1] != "get" {
			return errors.New("usage: account get ID")
		}
		return show(client.GetAccount(ctx, &v1.GetAccountRequest{AccountId: args[2]}))
	case "post":
		flags := flag.NewFlagSet("post", flag.ContinueOnError)
		req := new(v1.PostTransactionRequest)
		flags.StringVar(&req.IdempotencyKey, "key", "", "idempotency key")
		flags.StringVar(&req.Reference, "ref", "", "business reference")
		op := flags.String("operation", "deposit", "ledger operation")
		flags.Func("posting", "account:minor-units", func(value string) error {
			id, raw, ok := strings.Cut(value, ":")
			if !ok {
				return errors.New("expected account:minor-units")
			}
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return err
			}
			req.Postings = append(req.Postings, &v1.Posting{AccountId: id, Amount: n})
			return nil
		})
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		req.Operation = v1.Operation(v1.Operation_value["OPERATION_"+strings.ToUpper(*op)])
		return show(client.PostTransaction(ctx, req))
	case "demo":
		// Discard the first reply exactly as a client losing its connection would.
		const key = "quickstart-deposit-v1"
		req := &v1.PostTransactionRequest{IdempotencyKey: key, Reference: "quickstart", Operation: v1.Operation_OPERATION_DEPOSIT, Postings: []*v1.Posting{{AccountId: "acc_house_hot", Amount: -1000000}, {AccountId: "acc_player_1_main", Amount: 1000000}}}
		if _, err := client.PostTransaction(ctx, req); err != nil {
			return err
		}
		fmt.Println("1. Sent a transfer of 1.000000 USDT and discarded the reply.")
		post, err := client.PostTransaction(ctx, req)
		if err != nil {
			return err
		}
		if !post.Deduplicated {
			return errors.New("retry created another transaction")
		}
		fmt.Printf("2. Retried the same key. tx_id=%s deduplicated=true\n", post.TxId)
		for _, a := range post.Balances {
			fmt.Printf("   %s  %s\n", a.AccountId, ledger.FormatMoney(a.Balance, a.Scale, a.Currency))
		}
		report, err := client.VerifyIntegrity(ctx, &v1.VerifyIntegrityRequest{})
		if err != nil {
			return err
		}
		fmt.Printf("3. Verified journal. transactions=%d entries=%d audit=%d ok=%t\n", report.TransactionsChecked, report.EntriesChecked, report.AuditRecordsChecked, report.Ok)
		if !report.Ok {
			return fmt.Errorf("integrity check: %v", report.Problems)
		}
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
func show(result proto.Message, err error) error {
	if err != nil {
		return err
	}
	data, err := (protojson.MarshalOptions{Indent: "  ", UseProtoNames: true}).Marshal(result)
	if err == nil {
		fmt.Println(string(data))
	}
	return err
}
func operator(ctx context.Context, path string, args []string) error {
	var s *ledger.Store
	var err error
	if dsn := os.Getenv("LEDGER_DSN"); dsn != "" {
		s, err = ledger.OpenPostgres(ctx, dsn)
	} else {
		s, err = ledger.Open(path)
	}
	if err != nil {
		return err
	}
	defer s.Close()
	fs := flag.NewFlagSet(strings.Join(args[:2], " "), flag.ContinueOnError)
	if args[0] == "account" {
		a := ledger.Account{}
		fs.StringVar(&a.ID, "id", "", "account ID")
		fs.StringVar(&a.OwnerID, "owner", "", "owner ID")
		fs.StringVar(&a.OwnerType, "type", "player", "owner type")
		fs.StringVar(&a.Kind, "kind", "player_main", "account kind")
		fs.StringVar(&a.Currency, "currency", "USDT", "currency")
		scale := fs.Int("scale", 6, "fraction digits (0..18)")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *scale < 0 || *scale > 18 {
			return errors.New("invalid scale")
		}
		a.Scale = int32(*scale)
		return s.CreateAccount(ctx, a)
	}
	n := fs.Int("n", 20, "latest records (1..500)")
	if err = fs.Parse(args[2:]); err != nil {
		return err
	}
	if *n < 1 || *n > 500 {
		return errors.New("n must be in 1..500")
	}
	rows, err := s.DB().QueryContext(ctx, "SELECT seq,actor_id,action,target FROM audit_log ORDER BY seq DESC LIMIT $1", *n)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var actor, action, target string
		if err = rows.Scan(&seq, &actor, &action, &target); err != nil {
			return err
		}
		fmt.Printf("%d\t%s\t%s\t%s\n", seq, actor, action, target)
	}
	return rows.Err()
}
