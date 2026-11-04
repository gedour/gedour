// Package auth signs users in (OIDC or dev single-user mode), keeps their sessions in PostgreSQL
// and guards /api with session and CSRF checks.
package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"

	"github.com/gedour/gedour/pkg/config"
)

const (
	// SessionCookie holds the session token in dev mode; the database only stores its SHA-256.
	SessionCookie = "gedour_session"
	// SecureSessionCookie is its name when Secure: the __Host- prefix pins it to this origin, Path=/ and HTTPS.
	SecureSessionCookie = "__Host-gedour_session"
	// CSRFHeader must echo the session's CSRF token on every state-changing request.
	CSRFHeader = "X-CSRF-Token"
	// CSRFProblemTitle is the stable title of the 403 problem for a missing or stale CSRF token: the
	// client refreshes its token and retries only on this one.
	CSRFProblemTitle = "CSRF token invalid"
)

// Public /api paths: the API contract and build info, readable before login.
var publicAPI = map[string]bool{
	"/api/openapi.json":     true,
	"/api/openapi.yaml":     true,
	"/api/openapi-3.0.json": true,
	"/api/openapi-3.0.yaml": true,
	"/api/v1/version":       true,
}

// User is a signed-in user.
type User struct {
	ID          string
	Email       string
	DisplayName string
}

type session struct {
	User
	id      []byte
	csrf    string
	idToken string
}

// Service holds the auth configuration and the session store.
type Service struct {
	pool    *pgxpool.Pool
	log     *zap.Logger
	devUser string
	ttl     time.Duration
	idle    time.Duration
	secure  bool

	issuer   string
	oauth    oauth2.Config
	stateKey []byte
	client   *http.Client

	publicURL string

	discoverOnce singleflight.Group
	mu           sync.Mutex // guards the three disc* fields only, never held across I/O
	disc         *discovery
	discErr      error
	discFailedAt time.Time

	devMu      sync.Mutex
	devAccount *User // the dev user's row, upserted once
}

// SignedOutPath is the SPA page sign-out lands on; it never redirects to login by itself.
const SignedOutPath = "/auth/signed-out"

// New builds the service; OIDC discovery runs on the first login so an IdP outage never blocks startup.
func New(cfg config.Config, pool *pgxpool.Pool, log *zap.Logger) (*Service, error) {
	s := &Service{
		pool:    pool,
		log:     log,
		devUser: cfg.DevUser,
		ttl:     cfg.SessionTTL,
		idle:    cfg.SessionIdleTimeout,
		// 001-AC3: Secure everywhere but dev mode, which runs on plain http://localhost.
		secure: cfg.OIDC(),
		client: &http.Client{Timeout: 10 * time.Second},
	}
	if cfg.OIDC() {
		key, err := deriveStateKey(cfg.OIDCClientSecret)
		if err != nil {
			return nil, err
		}
		s.issuer = cfg.OIDCIssuer
		s.stateKey = key
		s.publicURL = strings.TrimRight(cfg.PublicURL, "/")
		s.oauth = oauth2.Config{
			ClientID:     cfg.OIDCClientID,
			ClientSecret: cfg.OIDCClientSecret,
			RedirectURL:  s.publicURL + "/auth/callback",
			Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		}
	}
	return s, nil
}

func (s *Service) sessionCookieName() string {
	if s.secure {
		return SecureSessionCookie
	}
	return SessionCookie
}

// DevMode reports whether every request runs as the configured dev user.
func (s *Service) DevMode() bool { return s.devUser != "" }

type ctxKey struct{}

// UserFrom returns the user Guard authenticated for this request.
func UserFrom(ctx context.Context) (User, bool) {
	sess, ok := ctx.Value(ctxKey{}).(*session)
	if !ok {
		return User{}, false
	}
	return sess.User, true
}

// UserID is UserFrom reduced to the ID, the shape audit.Register wants.
func UserID(ctx context.Context) (string, bool) {
	u, ok := UserFrom(ctx)
	return u.ID, ok
}

// loopbackHost reports whether a Host header names this machine (port ignored).
func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func csrfOK(r *http.Request, sess *session) bool {
	got := r.Header.Get(CSRFHeader)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(sess.csrf)) == 1
}

// Guard authenticates every /api request but the public ones and enforces CSRF on mutations.
// 001-AC1: no session → 401. 001-AC2: dev mode opens a session on the fly. 001-AC3: CSRF → 403.
func (s *Service) Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Dev mode trusts whoever asks, so only a loopback Host may ask: a rebound DNS name pointing at
		// 127.0.0.1 would otherwise let any web page drive the API in the developer's browser.
		if s.DevMode() && (strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/auth/")) && !loopbackHost(r.Host) {
			writeProblem(w, http.StatusForbidden, "dev mode only answers on localhost")
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/") || publicAPI[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		sess, err := s.current(r)
		if err == nil && sess == nil && s.DevMode() {
			// Only the SPA's bootstrap call gets a stored session (it hands out the CSRF token);
			// other cookieless calls run as the dev user without writing anything.
			if r.Method == http.MethodGet && r.URL.Path == "/api/v1/me" {
				sess, err = s.login(r.Context(), w, r, s.devIdentity())
			} else {
				sess, err = s.devSession(r.Context())
			}
		}
		switch {
		case err != nil:
			s.log.Error("session_lookup_failed", zap.Error(err))
			writeProblem(w, http.StatusInternalServerError, "session lookup failed")
			return
		case sess == nil:
			writeProblem(w, http.StatusUnauthorized, "sign in at /auth/login")
			return
		case !safeMethod(r.Method) && !csrfOK(r, sess):
			writeCSRFProblem(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, sess)))
	})
}

// writeProblem answers in the same problem+json shape huma uses for every other API error.
func writeProblem(w http.ResponseWriter, status int, detail string) {
	writeTitledProblem(w, status, http.StatusText(status), detail)
}

func writeTitledProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(huma.ErrorModel{Title: title, Status: status, Detail: detail})
}

func writeCSRFProblem(w http.ResponseWriter) {
	writeTitledProblem(w, http.StatusForbidden, CSRFProblemTitle, "missing or invalid "+CSRFHeader+" header")
}

type meOutput struct {
	Body struct {
		ID          string `json:"id" format:"uuid"`
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		CSRFToken   string `json:"csrf_token" doc:"Send back in the X-CSRF-Token header on every non-GET request"`
		DevMode     bool   `json:"dev_mode" doc:"Single-user mode: there is no real sign-out"`
	}
}

// Register mounts /auth/login, /auth/callback, /auth/logout on mux and GET /api/v1/me on api.
func (s *Service) Register(api huma.API, mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", s.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.handleCallback)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	huma.Register(api, huma.Operation{
		OperationID: "get-me",
		Method:      http.MethodGet,
		Path:        "/api/v1/me",
		Summary:     "The signed-in user and the session's CSRF token",
		Tags:        []string{"auth"},
	}, func(ctx context.Context, _ *struct{}) (*meOutput, error) {
		sess, ok := ctx.Value(ctxKey{}).(*session)
		if !ok {
			return nil, huma.Error401Unauthorized("not signed in")
		}
		out := &meOutput{}
		out.Body.ID, out.Body.Email, out.Body.DisplayName = sess.ID, sess.Email, sess.DisplayName
		out.Body.CSRFToken, out.Body.DevMode = sess.csrf, s.DevMode()
		return out, nil
	})
}
