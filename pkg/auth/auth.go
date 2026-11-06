// Package auth signs users in (OIDC or dev single-user mode), keeps their sessions in PostgreSQL
// and guards /api with session and CSRF checks.
package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"

	"github.com/gedour/gedour/pkg/audit"
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
	isAdmin bool
	// admin is the signed-in user while the session acts as User (006-AC15); nil otherwise.
	admin *User
}

// self is the user who signed in, whoever the session acts as.
func (s *session) self() User {
	if s.admin != nil {
		return *s.admin
	}
	return s.User
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
	// exchangeTimeout bounds the callback's calls to the provider (token, JWKS); a field for tests.
	exchangeTimeout time.Duration

	publicURLs []publicURL // the first one serves requests on an unlisted host

	// keySigninMaxAge is how long a sign-in carries the user's API keys; nil in dev mode, which has no sign-in to renew.
	keySigninMaxAge *time.Duration

	rolesClaim   string
	requiredRole string
	adminRole    string

	discoverOnce singleflight.Group
	mu           sync.Mutex // guards the three disc* fields only, never held across I/O
	disc         *discovery
	discErr      error
	discFailedAt time.Time

	devMu      sync.Mutex
	devAccount *User // the dev user's row, upserted once
}

// AdminPrefix starts every /api path only an admin session may call.
const AdminPrefix = "/api/v1/admin/"

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
		secure:          cfg.OIDC(),
		client:          &http.Client{Timeout: 10 * time.Second},
		exchangeTimeout: 10 * time.Second,
	}
	if cfg.OIDC() {
		key, err := deriveStateKey(cfg.OIDCClientSecret)
		if err != nil {
			return nil, err
		}
		s.issuer = cfg.OIDCIssuer
		s.keySigninMaxAge = &cfg.APIKeySigninMaxAge
		s.stateKey = key
		for _, raw := range cfg.PublicURLs() {
			u, err := url.Parse(raw)
			if err != nil || u.Host == "" {
				return nil, fmt.Errorf("GEDOUR_PUBLIC_URL %q: must be an absolute URL", raw)
			}
			s.publicURLs = append(s.publicURLs, publicURL{host: u.Host, base: strings.TrimRight(raw, "/")})
		}
		if len(s.publicURLs) == 0 {
			return nil, errors.New("GEDOUR_PUBLIC_URL is required with OIDC")
		}
		s.rolesClaim, s.requiredRole, s.adminRole = cfg.OIDCRolesClaim, cfg.OIDCRequiredRole, cfg.OIDCAdminRole
		s.oauth = oauth2.Config{
			ClientID:     cfg.OIDCClientID,
			ClientSecret: cfg.OIDCClientSecret,
			Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		}
	}
	return s, nil
}

type publicURL struct{ host, base string }

// publicURL picks the configured base URL whose host:port is r.Host, else the first one.
// Security: r.Host only selects among configured URLs, never builds one (host-header injection).
func (s *Service) publicURL(r *http.Request) string {
	for _, p := range s.publicURLs {
		if strings.EqualFold(p.host, r.Host) {
			return p.base
		}
	}
	return s.publicURLs[0].base
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

// SessionAlive reports whether r's session still holds, for long-lived requests (SSE) that outlive
// Guard's check: a sign-out or an expiry ends them. Read-only: an open tab is not activity, so the idle
// timeout still applies. Dev mode has no session to lose. Fails closed.
func (s *Service) SessionAlive(r *http.Request) bool {
	if s.DevMode() {
		return true
	}
	sess, err := s.lookup(r, false)
	return err == nil && sess != nil
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
		// 006-AC5: one check on the prefix covers every admin operation, whichever package registers it.
		case strings.HasPrefix(r.URL.Path, AdminPrefix) && !sess.isAdmin:
			writeProblem(w, http.StatusForbidden, "administrators only")
			return
		// Admin pages act on other accounts: never from inside one, but the way out stays open.
		case sess.admin != nil && strings.HasPrefix(r.URL.Path, AdminPrefix) && (r.Method != http.MethodDelete || r.URL.Path != actAsPath):
			writeProblem(w, http.StatusConflict, "stop acting as "+label(sess.User)+" first")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, sess)
		if sess.admin != nil {
			ctx = audit.WithActing(ctx, sess.admin.ID, sess.ID)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
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
		IsAdmin     bool   `json:"is_admin" doc:"The user held the admin role (GEDOUR_OIDC_ADMIN_ROLE) at sign-in; always true in dev mode"`
		ActingAs    *Admin `json:"acting_as" doc:"The admin who signed in, while this session acts as the user above; null otherwise"`
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
		sess, err := sessionOf(ctx)
		if err != nil {
			return nil, err
		}
		out := &meOutput{}
		out.Body.ID, out.Body.Email, out.Body.DisplayName = sess.ID, sess.Email, sess.DisplayName
		out.Body.CSRFToken, out.Body.DevMode, out.Body.IsAdmin = sess.csrf, s.DevMode(), sess.isAdmin
		if a := sess.admin; a != nil {
			out.Body.ActingAs = &Admin{ID: a.ID, Email: a.Email, DisplayName: a.DisplayName}
		}
		return out, nil
	})
	s.registerAdmin(api)
	s.registerAPIKeys(api)
}
