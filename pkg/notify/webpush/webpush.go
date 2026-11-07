// Package webpush holds the Web Push crypto: RFC 8291 aes128gcm payload encryption and RFC 8292 VAPID.
package webpush

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"
)

const (
	recordSize = 4096
	saltLen    = 16
	keyLen     = 65 // uncompressed P-256 point
	headerLen  = saltLen + 4 + 1 + keyLen
	// MaxPayload fits the whole body in one 4096-byte record: push services refuse larger bodies.
	MaxPayload = recordSize - headerLen - 1 - 16
	// JWTLifetime stays under RFC 8292's 24 h cap, with margin for a push service's clock skew.
	JWTLifetime = 12 * time.Hour
)

// ErrTooLarge is returned for a payload over MaxPayload.
var ErrTooLarge = errors.New("payload too large")

var b64 = base64.RawURLEncoding

// Subscription is what a browser's PushSubscription gives: its endpoint and keys.
type Subscription struct {
	Endpoint string
	P256DH   []byte // the user agent's public key, uncompressed point
	Auth     []byte // the 16-byte auth secret
}

// Encrypt seals plaintext for sub under a fresh ephemeral key and salt (RFC 8291).
func Encrypt(plaintext []byte, sub Subscription) ([]byte, error) {
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ephemeral key: %w", err)
	}
	salt := make([]byte, saltLen)
	_, _ = rand.Read(salt) // crypto/rand.Read never fails (Go 1.24+)
	return encrypt(plaintext, sub.P256DH, sub.Auth, as, salt)
}

func encrypt(plaintext, uaPublic, auth []byte, as *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(plaintext) > MaxPayload {
		return nil, fmt.Errorf("%w: %d bytes, at most %d", ErrTooLarge, len(plaintext), MaxPayload)
	}
	// Another length still gets a 201 from the push service, but the browser cannot decrypt the message.
	if len(auth) != 16 {
		return nil, fmt.Errorf("subscription auth secret: %d bytes, want 16", len(auth))
	}
	ua, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("subscription key: %w", err)
	}
	gcm, nonce, err := contentKey(as, ua, uaPublic, as.PublicKey().Bytes(), auth, salt)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, headerLen+len(plaintext)+17)
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, recordSize)
	out = append(out, keyLen)
	out = append(out, as.PublicKey().Bytes()...)
	// 0x02 delimits the last (here, only) record; no padding.
	return gcm.Seal(out, nonce, append(plaintext[:len(plaintext):len(plaintext)], 0x02), nil), nil
}

// contentKey derives the AES-128-GCM key and nonce from the ECDH secret between priv and peer.
func contentKey(priv *ecdh.PrivateKey, peer *ecdh.PublicKey, uaPublic, asPublic, auth, salt []byte) (cipher.AEAD, []byte, error) {
	secret, err := priv.ECDH(peer)
	if err != nil {
		return nil, nil, fmt.Errorf("ecdh: %w", err)
	}
	info := append(append([]byte("WebPush: info\x00"), uaPublic...), asPublic...)
	ikm, err1 := hkdf.Key(sha256.New, secret, auth, string(info), 32)
	cek, err2 := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, err3 := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err := errors.Join(err1, err2, err3); err != nil {
		return nil, nil, fmt.Errorf("hkdf: %w", err)
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, nil, fmt.Errorf("aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("gcm: %w", err)
	}
	return gcm, nonce, nil
}

// Decrypt opens a single-record aes128gcm body with the user agent's key: what a browser does.
func Decrypt(body []byte, ua *ecdh.PrivateKey, auth []byte) ([]byte, error) {
	if len(body) < headerLen || body[saltLen+4] != keyLen {
		return nil, errors.New("decrypt: bad header")
	}
	salt, asPublic := body[:saltLen], body[saltLen+5:headerLen]
	if rs := binary.BigEndian.Uint32(body[saltLen:]); len(body)-headerLen > int(rs) {
		return nil, errors.New("decrypt: more than one record")
	}
	as, err := ecdh.P256().NewPublicKey(asPublic)
	if err != nil {
		return nil, fmt.Errorf("decrypt: sender key: %w", err)
	}
	gcm, nonce, err := contentKey(ua, as, ua.PublicKey().Bytes(), asPublic, auth, salt)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, body[headerLen:], nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	plain = bytes.TrimRight(plain, "\x00")
	if len(plain) == 0 || plain[len(plain)-1] != 0x02 {
		return nil, errors.New("decrypt: no last-record delimiter")
	}
	return plain[:len(plain)-1], nil
}

// Key is the server's VAPID key pair (ECDSA P-256).
type Key struct {
	priv *ecdsa.PrivateKey
}

// GenerateKey returns a new VAPID key pair.
func GenerateKey() (*Key, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("vapid key: %w", err)
	}
	return &Key{priv: priv}, nil
}

