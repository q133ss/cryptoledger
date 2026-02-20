package ledger

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"

	ledgerv1 "cryptoledger/gen/ledger/v1"
)

// Каноническое представление — это то, что подписывает HSM и что можно
// пересчитать из базы спустя годы. Обычный JSON для этого не годится:
// порядок полей и форматирование чисел могут измениться и подпись «сломается»
// без изменения данных. Поэтому — фиксированный порядок полей и length-prefix
// перед каждым значением: "ab"+"c" и "a"+"bc" дают разные байты.
type encoder struct {
	buf []byte
}

func (e *encoder) raw(b []byte) {
	var l [8]byte
	binary.BigEndian.PutUint64(l[:], uint64(len(b)))
	e.buf = append(e.buf, l[:]...)
	e.buf = append(e.buf, b...)
}

func (e *encoder) str(s string) { e.raw([]byte(s)) }

func (e *encoder) i64(v int64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	e.buf = append(e.buf, b[:]...)
}

func (e *encoder) i32(v int32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(v))
	e.buf = append(e.buf, b[:]...)
}

func hash(e *encoder) []byte {
	sum := sha256.Sum256(e.buf)
	return sum[:]
}

// posting — одна сторона проводки в уже отсортированном виде.
type posting struct {
	AccountID string
	Amount    int64
}

// sortedPostings приводит проводки к детерминированному порядку
// (счёт, затем сумма). Клиент может прислать их в любом порядке — подпись
// и request_hash от этого не зависят.
func sortedPostings(in []*ledgerv1.Posting) []posting {
	out := make([]posting, 0, len(in))
	for _, p := range in {
		out = append(out, posting{AccountID: p.GetAccountId(), Amount: p.GetAmount()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AccountID != out[j].AccountID {
			return out[i].AccountID < out[j].AccountID
		}
		return out[i].Amount > out[j].Amount
	})
	return out
}

// requestHash отвечает на вопрос «это тот же самый запрос?».
// Используется при повторном idempotency_key: если тело другое — это не ретрай,
// а ошибка клиента, и операцию проводить нельзя.
//
// The replay path also compares the authenticated actor and signed note.
func requestHash(operation, reference string, postings []posting) []byte {
	e := &encoder{}
	e.str("ledger.request.v1")
	e.str(operation)
	e.str(reference)
	for _, p := range postings {
		e.str(p.AccountID)
		e.i64(p.Amount)
	}
	return hash(e)
}

// txDigest — предмет подписи HSM. Включает tx_id, поэтому подпись нельзя
// перенести на другую операцию с тем же набором проводок.
func txDigest(txID, idempotencyKey, operation, reference, actorID, note, currency string, scale int32, postings []posting) []byte {
	e := &encoder{}
	e.str("ledger.tx.v1")
	e.str(txID)
	e.str(idempotencyKey)
	e.str(operation)
	e.str(reference)
	e.str(actorID)
	e.str(note)
	e.str(currency)
	e.i32(scale)
	for _, p := range postings {
		e.str(p.AccountID)
		e.i64(p.Amount)
	}
	return hash(e)
}

// auditRowHash связывает строку аудита с предыдущей.
//
// row_hash = SHA-256(prev_hash || поля строки). Правка любой строки задним
// числом ломает её хеш, а удаление строки — prev_hash следующей записи.
//
// Важное ограничение: тот, кто может переписать весь журнал целиком и
// пересчитать все хеши, подделает цепочку. Поэтому tip цепочки периодически
// уходит во внешний якорь (S3 Object Lock / WORM), который менять нельзя.
func auditRowHash(prevHash []byte, actorID, action, target, payload, createdAt string) []byte {
	e := &encoder{}
	e.str("ledger.audit.v1")
	e.raw(prevHash)
	e.str(actorID)
	e.str(action)
	e.str(target)
	e.str(payload)
	e.str(createdAt)
	return hash(e)
}
