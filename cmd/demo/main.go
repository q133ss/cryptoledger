package main

// Демо-сценарий. Запускается одной командой:
//
//	go run ./cmd/demo
//
// Поднимает настоящее финансовое ядро (тот же код, что в cmd/ledgerserver),
// gRPC-сервер на случайном порту с mTLS и ходит в него настоящим gRPC-клиентом.
// Дальше по шагам показывает: двойную запись, идемпотентность, append-only,
// хеш-цепочку аудита и подпись HSM — включая попытку подмены истории.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	ledgerv1 "cryptoledger/gen/ledger/v1"
	"cryptoledger/internal/hsm"
	"cryptoledger/internal/ledger"
	"cryptoledger/internal/mtls"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

const (
	unit = int64(1_000_000) // scale=6: 1 USDT = 1_000_000 минимальных единиц

	accPlayerMain   = "acc_player_1_main"
	accPlayerLocked = "acc_player_1_locked"
	accHouseHot     = "acc_house_hot"
	accHouseEquity  = "acc_house_equity"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nДЕМО УПАЛО: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	title("ФИНАНСОВОЕ ЯДРО: ДВОЙНАЯ ЗАПИСЬ, ИДЕМПОТЕНТНОСТЬ, APPEND-ONLY, HSM")
	fmt.Println("Каждый шаг — реальный gRPC-вызов и реальные SQL-транзакции.")
	fmt.Println("Никаких заглушек бизнес-логики: подделать шаг и получить красивый вывод нельзя.")

	dir, err := os.MkdirTemp("", "cryptoledger-demo-*")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	store, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	signer, err := hsm.NewSoftHSM("soft-hsm:v1", nil)
	if err != nil {
		return err
	}
	pub := signer.PublicKey()

	// Логи сервиса в демо не нужны: они дублируют и без того подробный вывод.
	svc := ledger.NewService(store, signer, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// ---- транспорт: mTLS ---------------------------------------------------
	certFiles, err := mtls.EnsureDevCerts(filepath.Join(dir, "certs"))
	if err != nil {
		return err
	}
	serverTLS, err := mtls.ServerTLS(certFiles)
	if err != nil {
		return err
	}
	clientTLS, err := mtls.ClientTLS(certFiles)
	if err != nil {
		return err
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	ledgerv1.RegisterLedgerServiceServer(grpcServer, svc)
	go func() { _ = grpcServer.Serve(lis) }()
	defer grpcServer.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	client := ledgerv1.NewLedgerServiceClient(conn)

	section("ТРАНСПОРТ")
	fmt.Printf("  gRPC на %s\n", lis.Addr())
	fmt.Println("  TLS 1.3 + клиентский сертификат: без client.crt сервер не пустит.")
	fmt.Printf("  HSM key_id=%s, публичный ключ %x...\n", signer.KeyID(), pub[:8])
	fmt.Println("  Приватный ключ наружу не выходит: сервис умеет только Sign(digest).")

	// ---- счета -------------------------------------------------------------
	section("ШАГ 0. План счетов и стартовое фондирование")
	accounts := []ledger.Account{
		{ID: accHouseHot, OwnerID: "house", OwnerType: "house", Kind: "house_hot", Currency: "USDT", Scale: 6},
		{ID: accHouseEquity, OwnerID: "house", OwnerType: "house", Kind: "house_equity", Currency: "USDT", Scale: 6},
		{ID: accPlayerMain, OwnerID: "player_1", OwnerType: "player", Kind: "player_main", Currency: "USDT", Scale: 6},
		{ID: accPlayerLocked, OwnerID: "player_1", OwnerType: "player", Kind: "player_locked", Currency: "USDT", Scale: 6},
	}
	for _, a := range accounts {
		if err := store.CreateAccount(ctx, a); err != nil {
			return err
		}
	}
	fmt.Println("  Созданы счета (баланс = сумма проводок, отдельного поля «деньги» нет):")
	if err := printBalances(ctx, client, accHouseHot, accHouseEquity, accPlayerMain, accPlayerLocked); err != nil {
		return err
	}

	// Компания вносит 1000 USDT на горячий кошелёк. Вторая сторона — equity:
	// это не «появление денег», а перекладывание собственного капитала.
	funding := &ledgerv1.PostTransactionRequest{
		IdempotencyKey: "seed:house-funding:v1",
		Reference:      "seed:treasury",
		Operation:      ledgerv1.Operation_OPERATION_ADJUST,
		ActorId:        "system:seed",
		Note:           "стартовое фондирование горячего кошелька",
		Postings: []*ledgerv1.Posting{
			{AccountId: accHouseHot, Amount: 1000 * unit},
			{AccountId: accHouseEquity, Amount: -1000 * unit},
		},
	}
	fundResp, err := client.PostTransaction(ctx, funding)
	if err != nil {
		return fmt.Errorf("funding: %w", err)
	}
	fmt.Printf("\n  house_hot +1000 / house_equity -1000  (tx %s)\n", fundResp.TxId)
	if err := printBalances(ctx, client, accHouseHot, accHouseEquity, accPlayerMain, accPlayerLocked); err != nil {
		return err
	}

	// ---- депозит -----------------------------------------------------------
	section("ШАГ 1. Депозит игрока 250 USDT: две стороны, сумма ноль")
	deposit := &ledgerv1.PostTransactionRequest{
		IdempotencyKey: "deposit:trx-0001",
		Reference:      "deposit:0xabc123",
		Operation:      ledgerv1.Operation_OPERATION_DEPOSIT,
		ActorId:        "deposit-watcher",
		Note:           "USDT TRC-20, 20 подтверждений",
		Postings: []*ledgerv1.Posting{
			{AccountId: accHouseHot, Amount: -250 * unit},  // списание: деньги ушли из кассы
			{AccountId: accPlayerMain, Amount: 250 * unit}, // зачисление: обязательство перед игроком
		},
	}
	fmt.Println("  house_hot -250  /  player_main +250")
	fmt.Println("  Сумма = 0. Деньги не созданы, а перешли из кассы в обязательство.")

	before, err := getBalance(ctx, client, accPlayerMain)
	if err != nil {
		return err
	}
	depositResp, err := client.PostTransaction(ctx, deposit)
	if err != nil {
		return fmt.Errorf("deposit: %w", err)
	}
	after, err := getBalance(ctx, client, accPlayerMain)
	if err != nil {
		return err
	}
	fmt.Printf("\n  tx_id       = %s\n", depositResp.TxId)
	fmt.Printf("  deduplicated= %v\n", depositResp.Deduplicated)
	fmt.Printf("  player_main = %s -> %s\n",
		ledger.FormatMoney(before, 6, "USDT"), ledger.FormatMoney(after, 6, "USDT"))
	fmt.Printf("  HSM key_id  = %s\n", depositResp.Signature.KeyId)
	fmt.Printf("  digest      = %x...\n", depositResp.Signature.Digest[:12])
	fmt.Printf("  signature   = %x... (%d байт)\n",
		depositResp.Signature.Value[:12], len(depositResp.Signature.Value))
	fmt.Println("  Подпись ложится на канонический digest: tx_id + ключ + операция + все проводки.")

	// ---- идемпотентность ---------------------------------------------------
	section("ШАГ 2. Клиент не получил ответ и повторил ТОТ ЖЕ запрос")
	fmt.Println("  В реальности так бывает постоянно: таймаут сети, рестарт пода, retry в SDK.")
	replay, err := client.PostTransaction(ctx, deposit)
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	afterReplay, err := getBalance(ctx, client, accPlayerMain)
	if err != nil {
		return err
	}
	fmt.Printf("  tx_id       = %s (тот же самый)\n", replay.TxId)
	fmt.Printf("  deduplicated= %v\n", replay.Deduplicated)
	fmt.Printf("  player_main = %s (деньги не сдвинулись)\n", ledger.FormatMoney(afterReplay, 6, "USDT"))
	if replay.TxId != depositResp.TxId || !replay.Deduplicated {
		return errors.New("идемпотентность не сработала")
	}
	fmt.Println("  Как это устроено: UNIQUE(idempotency_key) + хеш тела запроса.")
	fmt.Println("  Повтор возвращает ИСХОДНУЮ операцию, а не создаёт новую.")

	section("ШАГ 3. Тот же ключ, но другое тело запроса")
	fmt.Println("  Это уже не ретрай, а ошибка клиента. Проводки быть не должно.")
	conflict := &ledgerv1.PostTransactionRequest{
		IdempotencyKey: deposit.IdempotencyKey, // тот же ключ
		Reference:      deposit.Reference,
		Operation:      deposit.Operation,
		ActorId:        deposit.ActorId,
		Postings: []*ledgerv1.Posting{
			{AccountId: accHouseHot, Amount: 251 * unit}, // другая сумма!
			{AccountId: accPlayerMain, Amount: -251 * unit},
		},
	}
	_, err = client.PostTransaction(ctx, conflict)
	if err := expectCode(err, codes.FailedPrecondition); err != nil {
		return err
	}

	// ---- инварианты --------------------------------------------------------
	section("ШАГ 4. Несбалансированная проводка: +100 и -90")
	unbalanced := &ledgerv1.PostTransactionRequest{
		IdempotencyKey: "deposit:trx-broken",
		Reference:      "deposit:broken",
		Operation:      ledgerv1.Operation_OPERATION_DEPOSIT,
		ActorId:        "deposit-watcher",
		Postings: []*ledgerv1.Posting{
			{AccountId: accHouseHot, Amount: 100 * unit},
			{AccountId: accPlayerMain, Amount: -90 * unit},
		},
	}
	_, err = client.PostTransaction(ctx, unbalanced)
	if err := expectCode(err, codes.InvalidArgument); err != nil {
		return err
	}
	fmt.Println("  10 USDT возникли бы из воздуха. Ядро отказывает ДО записи.")

	section("ШАГ 5. Ставка 300 USDT при балансе 250")
	bet := &ledgerv1.PostTransactionRequest{
		IdempotencyKey: "bet:round-42",
		Reference:      "game_round:crash-42",
		Operation:      ledgerv1.Operation_OPERATION_BET,
		ActorId:        "game-engine:crash",
		Postings: []*ledgerv1.Posting{
			{AccountId: accPlayerMain, Amount: -300 * unit},
			{AccountId: accHouseEquity, Amount: 300 * unit},
		},
	}
	_, err = client.PostTransaction(ctx, bet)
	if err := expectCode(err, codes.FailedPrecondition); err != nil {
		return err
	}
	fmt.Println("  Проверка идёт в той же транзакции, что и запись; в БД — триггер-страховка.")
	fmt.Println("  Баланс игрока не может уйти в минус.")

	// ---- резерв ------------------------------------------------------------
	section("ШАГ 6. Резерв под заявку на вывод: 100 USDT уходят в locked")
	hold := &ledgerv1.PostTransactionRequest{
		IdempotencyKey: "hold:payout-777",
		Reference:      "payout:777",
		Operation:      ledgerv1.Operation_OPERATION_HOLD,
		ActorId:        "payout-service",
		Note:           "резерв под вывод, maker-checker ожидает вторую подпись",
		Postings: []*ledgerv1.Posting{
			{AccountId: accPlayerMain, Amount: -100 * unit},
			{AccountId: accPlayerLocked, Amount: 100 * unit},
		},
	}
	holdResp, err := client.PostTransaction(ctx, hold)
	if err != nil {
		return fmt.Errorf("hold: %w", err)
	}
	fmt.Printf("  tx %s: player_main -100, player_locked +100\n", holdResp.TxId)
	if err := printBalances(ctx, client, accPlayerMain, accPlayerLocked); err != nil {
		return err
	}
	fmt.Println("  Игрок видит 150 доступных и 100 в резерве. Ставки списываются только с main.")

	// ---- журнал ------------------------------------------------------------
	section("ШАГ 7. Журнал проводок (append-only)")
	entries, err := client.ListEntries(ctx, &ledgerv1.ListEntriesRequest{Limit: 10})
	if err != nil {
		return err
	}
	fmt.Println("  Последние 10 проводок по всем счетам (полный журнал, не «баланс игрока»):")
	fmt.Printf("  %-4s %-10s %-22s %14s  %s\n", "seq", "операция", "счёт", "сумма", "reference")
	for _, e := range entries.Entries {
		fmt.Printf("  %-4d %-10s %-22s %14s  %s\n",
			e.Seq, strings.ToLower(strings.TrimPrefix(e.Operation.String(), "OPERATION_")),
			e.AccountId, ledger.FormatAmount(e.Amount, 6), e.Reference)
	}

	// ---- append-only -------------------------------------------------------
	section("ШАГ 8. Прямая попытка исправить историю в базе")
	fmt.Println("  Представим, что у нас есть SQL-доступ к базе (в проде его нет).")
	fmt.Println("  Пытаемся изменить и удалить проводку обычным UPDATE/DELETE:")

	_, err = store.DB().ExecContext(ctx, `UPDATE ledger_entries SET amount = amount + 1 WHERE seq = 1`)
	if err == nil {
		return errors.New("UPDATE прошёл: append-only не работает")
	}
	fmt.Printf("    UPDATE -> %s\n", trimDBError(err))

	_, err = store.DB().ExecContext(ctx, `DELETE FROM ledger_entries WHERE seq = 1`)
	if err == nil {
		return errors.New("DELETE прошёл: append-only не работает")
	}
	fmt.Printf("    DELETE -> %s\n", trimDBError(err))
	fmt.Println("  Это триггеры в БД, а не соглашение «мы так не делаем».")
	fmt.Println("  В Postgres дополнительно REVOKE UPDATE, DELETE на таблицу у роли сервиса.")

	// ---- аудит -------------------------------------------------------------
	section("ШАГ 9. Цепочка аудита и контрольная проверка")
	if err := printAuditChain(ctx, store); err != nil {
		return err
	}

	check, err := client.VerifyIntegrity(ctx, &ledgerv1.VerifyIntegrityRequest{})
	if err != nil {
		return err
	}
	fmt.Printf("\n  VerifyIntegrity: ok=%v, проводок=%d, операций=%d, записей аудита=%d\n",
		check.Ok, check.EntriesChecked, check.TransactionsChecked, check.AuditRecordsChecked)
	fmt.Println("  Всё сходится: суммы нулевые, подписи валидны, кэш балансов = журнал, цепочка целая.")

	// ---- атака -------------------------------------------------------------
	section("ШАГ 10. Атакующий обошёл триггеры (роль DBA / доступ к файлу БД)")
	fmt.Println("  DROP TRIGGER + UPDATE — то, что сделает человек с правами на базу.")
	fmt.Println("  Дальше вопрос: заметим ли мы это?")
	if _, err := store.DB().ExecContext(ctx, `DROP TRIGGER ledger_entries_no_update`); err != nil {
		return err
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE ledger_entries SET amount = amount + 50 * ? WHERE seq = 1`, unit); err != nil {
		return err
	}
	fmt.Println("  Изменена проводка seq=1: +50 USDT.")
	fmt.Println("  Подделана запись аудита: дописан пробел в payload последней записи.")
	if _, err := store.DB().ExecContext(ctx, `DROP TRIGGER audit_log_no_update`); err != nil {
		return err
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE audit_log SET payload = payload || ' ' WHERE seq = (SELECT MAX(seq) FROM audit_log)`); err != nil {
		return err
	}

	broken, err := client.VerifyIntegrity(ctx, &ledgerv1.VerifyIntegrityRequest{})
	if err != nil {
		return err
	}
	fmt.Printf("\n  VerifyIntegrity: ok=%v, находок=%d\n", broken.Ok, len(broken.Problems))
	for _, p := range broken.Problems {
		fmt.Printf("    ! %s\n", p)
	}
	if broken.Ok {
		return errors.New("подмена не обнаружена")
	}
	fmt.Println("\n  Четыре независимых проверки поймали подмену:")
	fmt.Println("    [double-entry] сумма проводок перестала быть нулевой;")
	fmt.Println("    [signature]    канонический digest не совпал с тем, что подписал HSM;")
	fmt.Println("    [balance]      кэш баланса разошёлся с журналом;")
	fmt.Println("    [audit]        row_hash перестал совпадать с содержимым записи.")
	fmt.Println("  Атакующий с доступом к БД может пересчитать и цепочку — поэтому tip")
	fmt.Println("  цепочки в проде ежедневно уходит во внешний WORM-якорь (S3 Object Lock).")

	title("ИТОГ")
	fmt.Println("  Двойная запись      — деньги не появляются, а перекладываются между счетами;")
	fmt.Println("  Идемпотентность     — retry возвращает исходную операцию, а не проводит вторую;")
	fmt.Println("  Append-only         — история защищена триггерами и правами БД;")
	fmt.Println("  Хеш-цепочка аудита  — правка записи задним числом видна;")
	fmt.Println("  HSM                 — подпись привязана к каноническому digest операции.")
	return nil
}

// --- helpers ----------------------------------------------------------------

func title(s string) {
	fmt.Printf("\n%s\n%s\n%s\n", strings.Repeat("=", 78), s, strings.Repeat("=", 78))
}

func section(s string) {
	fmt.Printf("\n%s\n", s)
}

func printBalances(ctx context.Context, c ledgerv1.LedgerServiceClient, ids ...string) error {
	for _, id := range ids {
		resp, err := c.GetAccount(ctx, &ledgerv1.GetAccountRequest{AccountId: id})
		if err != nil {
			return err
		}
		a := resp.Account
		fmt.Printf("    %-22s %14s %s\n", a.AccountId, ledger.FormatAmount(a.Balance, a.Scale), a.Currency)
	}
	return nil
}

func getBalance(ctx context.Context, c ledgerv1.LedgerServiceClient, id string) (int64, error) {
	resp, err := c.GetAccount(ctx, &ledgerv1.GetAccountRequest{AccountId: id})
	if err != nil {
		return 0, err
	}
	return resp.Account.Balance, nil
}

func expectCode(err error, want codes.Code) error {
	if err == nil {
		return fmt.Errorf("ожидали ошибку %s, а вызов прошёл успешно", want)
	}
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("ожидали gRPC-ошибку %s, получили: %w", want, err)
	}
	if st.Code() != want {
		return fmt.Errorf("ожидали %s, получили %s: %s", want, st.Code(), st.Message())
	}
	fmt.Printf("  код: %s\n  ответ: %s\n", st.Code(), st.Message())
	return nil
}

func printAuditChain(ctx context.Context, store *ledger.Store) error {
	rows, err := store.DB().QueryContext(ctx,
		`SELECT seq, actor_id, action, substring(hex(row_hash), 1, 16), substring(hex(prev_hash), 1, 16)
		 FROM audit_log ORDER BY seq`)
	if err != nil {
		return err
	}
	defer rows.Close()

	fmt.Printf("  %-4s %-20s %-30s %-18s %s\n", "seq", "кто", "что", "row_hash", "prev_hash")
	for rows.Next() {
		var seq int64
		var actor, action, rowHash, prevHash string
		if err := rows.Scan(&seq, &actor, &action, &rowHash, &prevHash); err != nil {
			return err
		}
		fmt.Printf("  %-4d %-20s %-30s %-18s %s\n", seq, actor, action, rowHash, prevHash)
	}
	return rows.Err()
}

func trimDBError(err error) string {
	msg := err.Error()
	// modernc.org/sqlite добавляет к тексту служебный суффикс с кодом SQLite.
	if i := strings.LastIndex(msg, " ("); i > 0 && strings.HasSuffix(msg, ")") {
		if _, convErr := strconv.Atoi(msg[i+2 : len(msg)-1]); convErr == nil {
			msg = msg[:i]
		}
	}
	msg = strings.TrimPrefix(msg, "constraint failed: ")
	msg = strings.TrimPrefix(msg, "SQL logic error: ")
	return msg
}
