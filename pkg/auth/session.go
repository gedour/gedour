package auth

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/db"
)

// identity is what the provider (or dev mode) asserts about the user signing in.
type identity struct {
	Sub         string
	Email       string
	DisplayName string
	IDToken     string // raw, empty in dev mode
	Admin       bool
}

func (s *Service) devIdentity() identity {
	id := identity{Sub: "dev:" + s.devUser, DisplayName: s.devUser}
	if strings.Contains(s.devUser, "@") {
		id.Email = s.devUser
	}
	return id
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24+)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// auditID names a session in the audit log without exposing the full key.
func auditID(id []byte) string { return hex.EncodeToString(id[:8]) }

// errAccountDisabled refuses the sign-in of a disabled or purged account.
var errAccountDisabled = errors.New("account disabled")

// upsertUser creates or refreshes the user keyed by the provider's sub (001-AC1). The upsert holds the
// row lock until commit and reads the latest status, so a disable racing this login either commits first
// (refused here) or waits and then revokes the new session (006-AC7).
func upsertUser(ctx context.Context, tx pgx.Tx, who identity) (User, bool, error) {
	var u User
	var created, disabled bool
	// xmax = 0 only on a freshly inserted row: tells insert from update in one statement.
	err := tx.QueryRow(ctx, `INSERT INTO users (oidc_sub, email, display_name, last_login_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (oidc_sub) DO UPDATE SET email = EXCLUDED.email, display_name = EXCLUDED.display_name, last_login_at = now()
		RETURNING id::text, email, display_name, xmax = 0, disabled_at IS NOT NULL OR purged_at IS NOT NULL`, who.Sub, who.Email, who.DisplayName).
		Scan(&u.ID, &u.Email, &u.DisplayName, &created, &disabled)
	if err != nil {
		return User{}, false, fmt.Errorf("upsert user: %w", err)
	}
	if disabled {
		return User{}, false, errAccountDisabled
	}
	return u, created, nil
}

// upsertAudited upserts the user and audits its creation, within tx.
func upsertAudited(ctx context.Context, tx pgx.Tx, who identity) (User, error) {
	u, created, err := upsertUser(ctx, tx, who)
	if err != nil || !created {
		return u, err
	}
	after := map[string]string{"id": u.ID, "oidc_sub": who.Sub, "email": u.Email, "display_name": u.DisplayName}
	return u, audit.Record(ctx, tx, audit.Entry{ActorUserID: u.ID, Action: "user.created", TargetType: "user", TargetID: u.ID, After: after})
}

// devSession is the dev user without a stored session: no CSRF token, so it can read but never mutate.
func (s *Service) devSession(ctx context.Context) (*session, error) {
	s.devMu.Lock()
	u := s.devAccount
	s.devMu.Unlock()
	if u == nil {
		// Unlocked: a concurrent first call upserts the same row, which ON CONFLICT makes harmless.
		var fresh User
		err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			var err error
			fresh, err = upsertAudited(ctx, tx, s.devIdentity())
			return err
		})
		if err != nil {
			return nil, err
		}
		s.devMu.Lock()
		s.devAccount = &fresh
		s.devMu.Unlock()
		u = &fresh
	}
	return &session{User: *u, isAdmin: true}, nil
}

// purgeQuery removes a bounded batch of dead sessions; a variable so a test can make it fail.
var purgeQuery = `DELETE FROM sessions WHERE id IN (
	SELECT id FROM sessions WHERE expires_at <= now() OR last_seen_at <= now() - $1::interval
	LIMIT 500 FOR UPDATE SKIP LOCKED)`

