package secret

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

var testKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))

// 001-AC6: AES-256-GCM round trip; another AAD (row) or a flipped byte fails to open.
func TestSealOpen(t *testing.T) {
	box, err := New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("ghp_supersecret")
	ct, nonce := box.Seal(plain, []byte("row-1"))
	if bytes.Contains(ct, plain) {
		t.Fatal("ciphertext carries the plaintext")
	}
	got, err := box.Open(ct, nonce, []byte("row-1"))
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("round trip: %q %v", got, err)
	}
	if _, err := box.Open(ct, nonce, []byte("row-2")); err == nil {
		t.Fatal("wrong AAD opened")
	}
	tampered := bytes.Clone(ct)
	tampered[0] ^= 1
	if _, err := box.Open(tampered, nonce, []byte("row-1")); err == nil {
		t.Fatal("tampered ciphertext opened")
	}
	if _, err := box.Open(ct, nonce[:4], []byte("row-1")); err == nil {
		t.Fatal("short nonce opened")
	}
	ct2, nonce2 := box.Seal(plain, []byte("row-1"))
	if bytes.Equal(nonce, nonce2) || bytes.Equal(ct, ct2) {
		t.Fatal("nonce reused")
	}
	other, _ := New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	if _, err := other.Open(ct, nonce, []byte("row-1")); err == nil {
		t.Fatal("another key opened")
	}
}

func TestParseKey(t *testing.T) {
	for in, want := range map[string]string{
		"":     "empty",
		"!!":   "not base64",
		"AAAA": "3 bytes",
		base64.StdEncoding.EncodeToString(make([]byte, 16)): "16 bytes",
	} {
		if _, err := New(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", in, want, err)
		}
	}
	if _, err := ParseKey(testKey); err != nil {
		t.Fatal(err)
	}
}
