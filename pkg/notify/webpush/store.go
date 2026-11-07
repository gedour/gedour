package webpush

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gedour/gedour/pkg/secret"
)

// ErrKeyUnreadable marks a stored key that does not open or parse: another encryption key, or a damaged row.
var ErrKeyUnreadable = errors.New("vapid key unreadable")

// keyAAD binds the sealed private key to its row: the table holds one row only.
var keyAAD = []byte("vapid_key")

// LoadOrCreate returns the stored VAPID key, generating and storing it on first start. Concurrent first
// starts each insert-if-absent then read back, so they all end up with the one row that won.
func LoadOrCreate(ctx context.Context, pool *pgxpool.Pool, box *secret.Box) (*Key, error) {
	k, err := load(ctx, pool, box)
	if !errors.Is(err, pgx.ErrNoRows) {
		return k, err
	}
	gen, err := GenerateKey()
	if err != nil {
		return nil, err
	}
	raw, err := gen.privateBytes()
	if err != nil {
		return nil, err
	}
	ct, nonce := box.Seal(raw, keyAAD)
	if _, err := pool.Exec(ctx, `INSERT INTO vapid_key (public_key, private_ciphertext, private_nonce, key_version)
		VALUES ($1, $2, $3, $4) ON CONFLICT (id) DO NOTHING`, gen.publicBytes(), ct, nonce, secret.KeyVersion); err != nil {
		return nil, fmt.Errorf("store vapid key: %w", err)
	}
	return load(ctx, pool, box)
}

func load(ctx context.Context, pool *pgxpool.Pool, box *secret.Box) (*Key, error) {
	var pub, ct, nonce []byte
	err := pool.QueryRow(ctx, `SELECT public_key, private_ciphertext, private_nonce FROM vapid_key`).Scan(&pub, &ct, &nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("load vapid key: %w", err)
	}
	raw, err := box.Open(ct, nonce, keyAAD)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKeyUnreadable, err)
	}
	k, err := ParseKey(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKeyUnreadable, err)
	}
	if !bytes.Equal(k.publicBytes(), pub) {
		return nil, fmt.Errorf("%w: stored public key does not match the private key", ErrKeyUnreadable)
	}
	return k, nil
}