// login upserts the user, replaces the request's previous session (if any) with a new one, audits it
// and sets the session cookie.
func (s *Service) login(ctx context.Context, w http.ResponseWriter, r *http.Request, who identity) (*session, error) {
	token := randomToken()
	// 006-AC19: dev admin lives in memory only, so a dev row never stays admin once the instance moves to OIDC.
	sess := &session{id: hashToken(token), csrf: randomToken(), isAdmin: who.Admin || s.DevMode()}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		u, err := upsertAudited(ctx, tx, who)
		if err != nil {
			return err
		}
		sess.User = u
		// The upsert holds the user row: a racing missing_role refusal suspends before or after, never in between.
		resumed, err := resumeKeys(ctx, tx, u.ID)
		if err != nil {
			return err
		}
		after := map[string]any{"session": auditID(sess.id)}
		if resumed > 0 {
			after["resumed_api_key_count"] = resumed
		}
		if err := audit.Record(ctx, tx, audit.Entry{ActorUserID: u.ID, Action: "user.login", TargetType: "user", TargetID: u.ID, After: after}); err != nil {
			return err
		}
		// A login over a live session replaces it rather than leaving it valid alongside.
		if old, err := r.Cookie(s.sessionCookieName()); err == nil && old.Value != "" {
			if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, hashToken(old.Value)); err != nil {
				return fmt.Errorf("drop previous session: %w", err)
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO sessions (id, user_id, csrf_token, id_token, expires_at, is_admin) VALUES ($1, $2, $3, NULLIF($4, ''), now() + $5::interval, $6)`,
			sess.id, u.ID, sess.csrf, who.IDToken, s.ttl, who.Admin)
		if err != nil {
			return fmt.Errorf("insert session: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.log.Info("user_login", zap.String("user_id", sess.ID))
	http.SetCookie(w, s.sessionCookie(token, int(s.ttl.Seconds())))
	// Opportunistic purge, outside the login tx: logins are rare, so dead rows go without a background job.
	if _, err := s.pool.Exec(ctx, purgeQuery, s.idle); err != nil {
		s.log.Warn("session_purge_failed", zap.Error(err))
	}
	return sess, nil
}

// 001-AC3: HttpOnly, Secure (but in dev mode), SameSite=Lax.
func (s *Service) sessionCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{ //nolint:gosec // Secure is off only in dev mode (plain http://localhost)
		Name:     s.sessionCookieName(),
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// current returns the request's live session, or nil when there is none or it expired; it moves the
// idle clock.
func (s *Service) current(r *http.Request) (*session, error) {
	return s.lookup(r, true)
}

// lookup is current, with touch false for checks that must not count as activity.
func (s *Service) lookup(r *http.Request, touch bool) (*session, error) {
	c, err := r.Cookie(s.sessionCookieName())
	if err != nil || c.Value == "" {
		return nil, nil //nolint:nilerr // no cookie is "signed out", not an error
	}
	ctx := r.Context()
	sess := &session{id: hashToken(c.Value)}
	var stale bool
	var target struct{ id, email, name *string }
	// The target may be disabled (act-as helps fix it before enabling); a purge clears acting_as.
	err = s.pool.QueryRow(ctx, `SELECT u.id::text, u.email, u.display_name, s.csrf_token, COALESCE(s.id_token, ''),
			s.last_seen_at < now() - interval '1 minute', s.is_admin, t.id::text, t.email, t.display_name
		FROM sessions s JOIN users u ON u.id = s.user_id AND u.disabled_at IS NULL AND u.purged_at IS NULL
		LEFT JOIN users t ON t.id = s.acting_as
		WHERE s.id = $1 AND s.expires_at > now() AND s.last_seen_at > now() - $2::interval`, sess.id, s.idle).
		Scan(&sess.ID, &sess.Email, &sess.DisplayName, &sess.csrf, &sess.idToken, &stale, &sess.isAdmin,
			&target.id, &target.email, &target.name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup session: %w", err)
	}
	// 006-AC19: a dev session row from before 00009 holds false, and dev mode never signs in again.
	sess.isAdmin = sess.isAdmin || s.DevMode()
	if target.id != nil {
		// 006-AC15: every owner-scoped handler reads User, so they all run as the target unchanged.
		admin := sess.User
		sess.admin = &admin
		sess.User = User{ID: *target.id, Email: *target.email, DisplayName: *target.name}
	}
	// Idle clock moves at most once a minute: no write on every request.
	if stale && touch {
		if _, err := s.pool.Exec(ctx, `UPDATE sessions SET last_seen_at = now() WHERE id = $1`, sess.id); err != nil {
			return nil, fmt.Errorf("touch session: %w", err)
		}
	}
	return sess, nil
}

// Reasons a user.login_denied entry records.
const (
	deniedMissingRole = "missing_role"
	deniedDisabled    = "disabled"
)

// auditDenied records a refused sign-in, without creating or changing the user row. A missing role also suspends the
// user's API keys until the next sign-in and deletes their sessions (007-AC12): a key never signs in to find out, and a
// live session could mint a new one.
func (s *Service) auditDenied(ctx context.Context, sub, reason string) error {
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		query := `SELECT id::text FROM users WHERE oidc_sub = $1`
		if reason == deniedMissingRole {
			// Waits for a key creation in flight, whose key the suspension below must see.
			query += ` FOR NO KEY UPDATE`
		}
		var id string
		err := tx.QueryRow(ctx, query, sub).Scan(&id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("look up denied user: %w", err)
		}
		after := map[string]any{"reason": reason}
		if reason == deniedMissingRole && id != "" {
			n, err := suspendActiveKeys(ctx, tx, id)
			if err != nil {
				return err
			}
			if n > 0 {
				after["suspended_api_key_count"] = n
			}
			tag, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, id)
			if err != nil {
				return fmt.Errorf("delete sessions: %w", err)
			}
			if n := tag.RowsAffected(); n > 0 {
				after["deleted_session_count"] = n
			}
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: id, Action: "user.login_denied", TargetType: "user",
			TargetID: cmp.Or(id, sub), After: after})
	})
}

// logout deletes the session and audits it in one transaction.
func (s *Service) logout(ctx context.Context, sess *session) error {
	// /auth/logout runs outside Guard, which tags acting requests.
	if sess.admin != nil {
		ctx = audit.WithActing(ctx, sess.admin.ID, sess.ID)
	}
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, sess.id); err != nil {
			return fmt.Errorf("delete session: %w", err)
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: sess.self().ID, Action: "session.logout", TargetType: "session", TargetID: auditID(sess.id)})
	})
}
