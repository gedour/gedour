package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/db"
)

const (
	// keyPrefix lets secret scanners match a leaked key.
	keyPrefix     = "gdr_"
	keyDisplayLen = 8
	maxKeyName    = 64
	maxActiveKeys = 10
)

// ErrInvalidKey is LookupKey's one answer for a malformed, unknown, revoked or expired key or a disabled or purged owner.
var ErrInvalidKey = errors.New("invalid API key")

// KeyPrincipal is who a valid key speaks for.
type KeyPrincipal struct {
	User  User
	KeyID string
	Scope string
	// Dormant: the owner has no sign-in within the max age; a sign-in wakes the key.
	Dormant bool
	// EffectiveUntil is nil only in dev mode on a never key.
	EffectiveUntil *time.Time
}

// APIKey is a key as its owner sees it; it has no field for the key or its hash.
type APIKey struct {
	ID             string     `json:"id" format:"uuid"`
	Name           string     `json:"name"`
	Scope          string     `json:"scope" enum:"read,write,manage"`
	Prefix         string     `json:"prefix" doc:"The first 8 random characters, after gdr_"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      *time.Time `json:"expires_at" nullable:"true" doc:"Null when created without an end date"`
	LastUsedAt     *time.Time `json:"last_used_at" nullable:"true"`
	RevokedAt      *time.Time `json:"revoked_at" nullable:"true"`
	EffectiveUntil *time.Time `json:"effective_until" nullable:"true" doc:"The earlier of expires_at and the owner's last sign-in plus the max age; null only in dev mode without an end date"`
	Status         string     `json:"status" enum:"active,dormant,expired,revoked"`
}

// APIKeyCreated is the only answer that holds the key.
type APIKeyCreated struct {
	APIKey
	Key string `json:"key" doc:"Shown once, never returned again"`
}

type createAPIKeyInput struct {
	Body struct {
		Name     string `json:"name" doc:"1 to 64 characters after trimming"`
		Scope    string `json:"scope,omitempty" enum:"read,write,manage" default:"read"`
		Duration string `json:"duration,omitempty" enum:"7,30,90,365,never" default:"90" doc:"Days until the key expires, or never"`
	}
}

type createAPIKeyOutput struct {
	// Security: the one answer holding the key must stay out of every cache.
	CacheControl string `header:"Cache-Control"`
	Body         APIKeyCreated
}

type listAPIKeysOutput struct {
	Body struct {
		Items []APIKey `json:"items" doc:"Newest first"`
	}
}

// keyAudit is what the audit log keeps of a key. Names ending in "key" would be redacted (audit.IsSecretKey).
type keyAudit struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Scope     string     `json:"scope"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// keySigninEndSQL is when the owner's last sign-in stops carrying key k, with $1 the max age (null in dev mode).
// Never signed in counts as already past: nothing ever vouched for the owner.
const keySigninEndSQL = `CASE WHEN $1::interval IS NOT NULL THEN COALESCE(u.last_login_at + $1::interval, k.created_at) END`

// keyActiveSQL matches a key not revoked and not expired.
const keyActiveSQL = `revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())`

// $2 is the owner.
const keySelectSQL = `SELECT k.id::text, k.name, k.scope, k.prefix, k.created_at, k.expires_at, k.last_used_at, k.revoked_at,
		LEAST(k.expires_at, ` + keySigninEndSQL + `),
		CASE WHEN k.revoked_at IS NOT NULL THEN 'revoked'
			WHEN k.expires_at <= now() THEN 'expired'
			WHEN ` + keySigninEndSQL + ` <= now() THEN 'dormant'
			ELSE 'active' END
	FROM api_keys k JOIN users u ON u.id = k.user_id
	WHERE k.user_id = $2`

func scanKeys(rows pgx.Rows) ([]APIKey, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (APIKey, error) {
		var k APIKey
		err := r.Scan(&k.ID, &k.Name, &k.Scope, &k.Prefix, &k.CreatedAt, &k.ExpiresAt, &k.LastUsedAt, &k.RevokedAt, &k.EffectiveUntil, &k.Status)
		return k, err
	})
}

