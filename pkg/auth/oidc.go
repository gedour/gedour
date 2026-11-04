package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
)

const (
	// loginCookie also seals the state (AEAD additional data); the cookie's name gets a prefix when Secure.
	loginCookie = "gedour_login"
	loginTTL    = 10 * time.Minute
)

// loginState survives the round trip to the provider, sealed in a cookie so any replica can finish it.
type loginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	ReturnTo string `json:"r"`
	Expires  int64  `json:"e"`
}

// Every replica shares the client secret, so the key needs no extra config.
func deriveStateKey(secret string) ([]byte, error) {
	key, err := hkdf.Key(sha256.New, []byte(secret), nil, "gedour login state cookie", 32)
	if err != nil {
		return nil, fmt.Errorf("derive state key: %w", err)
	}
	return key, nil
}

func (s *Service) aead() cipher.AEAD {
	block, _ := aes.NewCipher(s.stateKey) // key is always 32 bytes
	gcm, _ := cipher.NewGCM(block)
	return gcm
}

func (s *Service) seal(st loginState) string {
	plain, _ := json.Marshal(st)
	gcm := s.aead()
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	return base64.RawURLEncoding.EncodeToString(gcm.Seal(nonce, nonce, plain, []byte(loginCookie)))
}

var errBadLoginState = errors.New("login state missing, expired or tampered")

func (s *Service) open(value string) (loginState, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	gcm := s.aead()
	if err != nil || len(raw) < gcm.NonceSize() {
		return loginState{}, errBadLoginState
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], []byte(loginCookie))
	var st loginState
	if err != nil || json.Unmarshal(plain, &st) != nil || time.Now().Unix() > st.Expires {
		return loginState{}, errBadLoginState
	}
	return st, nil
}

// loginCookieName: Path=/auth/ rules out __Host-, __Secure- still forbids a plain-http overwrite.
func (s *Service) loginCookieName() string {
	if s.secure {
		return "__Secure-" + loginCookie
	}
	return loginCookie
}

func (s *Service) loginCookie(value string, maxAge int) *http.Cookie {
	// Lax: the callback is a top-level GET from the provider, which Lax still sends.
	return &http.Cookie{Name: s.loginCookieName(), Value: value, Path: "/auth/", MaxAge: maxAge, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode} //nolint:gosec // Secure is off only in dev mode (plain http://localhost)
}

// safeReturnTo keeps only same-origin absolute paths; anything else becomes "/" (no open redirect).
func safeReturnTo(raw string) string {
	// "//host" and "/\host" are protocol-relative for browsers; control chars can smuggle either.
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, "/\\") ||
		strings.ContainsAny(raw, "\\\r\n\t\x00") {
		return "/"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	return raw
}

// discoveryRetry is how long a failed discovery is answered from cache, so a down IdP is not hammered.
const discoveryRetry = 5 * time.Second

// discovery is what the provider's metadata tells us; immutable once built.
type discovery struct {
	verifier   *oidc.IDTokenVerifier
	endpoint   oauth2.Endpoint
	endSession string // "" when the provider has no RP-initiated logout
}

// endpoints returns the provider metadata, discovering it once. Concurrent callers share one discovery
// (singleflight), and the lock is never held across the network call.
func (s *Service) endpoints() (*discovery, error) {
	s.mu.Lock()
	d, failedAt, failErr := s.disc, s.discFailedAt, s.discErr
	s.mu.Unlock()
	if d != nil {
		return d, nil
	}
	if failErr != nil && time.Since(failedAt) < discoveryRetry {
		return nil, failErr
	}
	v, err, _ := s.discoverOnce.Do("discover", func() (any, error) {
		d, err := s.discover()
		s.mu.Lock()
		defer s.mu.Unlock()
		if err != nil {
			s.discFailedAt, s.discErr = time.Now(), err
			return nil, err
		}
		s.disc, s.discErr = d, nil
		return d, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*discovery), nil
}

func (s *Service) discover() (*discovery, error) {
	// Background, not the request ctx: the provider keeps it for later JWKS refreshes; s.client bounds the call.
	p, err := oidc.NewProvider(oidc.ClientContext(context.Background(), s.client), s.issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := p.Claims(&meta); err != nil {
		return nil, fmt.Errorf("oidc discovery metadata: %w", err)
	}
	return &discovery{
		verifier:   p.Verifier(&oidc.Config{ClientID: s.oauth.ClientID}),
		endpoint:   p.Endpoint(),
		endSession: meta.EndSession,
	}, nil
}

func (s *Service) oauthConfig(d *discovery) oauth2.Config {
	cfg := s.oauth
	cfg.Endpoint = d.endpoint
	return cfg
}

// handleLogin starts the authorization code + PKCE flow; in dev mode it signs the dev user in directly.
func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	returnTo := safeReturnTo(r.URL.Query().Get("return_to"))
	if s.DevMode() {
		if _, err := s.login(r.Context(), w, r, s.devIdentity()); err != nil {
			s.log.Error("dev_login_failed", zap.Error(err))
			http.Error(w, "sign-in failed", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, returnTo, http.StatusSeeOther) //nolint:gosec // returnTo went through safeReturnTo
		return
	}
	d, err := s.endpoints()
	if err != nil {
		s.log.Error("oidc_unavailable", zap.Error(err))
		http.Error(w, "identity provider unavailable, try again later", http.StatusServiceUnavailable)
		return
	}
	st := loginState{State: randomToken(), Nonce: randomToken(), Verifier: oauth2.GenerateVerifier(),
		ReturnTo: returnTo, Expires: time.Now().Add(loginTTL).Unix()}
	http.SetCookie(w, s.loginCookie(s.seal(st), int(loginTTL.Seconds())))
	cfg := s.oauthConfig(d)
	http.Redirect(w, r, cfg.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)), http.StatusFound)
}

