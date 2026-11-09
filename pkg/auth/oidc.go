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
	"net"
	"net/http"
	"net/url"
	"slices"
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

// Reasons the sign-in gate shows after a failed sign-in; fixed values, never provider text.
const (
	authUnavailable = "unavailable"
	authFailed      = "failed"
	authForbidden   = "forbidden"
)

// gateURL is returnTo (already safe) with the gate's reason in its query. Path, other query pairs and
// fragment are kept byte for byte: re-encoding would drop pairs url.ParseQuery refuses (";", "%zz").
func gateURL(returnTo, reason string) string {
	rest, fragment, hasFragment := strings.Cut(returnTo, "#")
	path, rawQuery, _ := strings.Cut(rest, "?")
	var pairs []string
	for _, p := range strings.Split(rawQuery, "&") {
		if p != "" && pairKey(p) != "auth" {
			pairs = append(pairs, p)
		}
	}
	out := path + "?" + strings.Join(append(pairs, "auth="+reason), "&")
	if hasFragment {
		out += "#" + fragment
	}
	return out
}

// pairKey decodes a raw query pair's key the way the browser reads it, so "%61uth" is the reason too.
func pairKey(pair string) string {
	key, _, _ := strings.Cut(pair, "=")
	if k, err := url.QueryUnescape(key); err == nil {
		return k
	}
	return key
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

// probeTimeout keeps a login from hanging on a provider that accepts connections but never answers.
const probeTimeout = 3 * time.Second

// reachable reports whether the provider's authorization endpoint answers now. Discovery is cached for
// good, so without this a provider gone down later would get the browser sent to a dead page (002-AC5).
// Concurrent logins share one probe; every later login probes afresh.
// ponytail: the server's view, not the browser's (a split network passes), and a provider slower than
// probeTimeout counts as down. An authorization endpoint on another host or port than the issuer is not probed
// (the server may not reach what browsers do), so its outage still reaches the browser; a browser-side
// check would be needed to close these gaps.
func (s *Service) reachable(ctx context.Context, d *discovery) error {
	if !sameHost(d.endpoint.AuthURL, s.issuer) {
		return nil
	}
	_, err, _ := s.discoverOnce.Do("probe", func() (any, error) {
		return nil, s.probe(context.WithoutCancel(ctx), d.endpoint.AuthURL)
	})
	return err
}

// probe calls the authorization endpoint without parameters: any answer below 500 (typically 400 for
// the missing parameters) proves it serves.
func (s *Service) probe(ctx context.Context, authURL string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authURL, nil)
	if err != nil {
		return fmt.Errorf("oidc probe: %w", err)
	}
	// A redirect (to a login page) is already an answer; following it would probe another host.
	c := *s.client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("oidc probe: %w", err)
	}
	// Close alone keeps the connection reusable: the transport drains up to 256 KiB within 50 ms itself.
	_ = resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("oidc probe: status %d", resp.StatusCode)
	}
	return nil
}

func sameHost(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	return errA == nil && errB == nil && ua.Host != "" && strings.EqualFold(ua.Host, ub.Host)
}

// seeOther redirects to a target safeReturnTo already checked. Not http.Redirect: it path.Cleans the
// target, fragment included when there is no query ("/w#a/../b" would become "/b").
func seeOther(w http.ResponseWriter, target string) {
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusSeeOther)
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

// oauthConfig is a per-request copy: login and callback run on the same host (the state cookie is
// host-bound), so both pick the same redirect URI.
func (s *Service) oauthConfig(d *discovery, r *http.Request) oauth2.Config {
	cfg := s.oauth
	cfg.Endpoint = d.endpoint
	cfg.RedirectURL = s.publicURL(r) + "/auth/callback"
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
		seeOther(w, returnTo)
		return
	}
	d, err := s.endpoints()
	if err == nil {
		err = s.reachable(r.Context(), d)
	}
	if err != nil {
		s.log.Error("oidc_unavailable", zap.Error(err))
		seeOther(w, gateURL(returnTo, authUnavailable))
		return
	}
	st := loginState{State: randomToken(), Nonce: randomToken(), Verifier: oauth2.GenerateVerifier(),
		ReturnTo: returnTo, Expires: time.Now().Add(loginTTL).Unix()}
	http.SetCookie(w, s.loginCookie(s.seal(st), int(loginTTL.Seconds())))
	cfg := s.oauthConfig(d, r)
	http.Redirect(w, r, cfg.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)), http.StatusFound) //nolint:gosec // provider URL; r.Host only selects a configured redirect_uri
}

