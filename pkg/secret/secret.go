// Package secret seals credential secrets at rest with AES-256-GCM.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// KeyVersion tags what Seal produces; a rotation adds a new version next to this one.
const KeyVersion = 1

// Box seals and opens secrets under one key.
type Box struct {
	aead cipher.AEAD
}

// ParseKey decodes a standard base64 32-byte key (GEDOUR_ENCRYPTION_KEY).
func ParseKey(b64 string) ([]byte, error) {
	if b64 == "" {
		return nil, errors.New("encryption key is empty")
	}
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("encryption key: not base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key: %d bytes, want 32", len(key))
	}
	return key, nil
}

// New returns a Box for the base64 key.
func New(b64 string) (*Box, error) {
	key, err := ParseKey(b64)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext bound to aad (the row ID), under a fresh random nonce.
func (b *Box) Seal(plaintext, aad []byte) (ciphertext, nonce []byte) {
	nonce = make([]byte, b.aead.NonceSize())
	_, _ = rand.Read(nonce) // crypto/rand.Read never fails (Go 1.24+)
	return b.aead.Seal(nil, nonce, plaintext, aad), nonce
}

// Open decrypts what Seal produced with the same aad; a changed byte or another aad fails.
func (b *Box) Open(ciphertext, nonce, aad []byte) ([]byte, error) {
	if len(nonce) != b.aead.NonceSize() {
		return nil, errors.New("open secret: bad nonce size")
	}
	out, err := b.aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("open secret: %w", err)
	}
	return out, nil
}
