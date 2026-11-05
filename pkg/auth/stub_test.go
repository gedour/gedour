package auth

import (
	"cmp"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
	// extra claims added to the ID token, like the roles a provider maps.
	extra map[string]any
	// noEndSession hides end_session_endpoint from discovery, like a provider without RP-initiated logout.
	noEndSession bool
	// discoveryDelay slows the metadata endpoint, like a sluggish IdP; discoveries counts its hits.
	discoveryDelay time.Duration
	discoveries    atomic.Int32
	// tokenStatus, when set, makes /token answer that status with tokenBody (JSON), like a broken endpoint.
	tokenStatus int
	tokenBody   string
	// authorizeStatus, when set, is /authorize's answer, like a provider in an outage; probeDelay slows
	// the parameterless calls (probes), probes counts them.
	authorizeStatus int
	probeDelay      time.Duration
	probes          atomic.Int32
	// probeRedirect sends parameterless calls elsewhere, like a provider redirecting to its login page.
	probeRedirect string
	// authorizeURL replaces the advertised authorization endpoint, like one on another host.
	authorizeURL string
	// tokenStall makes /token send its headers and part of the body, then stall this long.
	tokenStall time.Duration
	// tokenStallParams refuses the Basic auth attempt at once (401), then stalls the params attempt
	// oauth2 retries with, after its headers and part of its body.
	tokenStallParams time.Duration
	// jwksStall delays the key set, like a provider stalling the ID token check.
	jwksStall time.Duration
	// conns counts the connections opened to the provider.
	conns atomic.Int32

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
			"authorization_endpoint":                cmp.Or(p.authorizeURL, p.URL+"/authorize"),
			"token_endpoint":                        p.URL + "/token",
			"jwks_uri":                              p.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		if !p.noEndSession {
			meta["end_session_endpoint"] = p.URL + "/logout?ui_locales=en"
		}
		_ = json.NewEncoder(w).Encode(meta)
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(p.jwksStall):
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if p.authorizeStatus != 0 {
			w.WriteHeader(p.authorizeStatus)
			return
		}
		if q.Get("redirect_uri") == "" && p.probeRedirect != "" {
			p.probes.Add(1)
			http.Redirect(w, r, p.probeRedirect, http.StatusFound)
			return
		}
		if q.Get("redirect_uri") == "" {
			p.probes.Add(1)
			time.Sleep(p.probeDelay)
			// A login-page-sized answer: connection reuse must hold for a real body, not an empty one.
			http.Error(w, strings.Repeat("missing parameters ", 160), http.StatusBadRequest)
			return
		}
		code := randomToken()
		p.mu.Lock()
		p.seen = q
		p.codes[code] = authRequest{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
		p.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound) //nolint:gosec // test provider, redirect_uri checked at /token
	})
	mux.HandleFunc("POST /token", p.token)
	p.Server = httptest.NewUnstartedServer(mux)
	p.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			p.conns.Add(1)
		}
	}
	p.Start()
	t.Cleanup(p.Close)
	return p
}

func (p *stubProvider) token(w http.ResponseWriter, r *http.Request) {
	stall := p.tokenStall
	if p.tokenStallParams != 0 {
		if _, _, basic := r.BasicAuth(); basic {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		stall = p.tokenStallParams
	}
	if stall != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"at",`))
		w.(http.Flusher).Flush()
		select {
		case <-time.After(stall):
		case <-r.Context().Done():
		}
		return
	}
	if p.tokenStatus != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(p.tokenStatus)
		_, _ = w.Write([]byte(p.tokenBody))
		return
	}
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
	claims := map[string]any{
		"iss": p.URL, "sub": p.sub, "aud": clientID, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
		"nonce": p.nonceFn(req.nonce), "email": p.email, "name": p.name, "preferred_username": "alice",
	}
	maps.Copy(claims, p.extra)
	idToken, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		p.t.Error(err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": idToken, "expires_in": 60})
}
