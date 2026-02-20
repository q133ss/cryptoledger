package main

import (
	"context"
	"errors"
	"flag"
	"golang.org/x/time/rate"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	ledgerv1 "cryptoledger/gen/ledger/v1"
	"cryptoledger/internal/hsm"
	"cryptoledger/internal/ledger"
	"cryptoledger/internal/mtls"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection"
)

// ledgerserver — отдельный процесс финансового ядра.
// В проде он живёт в изолированном namespace, доступен только по gRPC/mTLS и
// не имеет доступа к остальным базам. Демо умеет запускаться и без TLS,
// чтобы было удобно смотреть через grpcurl на localhost.
func main() {
	addr := flag.String("addr", "127.0.0.1:50051", "адрес gRPC-сервера")
	dbPath := flag.String("db", "ledger.db", "путь к базе журнала")
	mtlsDir := flag.String("mtls-dir", ".ledger/certs", "каталог с сертификатами; если задан — включить mTLS")
	seed := flag.Bool("seed", false, "создать демонстрационные счета при старте")
	keyPath := flag.String("signing-key", ".ledger/signing.key", "path to local Ed25519 signing seed")
	plain := flag.Bool("dev-plaintext", false, "allow local plaintext and caller-supplied actors")
	health := flag.String("http-addr", "127.0.0.1:8081", "health and metrics listener")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	var store *ledger.Store
	var err error
	if dsn := os.Getenv("LEDGER_DSN"); dsn != "" {
		store, err = ledger.OpenPostgres(context.Background(), dsn)
	} else {
		store, err = ledger.Open(*dbPath)
	}
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	signer, err := hsm.OpenLocalSigner(*keyPath)
	if err != nil {
		log.Error("init hsm", "err", err)
		os.Exit(1)
	}

	if *seed {
		for _, a := range []ledger.Account{
			{ID: "acc_house_hot", OwnerID: "house", OwnerType: "house", Kind: "house_hot", Currency: "USDT", Scale: 6},
			{ID: "acc_house_equity", OwnerID: "house", OwnerType: "house", Kind: "house_equity", Currency: "USDT", Scale: 6},
			{ID: "acc_player_1_main", OwnerID: "player_1", OwnerType: "player", Kind: "player_main", Currency: "USDT", Scale: 6},
			{ID: "acc_player_1_locked", OwnerID: "player_1", OwnerType: "player", Kind: "player_locked", Currency: "USDT", Scale: 6},
		} {
			if err := store.CreateAccount(context.Background(), a); err != nil {
				log.Error("seed account", "id", a.ID, "err", err)
				os.Exit(1)
			}
		}
		log.Info("demo accounts created")
	}

	var opts []grpc.ServerOption
	var interceptors []grpc.UnaryServerInterceptor
	if !*plain {
		if *mtlsDir == "" {
			log.Error("missing certificate directory")
			os.Exit(1)
		}
		interceptors = append(interceptors, mtls.Identity)
		files, err := mtls.EnsureDevCerts(*mtlsDir)
		if err != nil {
			log.Error("init mtls", "err", err)
			os.Exit(1)
		}
		tlsConfig, err := mtls.ServerTLS(files)
		if err != nil {
			log.Error("server tls", "err", err)
			os.Exit(1)
		}
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlsConfig)))
		log.Info("mTLS enabled", "ca", files.CAPEM, "client_cert", files.ClientCert)
	}

	m := new(metrics)
	interceptors = append(interceptors, m.interceptor(log, rate.NewLimiter(1000, 2000)))
	opts = append(opts, grpc.ChainUnaryInterceptor(interceptors...))
	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}

	server := grpc.NewServer(opts...)
	ledgerv1.RegisterLedgerServiceServer(server, ledger.NewService(store, signer, log))
	// Reflection — только для удобства разработки (grpcurl). В проде выключен.
	reflection.Register(server)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpLis, err := net.Listen("tcp", *health)
	if err != nil {
		log.Error("health listener", "err", err)
		os.Exit(1)
	}
	healthServer := &http.Server{Handler: m.routes(store), ReadHeaderTimeout: 3 * time.Second}
	go func() {
		if err := healthServer.Serve(httpLis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health server", "err", err)
			stop()
		}
	}()
	defer healthServer.Close()
	go func() {
		<-ctx.Done()
		stopped := make(chan struct{})
		go func() {
			server.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			server.Stop()
		}
	}()

	log.Info("ledger server listening", "addr", lis.Addr().String(), "db", *dbPath)
	if err := server.Serve(lis); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
