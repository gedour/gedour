package webpush

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func unb64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// RFC 8291 section 5.
const (
	rfcPlaintext = "When I grow up, I want to be a watermelon"
	rfcASPrivate = "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"
	rfcUAPrivate = "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"
	rfcUAPublic  = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	rfcSalt      = "DGv6ra1nlYgDCS1FRnbzlw"
	rfcAuth      = "BTBZMqHH6r4Tts7J_aSIgg"
	rfcBody      = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
)

// 010-AC6: encryption with the RFC's server key and salt gives the RFC's body byte for byte, and it decrypts back.
func TestRFC8291Vector(t *testing.T) {
	as, err := ecdh.P256().NewPrivateKey(unb64(t, rfcASPrivate))
	if err != nil {
		t.Fatal(err)
	}
	got, err := encrypt([]byte(rfcPlaintext), unb64(t, rfcUAPublic), unb64(t, rfcAuth), as, unb64(t, rfcSalt))
	if err != nil {
		t.Fatal(err)
	}
	if want := unb64(t, rfcBody); !bytes.Equal(got, want) {
		t.Fatalf("body\n got %s\nwant %s", base64.RawURLEncoding.EncodeToString(got), rfcBody)
	}
	ua, err := ecdh.P256().NewPrivateKey(unb64(t, rfcUAPrivate))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Decrypt(unb64(t, rfcBody), ua, unb64(t, rfcAuth))
	if err != nil || string(plain) != rfcPlaintext {
		t.Fatalf("decrypt: %q, %v", plain, err)
	}
}

// 010-AC6: a random-key encryption decrypts with the subscription's keys only; size and tampering are refused.
func TestEncryptDecrypt(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := bytes.Repeat([]byte{7}, 16)
	sub := Subscription{Endpoint: "https://push.example/x", P256DH: ua.PublicKey().Bytes(), Auth: auth}
	max := bytes.Repeat([]byte("a"), MaxPayload)
	body, err := Encrypt(max, sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != recordSize {
		t.Errorf("max payload body is %d bytes, want %d", len(body), recordSize)
	}
	if plain, err := Decrypt(body, ua, auth); err != nil || !bytes.Equal(plain, max) {
		t.Fatalf("decrypt: %v", err)
	}
	if _, err := Encrypt(append(max, 'a'), sub); !errors.Is(err, ErrTooLarge) {
		t.Errorf("over max: %v", err)
	}
	other, _ := ecdh.P256().GenerateKey(rand.Reader)
	edit := func(f func([]byte)) []byte { b := bytes.Clone(body); f(b); return b }
	flipped := edit(func(b []byte) { b[len(b)-1] ^= 1 })
	smallRecord := edit(func(b []byte) { b[saltLen+2] = 0 })
	noSenderKey := edit(func(b []byte) { clear(b[saltLen+5 : headerLen]) })
	for name, tt := range map[string]struct {
		try  func() error
		want string
	}{
		"other key":     {func() error { _, err := Decrypt(body, other, auth); return err }, "message authentication failed"},
		"other auth":    {func() error { _, err := Decrypt(body, ua, make([]byte, 16)); return err }, "message authentication failed"},
		"flipped byte":  {func() error { _, err := Decrypt(flipped, ua, auth); return err }, "message authentication failed"},
		"short":         {func() error { _, err := Decrypt(body[:headerLen-1], ua, auth); return err }, "bad header"},
		"two records":   {func() error { _, err := Decrypt(smallRecord, ua, auth); return err }, "more than one record"},
		"no sender key": {func() error { _, err := Decrypt(noSenderKey, ua, auth); return err }, "sender key"},
		"bad sub key": {func() error {
			_, err := Encrypt([]byte("x"), Subscription{P256DH: []byte{4, 1}, Auth: auth})
			return err
		}, "subscription key"},
		"compressed sub key": {func() error {
			pub := ua.PublicKey().Bytes()
			_, err := Encrypt([]byte("x"), Subscription{P256DH: append([]byte{2 + pub[64]&1}, pub[1:33]...), Auth: auth})
			return err
		}, "subscription key"},
		"short auth": {func() error {
			_, err := Encrypt([]byte("x"), Subscription{P256DH: ua.PublicKey().Bytes(), Auth: auth[:15]})
			return err
		}, "auth secret: 15 bytes"},
		"long auth": {func() error {
			_, err := Encrypt([]byte("x"), Subscription{P256DH: ua.PublicKey().Bytes(), Auth: append(bytes.Clone(auth), 0)})
			return err
		}, "auth secret: 17 bytes"},
	} {
		if err := tt.try(); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", name, err, tt.want)
		}
	}
}