func (s *Service) registerAPIKeys(api huma.API) {
	tags := []string{"api-keys"}
	huma.Register(api, huma.Operation{
		OperationID: "list-api-keys", Method: http.MethodGet, Path: "/api/v1/api-keys",
		Summary: "The caller's API keys; a key is never returned", Tags: tags,
	}, s.listKeys)
	huma.Register(api, huma.Operation{
		OperationID: "create-api-key", Method: http.MethodPost, Path: "/api/v1/api-keys",
		Summary: "Create an API key, returned this once; refused while acting as a user", Tags: tags, DefaultStatus: http.StatusCreated,
	}, s.createKey)
	huma.Register(api, huma.Operation{
		OperationID: "revoke-api-key", Method: http.MethodDelete, Path: "/api/v1/api-keys/{id}",
		Summary: "Revoke one of the caller's API keys", Tags: tags, DefaultStatus: http.StatusNoContent,
	}, s.revokeKey)
}

func (s *Service) listKeys(ctx context.Context, _ *struct{}) (*listAPIKeysOutput, error) {
	sess, err := sessionOf(ctx)
	if err != nil {
		return nil, err
	}
	// A Query error stays on rows, and CollectRows reports it.
	rows, _ := s.pool.Query(ctx, keySelectSQL+` ORDER BY k.created_at DESC, k.id`, s.keySigninMaxAge, sess.ID)
	out := &listAPIKeysOutput{}
	if out.Body.Items, err = scanKeys(rows); err != nil {
		return nil, s.apiFail("list-api-keys", fmt.Errorf("list keys: %w", err))
	}
	return out, nil
}

// createKey holds the user row: the cap survives concurrent creates, and a racing disable cannot miss the new key.
func (s *Service) createKey(ctx context.Context, in *createAPIKeyInput) (*createAPIKeyOutput, error) {
	sess, err := sessionOf(ctx)
	if err != nil {
		return nil, err
	}
	// 007-AC5: a key would keep the admin a door into the target's account after the act-as ends.
	if sess.admin != nil {
		return nil, huma.Error409Conflict("stop acting as " + label(sess.User) + " first")
	}
	name := strings.TrimSpace(in.Body.Name)
	if n := utf8.RuneCountInString(name); n < 1 || n > maxKeyName || strings.ContainsRune(name, 0) {
		return nil, huma.Error400BadRequest("invalid API key", &huma.ErrorDetail{Location: "body.name", Message: "must be 1 to 64 characters", Value: in.Body.Name})
	}
	var days *int
	if in.Body.Duration != "never" {
		d, _ := strconv.Atoi(in.Body.Duration) // huma's enum already refused anything else
		days = &d
	}
	key := keyPrefix + randomToken()
	prefix := key[len(keyPrefix) : len(keyPrefix)+keyDisplayLen]
	var created APIKey
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var live bool
		err := tx.QueryRow(ctx, `SELECT disabled_at IS NULL AND purged_at IS NULL FROM users WHERE id = $1 FOR NO KEY UPDATE`, sess.ID).Scan(&live)
		if err != nil {
			return fmt.Errorf("lock user: %w", err)
		}
		if !live {
			return huma.Error409Conflict("the account is disabled")
		}
		var active int
		var signedIn bool
		// A statement after the lock, with its own snapshot: a missing-role refusal may have ended the session while it waited.
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM api_keys WHERE user_id = $1 AND `+keyActiveSQL+`), EXISTS (SELECT 1 FROM sessions WHERE id = $2)`,
			sess.ID, sess.id).Scan(&active, &signedIn); err != nil {
			return fmt.Errorf("count keys: %w", err)
		}
		if !signedIn {
			return huma.Error401Unauthorized("sign in at /auth/login")
		}
		if active >= maxActiveKeys {
			return huma.Error409Conflict(fmt.Sprintf("you already have %d active API keys: revoke one first", maxActiveKeys))
		}
		if _, err := tx.Exec(ctx, `DELETE FROM api_keys WHERE user_id = $1 AND (revoked_at <= now() - interval '30 days' OR expires_at <= now() - interval '30 days')`, sess.ID); err != nil {
			return fmt.Errorf("delete dead keys: %w", err)
		}
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO api_keys (user_id, name, hash, prefix, scope, expires_at)
			VALUES ($1, $2, $3, $4, $5, now() + make_interval(days => $6::int)) RETURNING id::text`,
			sess.ID, name, hashToken(key), prefix, in.Body.Scope, days).Scan(&id); err != nil {
			return fmt.Errorf("insert key: %w", err)
		}
		rows, _ := tx.Query(ctx, keySelectSQL+` AND k.id = $3`, s.keySigninMaxAge, sess.ID, id)
		keys, err := scanKeys(rows)
		if err != nil {
			return fmt.Errorf("read key: %w", err)
		}
		created = keys[0]
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: sess.ID, Action: "api_key.created", TargetType: "api_key", TargetID: id,
			After: keyAudit{ID: id, Name: created.Name, Scope: created.Scope, ExpiresAt: created.ExpiresAt}})
	})
	if err != nil {
		return nil, s.apiFail("create-api-key", err)
	}
	s.log.Info("api_key_created", zap.String("key_id", created.ID), zap.String("user_id", sess.ID))
	return &createAPIKeyOutput{CacheControl: "no-store", Body: APIKeyCreated{APIKey: created, Key: key}}, nil
}