// handleCallback checks state, exchanges the code with the PKCE verifier, checks the nonce, then signs in.
func (s *Service) handleCallback(w http.ResponseWriter, r *http.Request) {
	if s.DevMode() {
		http.NotFound(w, r)
		return
	}
	// One-shot: the state cookie is cleared whatever the outcome.
	http.SetCookie(w, s.loginCookie("", -1))
	fail := func(code int, msg string, err error) {
		s.log.Warn("login_rejected", zap.String("reason", msg), zap.Error(err))
		http.Error(w, msg+": start again from the sign-in page", code)
	}
	c, err := r.Cookie(s.loginCookieName())
	if err != nil {
		fail(http.StatusBadRequest, "login state missing", err)
		return
	}
	st, err := s.open(c.Value)
	if err != nil {
		fail(http.StatusBadRequest, "login state invalid", err)
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		fail(http.StatusUnauthorized, "provider refused the sign-in", errors.New(e))
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		fail(http.StatusBadRequest, "state mismatch", nil)
		return
	}
	code := q.Get("code")
	if code == "" {
		fail(http.StatusBadRequest, "authorization code missing", nil)
		return
	}
	d, err := s.endpoints()
	if err != nil {
		fail(http.StatusServiceUnavailable, "identity provider unavailable", err)
		return
	}
	ctx := oidc.ClientContext(r.Context(), s.client)
	cfg := s.oauthConfig(d)
	tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		fail(http.StatusUnauthorized, "code exchange failed", err)
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	idt, err := d.verifier.Verify(ctx, rawID)
	if err != nil {
		fail(http.StatusUnauthorized, "invalid ID token", err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(st.Nonce)) != 1 {
		fail(http.StatusUnauthorized, "nonce mismatch", nil)
		return
	}
	var claims struct {
		Email             string `json:"email"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
	}
	if err := idt.Claims(&claims); err != nil {
		fail(http.StatusUnauthorized, "invalid ID token claims", err)
		return
	}
	who := identity{Sub: idt.Subject, Email: claims.Email, DisplayName: claims.Name, IDToken: rawID}
	if who.DisplayName == "" {
		who.DisplayName = claims.PreferredUsername
	}
	if _, err := s.login(r.Context(), w, r, who); err != nil {
		s.log.Error("login_failed", zap.Error(err))
		http.Error(w, "sign-in failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, safeReturnTo(st.ReturnTo), http.StatusSeeOther)
}

// logoutRedirect is where the browser goes after sign-out: the provider's end-session endpoint
// (RP-initiated logout, so the IdP session ends too), else the signed-out page.
func (s *Service) logoutRedirect(sess *session) string {
	if s.DevMode() || sess == nil {
		return SignedOutPath
	}
	// This replica may never have run a login: discover now, or the IdP session would survive.
	d, err := s.endpoints()
	if err != nil {
		s.log.Warn("oidc_unavailable_on_logout", zap.Error(err))
		return SignedOutPath
	}
	u, err := url.Parse(d.endSession)
	if d.endSession == "" || err != nil {
		return SignedOutPath
	}
	q := u.Query()
	q.Set("client_id", s.oauth.ClientID)
	q.Set("post_logout_redirect_uri", s.publicURL+SignedOutPath)
	if sess.idToken != "" {
		q.Set("id_token_hint", sess.idToken)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// handleLogout ends the session (CSRF-checked), clears the cookie and answers where to send the browser.
// Without a session it only points at the signed-out page.
func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	sess, err := s.current(r)
	if err != nil {
		s.log.Error("session_lookup_failed", zap.Error(err))
		writeProblem(w, http.StatusInternalServerError, "session lookup failed")
		return
	}
	if sess != nil {
		if !csrfOK(r, sess) {
			writeCSRFProblem(w)
			return
		}
		if err := s.logout(r.Context(), sess); err != nil {
			s.log.Error("logout_failed", zap.Error(err))
			writeProblem(w, http.StatusInternalServerError, "logout failed")
			return
		}
	}
	http.SetCookie(w, s.sessionCookie("", -1))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"redirect": s.logoutRedirect(sess)})
}