// 010-AC6: the VAPID token verifies against the served public key, for the endpoint's origin, and nothing else does.
func TestVAPID(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	h, err := k.Authorization("https://push.example:8443/send/abc?x=1", "https://gedour.example", now)
	if err != nil {
		t.Fatal(err)
	}
	token, key, err := ParseAuthorization(h)
	if err != nil {
		t.Fatal(err)
	}
	if key != k.PublicKey() || len(unb64(t, key)) != 65 || unb64(t, key)[0] != 4 {
		t.Fatalf("k=%q, served %q", key, k.PublicKey())
	}
	claims := string(unb64(t, strings.Split(token, ".")[1]))
	if !strings.Contains(claims, `"sub":"https://gedour.example"`) {
		t.Errorf("claims %s", claims)
	}
	other, _ := GenerateKey()
	for _, tt := range []struct {
		name, token, key, aud string
		at                    time.Time
		ok                    bool
	}{
		{"valid", token, key, "https://push.example:8443", now, true},
		{"other key", token, other.PublicKey(), "https://push.example:8443", now, false},
		{"other audience", token, key, "https://push.example", now, false},
		{"expired", token, key, "https://push.example:8443", now.Add(JWTLifetime), false},
		{"over 24 h ahead", token, key, "https://push.example:8443", now.Add(-13 * time.Hour), false},
		{"tampered claims", strings.Replace(token, ".", ".e", 1), key, "https://push.example:8443", now, false},
		{"two parts", "a.b", key, "https://push.example:8443", now, false},
		{"bad key", token, "!!", "https://push.example:8443", now, false},
	} {
		if err := VerifyJWT(tt.token, tt.key, tt.aud, tt.at); (err == nil) != tt.ok {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
	if _, err := k.Authorization("/relative", "", now); err == nil {
		t.Error("relative endpoint accepted")
	}
	for _, v := range []string{"Bearer x", "vapid t=a", "vapid k=b"} {
		if _, _, err := ParseAuthorization(v); err == nil {
			t.Errorf("%q parsed", v)
		}
	}
}

func TestParseKey(t *testing.T) {
	k, _ := GenerateKey()
	raw, err := k.privateBytes()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseKey(raw)
	if err != nil || back.PublicKey() != k.PublicKey() {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := ParseKey([]byte{1, 2}); err == nil {
		t.Error("short key parsed")
	}
}

// 010-AC6: a body sealed without the last-record delimiter is refused, as a browser does.
func TestDecryptNoDelimiter(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	as, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth, salt := make([]byte, 16), make([]byte, saltLen)
	gcm, nonce, err := contentKey(as, ua.PublicKey(), ua.PublicKey().Bytes(), as.PublicKey().Bytes(), auth, salt)
	if err != nil {
		t.Fatal(err)
	}
	body := append(append(salt, 0, 0, 0x10, 0, keyLen), as.PublicKey().Bytes()...)
	for _, record := range []string{"no delimiter", "padded\x00\x00", ""} {
		sealed := gcm.Seal(bytes.Clone(body), nonce, []byte(record), nil)
		if _, err := Decrypt(sealed, ua, auth); err == nil || !strings.Contains(err.Error(), "delimiter") {
			t.Errorf("%q: %v", record, err)
		}
	}
}

// 010-AC6: a token signed by the right key but with a wrong header or unreadable claims is refused.
func TestVerifyJWTShapes(t *testing.T) {
	k, _ := GenerateKey()
	enc := func(s string) string { return b64.EncodeToString([]byte(s)) }
	signed := func(hdr, claims string) string {
		tok, err := k.sign(hdr + "." + claims)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	now := time.Now()
	claims := enc(`{"aud":"https://p.example","exp":` + strconv.FormatInt(now.Add(time.Hour).Unix(), 10) + `}`)
	for _, tt := range []struct{ name, token, key, want string }{
		{"valid", signed(jwtHeader, claims), k.PublicKey(), ""},
		{"alg none", signed(enc(`{"alg":"none"}`), claims), k.PublicKey(), "bad header"},
		{"header not base64", signed("!", claims), k.PublicKey(), "bad header"},
		{"claims not json", signed(jwtHeader, enc("[")), k.PublicKey(), "bad claims"},
		{"short signature", jwtHeader + "." + claims + ".AAAA", k.PublicKey(), "bad signature encoding"},
		{"key not a point", signed(jwtHeader, claims), enc("not a point"), "vapid key"},
	} {
		err := VerifyJWT(tt.token, tt.key, "https://p.example", now)
		if (tt.want == "" && err != nil) || (tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want))) {
			t.Errorf("%s: %v, want %q", tt.name, err, tt.want)
		}
	}
}