// revokeKey acts on the effective user, so an admin acting as them can close a leak (audited with on_behalf_of).
func (s *Service) revokeKey(ctx context.Context, in *userIDPath) (*struct{}, error) {
	sess, err := sessionOf(ctx)
	if err != nil {
		return nil, err
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var a keyAudit
		var revoked bool
		err := tx.QueryRow(ctx, `SELECT id::text, name, scope, expires_at, revoked_at IS NOT NULL FROM api_keys WHERE id = $1 AND user_id = $2 FOR NO KEY UPDATE`,
			in.ID, sess.ID).Scan(&a.ID, &a.Name, &a.Scope, &a.ExpiresAt, &revoked)
		if errors.Is(err, pgx.ErrNoRows) {
			return huma.Error404NotFound("no such API key")
		}
		if err != nil {
			return fmt.Errorf("lock key: %w", err)
		}
		if revoked {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1`, a.ID); err != nil {
			return fmt.Errorf("revoke key: %w", err)
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: sess.ID, Action: "api_key.revoked", TargetType: "api_key", TargetID: a.ID, After: a})
	})
	if err != nil {
		return nil, s.apiFail("revoke-api-key", err)
	}
	return nil, nil
}

// revokeActiveKeys needs the user row held by tx; an already dead key keeps its date, or its 30-day cleanup clock would restart.
func revokeActiveKeys(ctx context.Context, tx pgx.Tx, userID string) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE user_id = $1 AND `+keyActiveSQL, userID)
	if err != nil {
		return 0, fmt.Errorf("revoke keys: %w", err)
	}
	return tag.RowsAffected(), nil
}

// LookupKey resolves a bearer string to its principal or ErrInvalidKey; any other error is a database failure.
func (s *Service) LookupKey(ctx context.Context, raw string) (KeyPrincipal, error) {
	if !strings.HasPrefix(raw, keyPrefix) {
		return KeyPrincipal{}, ErrInvalidKey
	}
	var p KeyPrincipal
	var stale bool
	err := s.pool.QueryRow(ctx, `SELECT k.id::text, k.scope, u.id::text, u.email, u.display_name,
			COALESCE(`+keySigninEndSQL+` <= now(), false), LEAST(k.expires_at, `+keySigninEndSQL+`),
			k.last_used_at IS NULL OR k.last_used_at < now() - interval '1 minute'
		FROM api_keys k JOIN users u ON u.id = k.user_id AND u.disabled_at IS NULL AND u.purged_at IS NULL
		WHERE k.hash = $2 AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at > now())`, s.keySigninMaxAge, hashToken(raw)).
		Scan(&p.KeyID, &p.Scope, &p.User.ID, &p.User.Email, &p.User.DisplayName, &p.Dormant, &p.EffectiveUntil, &stale)
	if errors.Is(err, pgx.ErrNoRows) {
		return KeyPrincipal{}, ErrInvalidKey
	}
	if err != nil {
		return KeyPrincipal{}, fmt.Errorf("lookup key: %w", err)
	}
	// Moves at most once a minute: no write on every request.
	if stale {
		if _, err := s.pool.Exec(ctx, `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, p.KeyID); err != nil {
			return KeyPrincipal{}, fmt.Errorf("touch key: %w", err)
		}
	}
	return p, nil
}
