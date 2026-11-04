package watch

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/secret"
)

// Credential is a stored secret's metadata. It has no field for the secret itself, so neither the API
// nor the audit log (which records this type) can carry it. 001-AC6.
type Credential struct {
	ID          string    `json:"id" format:"uuid"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind" enum:"github_token,gitlab_token,registry_basic"`
	Username    *string   `json:"username" nullable:"true" doc:"registry_basic only"`
	Last4       string    `json:"last4" doc:"Last 4 characters of the secret; empty for secrets under 16 characters"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	UsedByCount int       `json:"used_by_count" doc:"Watches using this credential"`
}

type credentialOutput struct {
	Body Credential
}

type listCredentialsInput struct {
	Kind string `query:"kind" enum:"github_token,gitlab_token,registry_basic" doc:"Only credentials of this kind"`
}

type listCredentialsOutput struct {
	Body struct {
		Items []Credential `json:"items" doc:"Sorted by name"`
	}
}

type secretBody struct {
	Secret string `json:"secret" minLength:"8" maxLength:"8192" doc:"Write-only, never returned"`
}

type createCredentialInput struct {
	Body struct {
		Name     string `json:"name" minLength:"1" maxLength:"200"`
		Kind     string `json:"kind" enum:"github_token,gitlab_token,registry_basic"`
		Username string `json:"username,omitempty" maxLength:"256" doc:"Required for registry_basic, refused otherwise"`
		Secret   string `json:"secret" minLength:"8" maxLength:"8192" doc:"Write-only, never returned"`
	}
}

type rotateSecretInput struct {
	ID   string `path:"id" format:"uuid"`
	Body secretBody
}

//nolint:gosec // G101 false positive: a query, not a credential
const credentialSelect = `SELECT c.id::text, c.name, c.kind, c.username, c.last4, c.created_at, c.updated_at,
		(SELECT count(*) FROM watches w WHERE w.credential_id = c.id)
	FROM credentials c WHERE c.owner_id = @owner`

// last4 shows a hint of long secrets only: 4 characters of a short password give too much away.
func last4(s string) string {
	if utf8.RuneCountInString(s) < 16 {
		return ""
	}
	r := []rune(s)
	return string(r[len(r)-4:])
}

// ownCredential loads one of owner's credentials; anyone else's is a 404, like a missing one.
// Row lock strengths for ownCredential. A rotation leaves the key alone, so it must not block an attach
// (FOR KEY SHARE); a delete removes the key, so it waits for every attach in flight.
const (
	lockNone      = ""
	lockNoKeyUpd  = " FOR NO KEY UPDATE OF c"
	lockForDelete = " FOR UPDATE OF c"
)

func ownCredential(ctx context.Context, q querier, owner, id, lock string) (Credential, error) {
	sql := credentialSelect + " AND c.id = @id" + lock
	rows, err := q.Query(ctx, sql, pgx.NamedArgs{"owner": owner, "id": id})
	if err != nil {
		return Credential{}, err
	}
	cs, err := scanCredentials(rows)
	if err != nil {
		return Credential{}, err
	}
	if len(cs) == 0 {
		return Credential{}, huma.Error404NotFound("credential not found")
	}
	return cs[0], nil
}

func scanCredentials(rows pgx.Rows) ([]Credential, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Credential, error) {
		var c Credential
		err := r.Scan(&c.ID, &c.Name, &c.Kind, &c.Username, &c.Last4, &c.CreatedAt, &c.UpdatedAt, &c.UsedByCount)
		return c, err
	})
}

func (s *Service) registerCredentials(api huma.API) {
	tags := []string{"credentials"}
	huma.Register(api, huma.Operation{
		OperationID: "list-credentials", Method: http.MethodGet, Path: "/api/v1/credentials",
		Summary: "The caller's credentials; secrets are never returned", Tags: tags,
	}, s.listCredentials)
	huma.Register(api, huma.Operation{
		OperationID: "get-credential", Method: http.MethodGet, Path: "/api/v1/credentials/{id}",
		Summary: "One of the caller's credentials", Tags: tags,
	}, func(ctx context.Context, in *idPath) (*credentialOutput, error) {
		me, err := s.me(ctx)
		if err != nil {
			return nil, err
		}
		c, err := ownCredential(ctx, s.pool, me, in.ID, lockNone)
		if err != nil {
			return nil, s.fail("get-credential", err)
		}
		return &credentialOutput{Body: c}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "create-credential", Method: http.MethodPost, Path: "/api/v1/credentials",
		Summary: "Store a credential, encrypted at rest", Tags: tags, DefaultStatus: http.StatusCreated,
	}, s.createCredential)
	huma.Register(api, huma.Operation{
		OperationID: "rotate-credential-secret", Method: http.MethodPut, Path: "/api/v1/credentials/{id}/secret",
		Summary: "Replace a credential's secret", Tags: tags,
	}, s.rotateSecret)
	huma.Register(api, huma.Operation{
		OperationID: "delete-credential", Method: http.MethodDelete, Path: "/api/v1/credentials/{id}",
		Summary: "Delete a credential no watch uses", Tags: tags, DefaultStatus: http.StatusNoContent,
	}, s.deleteCredential)
}

func (s *Service) listCredentials(ctx context.Context, in *listCredentialsInput) (*listCredentialsOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	sql := credentialSelect
	args := pgx.NamedArgs{"owner": me}
	if in.Kind != "" {
		sql += " AND c.kind = @kind"
		args["kind"] = in.Kind
	}
	rows, err := s.pool.Query(ctx, sql+" ORDER BY lower(c.name), c.id", args)
	if err != nil {
		return nil, s.fail("list-credentials", err)
	}
	out := &listCredentialsOutput{}
	if out.Body.Items, err = scanCredentials(rows); err != nil {
		return nil, s.fail("list-credentials", err)
	}
	if out.Body.Items == nil {
		out.Body.Items = []Credential{}
	}
	return out, nil
}

func (s *Service) createCredential(ctx context.Context, in *createCredentialInput) (*credentialOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	name := strings.TrimSpace(b.Name)
	var errs []error
	if name == "" {
		errs = append(errs, &huma.ErrorDetail{Location: "body.name", Message: "must not be blank"})
	}
	var username *string
	switch u := strings.TrimSpace(b.Username); {
	case b.Kind == CredRegistryBasic && u == "":
		errs = append(errs, &huma.ErrorDetail{Location: "body.username", Message: "required for registry_basic"})
	case b.Kind != CredRegistryBasic && u != "":
		errs = append(errs, &huma.ErrorDetail{Location: "body.username", Message: "only registry_basic credentials have a username"})
	case u != "":
		username = &u
	}
	if len(errs) > 0 {
		return nil, huma.Error400BadRequest("invalid credential", errs...)
	}
	id := uuid.NewString()
	ct, nonce := s.box.Seal([]byte(b.Secret), []byte(id))
	var c Credential
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO credentials (id, owner_id, name, kind, username, secret_ciphertext, secret_nonce, key_version, last4)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, id, me, name, b.Kind, username, ct, nonce, secret.KeyVersion, last4(b.Secret))
		if isUniqueViolation(err) {
			return huma.Error409Conflict("a credential with this name exists",
				&huma.ErrorDetail{Location: "body.name", Message: "you already have a credential with this name", Value: name})
		}
		if err != nil {
			return err
		}
		if c, err = ownCredential(ctx, tx, me, id, lockNone); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "credential.created", TargetType: "credential", TargetID: id, After: c})
	})
	if err != nil {
		return nil, s.fail("create-credential", err)
	}
	return &credentialOutput{Body: c}, nil
}

func (s *Service) rotateSecret(ctx context.Context, in *rotateSecretInput) (*credentialOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	ct, nonce := s.box.Seal([]byte(in.Body.Secret), []byte(in.ID))
	var after Credential
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := ownCredential(ctx, tx, me, in.ID, lockNoKeyUpd)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE credentials SET secret_ciphertext = $2, secret_nonce = $3, key_version = $4, last4 = $5, updated_at = now()
			WHERE id = $1`, in.ID, ct, nonce, secret.KeyVersion, last4(in.Body.Secret)); err != nil {
			return err
		}
		if after, err = ownCredential(ctx, tx, me, in.ID, lockNone); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "credential.secret_rotated", TargetType: "credential", TargetID: in.ID,
			Before: before, After: after})
	})
	if err != nil {
		return nil, s.fail("rotate-credential-secret", err)
	}
	return &credentialOutput{Body: after}, nil
}

func (s *Service) deleteCredential(ctx context.Context, in *idPath) (*struct{}, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := ownCredential(ctx, tx, me, in.ID, lockForDelete)
		if err != nil {
			return err
		}
		// A credential only attaches to its owner's watches, so every user of it is visible to the caller.
		rows, err := tx.Query(ctx, `SELECT name FROM watches WHERE credential_id = $1 AND owner_id = $2 ORDER BY lower(name)`, in.ID, me)
		if err != nil {
			return err
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if len(names) > 0 {
			details := make([]error, len(names))
			for i, n := range names {
				details[i] = &huma.ErrorDetail{Location: "watches", Message: "uses this credential", Value: n}
			}
			return huma.Error409Conflict("used by "+strings.Join(names, ", ")+": detach it from these watches first", details...)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM credentials WHERE id = $1`, in.ID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "credential.deleted", TargetType: "credential", TargetID: in.ID, Before: before})
	})
	if err != nil {
		return nil, s.fail("delete-credential", err)
	}
	return nil, nil
}
