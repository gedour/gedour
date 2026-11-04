package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// stubProvider is a minimal OIDC provider: discovery, JWKS, authorize (auto-approves) and token,
// with real PKCE S256 verification and knobs to forge a bad nonce.
type stubProvider struct {
	*httptest.Server
	t       *testing.T
	key     *rsa.PrivateKey
	sub     string
	email   string
	name    string
	nonceFn func(string) string // rewrites the nonce put in the ID token
	// noEndSession hides end_session_endpoint from discovery, like a provider without RP-initiated logout.
	noEndSession bool
	// discoveryDelay slows the metadata endpoint, like a sluggish IdP; discoveries counts its hits.
	discoveryDelay time.Duration
	discoveries    atomic.Int32

	mu    sync.Mutex
	codes map[string]authRequest
	seen  url.Values // last authorize query
}

type authRequest struct {
	nonce, challenge, redirect string
}

func newStub(t *testing.T) *stubProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &stubProvider{t: t, key: key, sub: "alice-sub", email: "alice@example.test", name: "Alice",
		nonceFn: func(n string) string { return n }, codes: map[string]authRequest{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		p.discoveries.Add(1)
		time.Sleep(p.discoveryDelay)
		meta := map[string]any{
			"issuer":                                p.URL,
			"authorization_endpoint":                p.URL + "/authorize",
			"token_endpoint":                        p.URL + "/token",
			"jwks_uri":                              p.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		if !p.noEndSession {
			meta["end_session_endpoint"] = p.URL + "/logout?ui_locales=en"
		}
		_ = json.NewEncoder(w).Encode(meta)
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := randomToken()
		p.mu.Lock()
		p.seen = q
		p.codes[code] = authRequest{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
		p.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound) //nolint:gosec // test provider, redirect_uri checked at /token
	})
	mux.HandleFunc("POST /token", p.token)
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Close)
	return p
}

func (p *stubProvider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	req, ok := p.codes[r.PostForm.Get("code")]
	delete(p.codes, r.PostForm.Get("code"))
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != req.challenge || r.PostForm.Get("redirect_uri") != req.redirect {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	clientID, _, _ := r.BasicAuth()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		p.t.Error(err)
		return
	}
	now := time.Now()
	idToken, err := jwt.Signed(signer).Claims(map[string]any{
		"iss": p.URL, "sub": p.sub, "aud": clientID, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
		"nonce": p.nonceFn(req.nonce), "email": p.email, "name": p.name, "preferred_username": "alice",
	}).Serialize()
	if err != nil {
		p.t.Error(err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": idToken, "expires_in": 60})
}
