package webpush

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/secret"
	"github.com/gedour/gedour/pkg/testpg"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testpg.Terminate()
	os.Exit(code)
}

const testKey = "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc="

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := db.Connect(context.Background(), testpg.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

// 010-AC11: the key survives a restart (a second load), concurrent first starts agree on one key, and the
// row holds the private key sealed only.
func TestLoadOrCreate(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	box, _ := secret.New(testKey)
	keys := make([]*Key, 16)
	var wg sync.WaitGroup
	// Released together, so they all find no row and race on the insert.
	start := make(chan struct{})
	for i := range keys {
		wg.Go(func() {
			<-start
			k, err := LoadOrCreate(ctx, pool, box)
			if err != nil {
				t.Error(err)
			}
			keys[i] = k
		})
	}
	close(start)
	wg.Wait()
	if t.Failed() {
		return
	}
	for _, k := range keys[1:] {
		if k.PublicKey() != keys[0].PublicKey() {
			t.Fatal("concurrent first starts made different keys")
		}
	}
	again, err := LoadOrCreate(ctx, pool, box)
	if err != nil || again.PublicKey() != keys[0].PublicKey() {
		t.Fatalf("reload: %v", err)
	}
	var n int
	var ct []byte
	if err := pool.QueryRow(ctx, `SELECT count(*), min(private_ciphertext) FROM vapid_key`).Scan(&n, &ct); err != nil {
		t.Fatal(err)
	}
	raw, _ := again.privateBytes()
	if n != 1 || bytes.Contains(ct, raw) {
		t.Fatalf("%d rows, ciphertext holds the clear key: %v", n, bytes.Contains(ct, raw))
	}
	if _, err := pool.Exec(ctx, `INSERT INTO vapid_key (id, public_key, private_ciphertext, private_nonce, key_version) VALUES (false, '', '', '', 1)`); err == nil {
		t.Error("a second row was accepted")
	}

	other, _ := secret.New("CAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAg=")
	if _, err := LoadOrCreate(ctx, pool, other); !errors.Is(err, ErrKeyUnreadable) || !strings.Contains(err.Error(), "open secret") {
		t.Errorf("opened under another encryption key: %v", err)
	}
	junk, nonce := box.Seal([]byte{1, 2}, keyAAD)
	if _, err := pool.Exec(ctx, `UPDATE vapid_key SET private_ciphertext = $1, private_nonce = $2`, junk, nonce); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(ctx, pool, box); !errors.Is(err, ErrKeyUnreadable) || !strings.Contains(err.Error(), "invalid scalar length") {
		t.Errorf("loaded a sealed value that is not a P-256 scalar: %v", err)
	}
	ct, nonce = box.Seal(raw, keyAAD)
	if _, err := pool.Exec(ctx, `UPDATE vapid_key SET private_ciphertext = $1, private_nonce = $2, public_key = $3`, ct, nonce, make([]byte, keyLen)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(ctx, pool, box); !errors.Is(err, ErrKeyUnreadable) || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("loaded a row whose public key does not match: %v", err)
	}
	pool.Close()
	if _, err := LoadOrCreate(ctx, pool, box); err == nil || errors.Is(err, ErrKeyUnreadable) || !strings.Contains(err.Error(), "load vapid key") {
		t.Errorf("closed pool: %v", err)
	}
}

// A first start that cannot write the key (a read-only replica) fails instead of running without one.
func TestLoadOrCreateReadOnly(t *testing.T) {
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(testpg.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	rw, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	if _, err := db.Migrate(ctx, rw); err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	ro, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	box, _ := secret.New(testKey)
	if _, err := LoadOrCreate(ctx, ro, box); err == nil || errors.Is(err, ErrKeyUnreadable) || !strings.Contains(err.Error(), "store vapid key") {
		t.Fatalf("read-only first start: %v", err)
	}
}