// handleCallback checks state, exchanges the code with the PKCE verifier, checks the nonce, then signs in.
func (s *Service) handleCallback(w http.ResponseWriter, r *http.Request) {
	if s.DevMode() {
		http.NotFound(w, r)
		return
	}
	// One-shot: the state cookie is cleared whatever the outcome.
	http.SetCookie(w, s.loginCookie("", -1))
	// Until the login state is read, the page the user asked for is unknown.
	returnTo := "/"
	fail := func(reason, msg string, err error) {
		s.log.Warn("login_rejected", zap.String("reason", msg), zap.Error(err))
		seeOther(w, gateURL(returnTo, reason))
	}
	c, err := r.Cookie(s.loginCookieName())
	if err != nil {
		fail(authFailed, "login state missing", err)
		return
	}
	st, err := s.open(c.Value)
	if err != nil {
		fail(authFailed, "login state invalid", err)
		return
	}
	returnTo = safeReturnTo(st.ReturnTo)
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		fail(authFailed, "provider refused the sign-in", errors.New(e))
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		fail(authFailed, "state mismatch", nil)
		return
	}
	code := q.Get("code")
	if code == "" {
		fail(authFailed, "authorization code missing", nil)
		return
	}
	d, err := s.endpoints()
	if err != nil {
		fail(authUnavailable, "identity provider unavailable", err)
		return
	}
	// A stall ends at the deadline. oauth2 may then retry the token call (params after Basic auth) with
	// the expired ctx, or lose the timeout's type while reading a body: read the deadline, not the error.
	ctx, cancel := context.WithTimeout(oidc.ClientContext(r.Context(), s.client), s.exchangeTimeout)
	defer cancel()
	timedOut := func(reason string) string {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return authUnavailable
		}
		return reason
	}
	cfg := s.oauthConfig(d, r)
	tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		fail(timedOut(exchangeReason(err)), "code exchange failed", err)
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	idt, err := d.verifier.Verify(ctx, rawID)
	if err != nil {
		fail(timedOut(authFailed), "invalid ID token", err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(st.Nonce)) != 1 {
		fail(authFailed, "nonce mismatch", nil)
		return
	}
	var claims struct {
		Email             string `json:"email"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
	}
	if err := idt.Claims(&claims); err != nil {
		fail(authFailed, "invalid ID token claims", err)
		return
	}
	var all map[string]any
	if err := idt.Claims(&all); err != nil {
		fail(authFailed, "invalid ID token claims", err)
		return
	}
	roles := rolesFrom(all, s.rolesClaim)
	who := identity{Sub: idt.Subject, Email: claims.Email, DisplayName: claims.Name, IDToken: rawID,
		Admin: s.adminRole != "" && slices.Contains(roles, s.adminRole)}
	if who.DisplayName == "" {
		who.DisplayName = claims.PreferredUsername
	}
	// 006-AC4: the admin role alone grants sign-in, so an admin cannot lock themselves out.
	if s.requiredRole != "" && !who.Admin && !slices.Contains(roles, s.requiredRole) {
		s.deny(r.Context(), who.Sub, deniedMissingRole)
		fail(authForbidden, "required role missing", nil)
		return
	}
	if _, err := s.login(r.Context(), w, r, who); err != nil {
		// 006-AC7: checked in the login tx under the user row lock, so no session survives a racing disable.
		if errors.Is(err, errAccountDisabled) {
			s.deny(r.Context(), who.Sub, deniedDisabled)
			fail(authForbidden, "account disabled", nil)
			return
		}
		s.log.Error("login_failed", zap.Error(err))
		seeOther(w, gateURL(returnTo, authFailed))
		return
	}
	seeOther(w, returnTo)
}

// denyTimeout bounds a refusal's write once it no longer follows the request.
const denyTimeout = 10 * time.Second

// deny audits a refused sign-in. It outlives the request, as a dropped callback must not lose a suspension; a failure
// is logged with the subject (keys and sessions stay until the next refusal) and the refusal stands.
func (s *Service) deny(ctx context.Context, sub, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), denyTimeout)
	defer cancel()
	if err := s.auditDenied(ctx, sub, reason); err != nil {
		event := "login_denied_audit_failed"
		if reason == deniedMissingRole {
			event = "login_denied_suspension_lost"
		}
		s.log.Error(event, zap.String("oidc_sub", sub), zap.String("reason", reason), zap.Error(err))
	}
}

// rolesFrom reads the roles claim: the name as a top-level key first (URL-shaped names hold dots), then
// as a dotted path. Anything but an array yields no role, and non-string items are skipped (006-AC3).
func rolesFrom(claims map[string]any, name string) []string {
	v, ok := claims[name]
	if !ok {
		var cur any = claims
		for part := range strings.SplitSeq(name, ".") {
			obj, isObj := cur.(map[string]any)
			if !isObj {
				return nil
			}
			if cur, ok = obj[part]; !ok {
				return nil
			}
		}
		v = cur
	}
	items, _ := v.([]any)
	var roles []string
	for _, it := range items {
		if role, isStr := it.(string); isStr {
			roles = append(roles, role)
		}
	}
	return roles
}

// exchangeReason calls the exchange an outage only when the token endpoint could not answer (transport
// error, timeout, 5xx); anything else (refused code, malformed answer) is a failed sign-in.
func exchangeReason(err error) string {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if re.Response != nil && re.Response.StatusCode >= http.StatusInternalServerError {
			return authUnavailable
		}
		return authFailed
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return authUnavailable
	}
	return authFailed
}

// logoutRedirect is where the browser goes after sign-out: the provider's end-session endpoint
// (RP-initiated logout, so the IdP session ends too), else the signed-out page.
func (s *Service) logoutRedirect(r *http.Request, sess *session) string {
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
	q.Set("post_logout_redirect_uri", s.publicURL(r)+SignedOutPath)
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
	_ = json.NewEncoder(w).Encode(map[string]string{"redirect": s.logoutRedirect(r, sess)})
}