// ParseKey reads a raw 32-byte private scalar.
func ParseKey(raw []byte) (*Key, error) {
	priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
	if err != nil {
		return nil, fmt.Errorf("vapid key: %w", err)
	}
	return &Key{priv: priv}, nil
}

func (k *Key) privateBytes() ([]byte, error) {
	raw, err := k.priv.Bytes()
	if err != nil {
		return nil, fmt.Errorf("vapid key: %w", err)
	}
	return raw, nil
}

func (k *Key) publicBytes() []byte {
	raw, _ := k.priv.PublicKey.Bytes() // a P-256 key from GenerateKey or ParseRawPrivateKey always encodes
	return raw
}

// PublicKey is the uncompressed public point, base64url without padding: the browser's applicationServerKey.
func (k *Key) PublicKey() string { return b64.EncodeToString(k.publicBytes()) }

type jwtClaims struct {
	Aud string `json:"aud"`
	Exp int64  `json:"exp"`
	Sub string `json:"sub,omitempty"`
}

var jwtHeader = b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))

// Origin is the JWT audience for endpoint: its scheme and host.
func Origin(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("push endpoint: not an absolute URL")
	}
	return u.Scheme + "://" + u.Host, nil
}

// Authorization is the RFC 8292 header value for a push to endpoint; sub is the contact, empty to omit.
func (k *Key) Authorization(endpoint, sub string, now time.Time) (string, error) {
	aud, err := Origin(endpoint)
	if err != nil {
		return "", err
	}
	claims, _ := json.Marshal(jwtClaims{Aud: aud, Exp: now.Add(JWTLifetime).Unix(), Sub: sub}) // strings and an int always encode
	token, err := k.sign(jwtHeader + "." + b64.EncodeToString(claims))
	if err != nil {
		return "", err
	}
	return "vapid t=" + token + ", k=" + k.PublicKey(), nil
}

// sign appends the ES256 signature (raw r || s, RFC 7518 section 3.4) to a JWT signing input.
func (k *Key) sign(signing string) (string, error) {
	h := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, h[:])
	if err != nil {
		return "", fmt.Errorf("vapid sign: %w", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + b64.EncodeToString(sig), nil
}

// ParseAuthorization splits a "vapid t=<jwt>, k=<key>" header value.
func ParseAuthorization(v string) (token, key string, err error) {
	rest, ok := strings.CutPrefix(v, "vapid ")
	if !ok {
		return "", "", errors.New("authorization: not vapid")
	}
	for p := range strings.SplitSeq(rest, ",") {
		name, val, _ := strings.Cut(strings.TrimSpace(p), "=")
		switch name {
		case "t":
			token = val
		case "k":
			key = val
		}
	}
	if token == "" || key == "" {
		return "", "", errors.New("authorization: t or k missing")
	}
	return token, key, nil
}

// VerifyJWT checks a VAPID token as a push service does: ES256 under the base64url public key, audience, expiry.
func VerifyJWT(token, publicKey, aud string, now time.Time) error {
	pubRaw, err := b64.DecodeString(publicKey)
	if err != nil {
		return fmt.Errorf("vapid key: %w", err)
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pubRaw)
	if err != nil {
		return fmt.Errorf("vapid key: %w", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("jwt: not three parts")
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return errors.New("jwt: bad signature encoding")
	}
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return errors.New("jwt: bad signature")
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	if raw, err := b64.DecodeString(parts[0]); err != nil || json.Unmarshal(raw, &hdr) != nil || hdr.Alg != "ES256" {
		return errors.New("jwt: bad header")
	}
	var c jwtClaims
	if raw, err := b64.DecodeString(parts[1]); err != nil || json.Unmarshal(raw, &c) != nil {
		return errors.New("jwt: bad claims")
	}
	switch exp := time.Unix(c.Exp, 0); {
	case c.Aud != aud:
		return fmt.Errorf("jwt: audience %q, want %q", c.Aud, aud)
	case !exp.After(now):
		return errors.New("jwt: expired")
	case exp.After(now.Add(24 * time.Hour)):
		return errors.New("jwt: expires more than 24 h ahead")
	}
	return nil
}
