// Package hsm — граница с аппаратным модулем подписи.
//
// В резюме: «Подпись транзакций изолировал в HSM».
// Смысл изоляции в том, что приватный ключ НЕ покидает модуль: наружу уходит
// только подпись и идентификатор ключа. Поэтому интерфейс Signer намеренно
// не содержит метода, который вернул бы приватный ключ.
//
// В демо используется SoftHSM — ed25519-ключ в памяти процесса. В проде вместо
// него подключается PKCS#11-клиент к CloudHSM/YubiHSM или Vault Transit:
// код сервиса при этом не меняется, меняется только реализация интерфейса.
package hsm

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
)

// Signer — то, что видит финансовое ядро.
type Signer interface {
	// KeyID — версия ключа. Stores in transactions.key_id: при ротации
	// старые подписи остаются проверяемыми.
	KeyID() string

	// PublicKey нужен верификатору. Приватный ключ наружу не отдаётся.
	PublicKey() ed25519.PublicKey

	// Sign подписывает digest (SHA-256 канонического представления операции).
	Sign(ctx context.Context, digest []byte) ([]byte, error)
}

// SoftHSM — демонстрационная заглушка HSM. Ключ создаётся при старте и живёт
// только в памяти; в реальном модуле он вообще не покидает чип.
type SoftHSM struct {
	keyID   string
	public  ed25519.PublicKey
	private ed25519.PrivateKey
}

// NewSoftHSM генерирует ключ. seed == nil — случайный ключ (как в проде при
// инициализации модуля); seed != nil — детерминированный ключ для тестов.
func NewSoftHSM(keyID string, seed []byte) (*SoftHSM, error) {
	if keyID == "" {
		return nil, errors.New("hsm: key id is required")
	}
	if seed != nil && len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("hsm: seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}

	var pub ed25519.PublicKey
	var priv ed25519.PrivateKey
	if seed == nil {
		p, k, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("hsm: generate key: %w", err)
		}
		pub, priv = p, k
	} else {
		priv = ed25519.NewKeyFromSeed(seed)
		pub = priv.Public().(ed25519.PublicKey)
	}

	return &SoftHSM{keyID: keyID, public: pub, private: priv}, nil
}

func (h *SoftHSM) KeyID() string { return h.keyID }

func (h *SoftHSM) PublicKey() ed25519.PublicKey {
	// Возвращаем копию, чтобы вызывающий код не мог испортить ключ.
	out := make(ed25519.PublicKey, len(h.public))
	copy(out, h.public)
	return out
}

func (h *SoftHSM) Sign(ctx context.Context, digest []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(digest) != 32 {
		return nil, fmt.Errorf("hsm: digest must be 32 bytes (sha-256), got %d", len(digest))
	}
	return ed25519.Sign(h.private, digest), nil
}

// Verify проверяет подпись. В проде это делает либо сам HSM, либо сервис
// по публичному ключу — приватный ключ для проверки не нужен.
func Verify(public ed25519.PublicKey, digest, signature []byte) bool {
	if len(public) != ed25519.PublicKeySize || len(digest) != 32 {
		return false
	}
	return ed25519.Verify(public, digest, signature)
}
