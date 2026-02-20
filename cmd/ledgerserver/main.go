package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
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
	mtlsDir := flag.String("mtls-dir", "", "каталог с сертификатами; если задан — включить mTLS")
	seed := flag.Bool("seed", false, "создать демонстрационные счета при старте")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	store, err := ledger.Open(*dbPath)
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	signer, err := hsm.NewSoftHSM("soft-hsm:v1", nil)
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
	if *mtlsDir != "" {
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
