// Package secretbox — шифрование учётных данных хостингов перед записью в
// БД (design add-repo-onboarding, решение 4). Ключ живёт в окружении rivetd,
// в базе его нет: дампа БД недостаточно, чтобы восстановить токены.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// ErrNoKey — ключ шифрования не настроен. Операции с токенами отключены
// (fail-closed), установка продолжает работать на глобальном токене.
var ErrNoKey = errors.New("ключ шифрования не настроен (RIVET_SECRET_KEY)")

// Box шифрует и расшифровывает секреты AES-256-GCM. Нулевой Box (без ключа)
// возвращает ErrNoKey на любую операцию — так вызывающему не нужно
// отдельно проверять, настроен ли ключ.
type Box struct {
	aead cipher.AEAD
}

// New разбирает ключ из base64 (ровно 32 байта после декодирования).
// Пустая строка — Box без ключа, это не ошибка конфигурации.
func New(keyB64 string) (*Box, error) {
	if keyB64 == "" {
		return &Box{}, nil
	}
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, fmt.Errorf("ключ шифрования: ожидается base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("ключ шифрования: ожидается 32 байта, получено %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Enabled — настроен ли ключ.
func (b *Box) Enabled() bool { return b != nil && b.aead != nil }

// Seal шифрует секрет; результат — nonce || шифротекст.
func (b *Box) Seal(plain string) ([]byte, error) {
	if !b.Enabled() {
		return nil, ErrNoKey
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, []byte(plain), nil), nil
}

// Open расшифровывает то, что вернул Seal. Порча шифротекста или чужой
// ключ дают ошибку аутентификации GCM, а не мусор.
func (b *Box) Open(sealed []byte) (string, error) {
	if !b.Enabled() {
		return "", ErrNoKey
	}
	ns := b.aead.NonceSize()
	if len(sealed) < ns {
		return "", errors.New("шифротекст короче nonce")
	}
	plain, err := b.aead.Open(nil, sealed[:ns], sealed[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("расшифровка: %w", err)
	}
	return string(plain), nil
}
