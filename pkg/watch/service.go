package watch

import (
	"context"
	"errors"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/secret"
)

// Service serves the watch, share and credential operations.
type Service struct {
	pool   *pgxpool.Pool
	log    *zap.Logger
	box    *secret.Box
	v      validator
	viewer func(context.Context) (string, bool)
}

// New builds the service; httpHosts may be reached over plain http by watch URLs.
func New(pool *pgxpool.Pool, log *zap.Logger, box *secret.Box, httpHosts []string) *Service {
	return &Service{pool: pool, log: log, box: box, v: validator{httpHosts: httpHosts}}
}

// Register mounts every operation on api; viewer returns the authenticated user's ID.
func (s *Service) Register(api huma.API, viewer func(context.Context) (string, bool)) {
	s.viewer = viewer
	s.registerWatches(api)
	s.registerShares(api)
	s.registerCredentials(api)
}

func (s *Service) me(ctx context.Context) (string, error) {
	id, ok := s.viewer(ctx)
	if !ok {
		return "", huma.Error401Unauthorized("not signed in")
	}
	return id, nil
}

// fail passes API errors through and turns anything else into a logged, opaque 500.
func (s *Service) fail(op string, err error) error {
	var se huma.StatusError
	if errors.As(err, &se) {
		return se
	}
	s.log.Error("watch_api_failed", zap.String("operation", op), zap.Error(err))
	return huma.Error500InternalServerError("internal error")
}

// Access levels on a watch, in increasing order.
type level int

const (
	levelNone level = iota
	levelRead
	levelWrite
	levelOwner
)

func levelOf(permission string) level {
	switch permission {
	case "owner":
		return levelOwner
	case "rw":
		return levelWrite
	case "ro":
		return levelRead
	}
	return levelNone
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// watchSelect returns watches the viewer can read, with the viewer's permission on each.
const watchSelect = `SELECT w.id::text, w.name, w.kind, w.config, w.credential_id::text, w.schedule, w.paused,
		w.owner_id::text, COALESCE(NULLIF(u.display_name, ''), u.email, ''),
		CASE WHEN w.owner_id = @viewer THEN 'owner' ELSE s.permission END, w.created_at, w.updated_at
	FROM watches w JOIN users u ON u.id = w.owner_id
	LEFT JOIN watch_shares s ON s.watch_id = w.id AND s.user_id = @viewer
	WHERE (w.owner_id = @viewer OR s.user_id IS NOT NULL)`

func scanWatches(rows pgx.Rows) ([]Watch, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Watch, error) {
		var w Watch
		err := r.Scan(&w.ID, &w.Name, &w.Kind, &w.Config, &w.CredentialID, &w.Schedule, &w.Paused,
			&w.OwnerID, &w.OwnerName, &w.Permission, &w.CreatedAt, &w.UpdatedAt)
		return w, err
	})
}

// authorize is the one access check of every watch route: 404 when the viewer cannot read the watch
// (its existence never leaks), 403 when they can but below need. lock holds the row for the tx.
// 001-AC5.
func authorize(ctx context.Context, q querier, viewer, id string, need level, lock bool) (Watch, error) {
	if lock {
		// Its own statement: FOR UPDATE on the join would recheck only the watch row after waiting and
		// reuse the stale share row, letting a share revoked meanwhile write once more. The select below
		// then reads the shares in a fresh snapshot.
		rows, err := q.Query(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR UPDATE`, id)
		if err != nil {
			return Watch{}, err
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return Watch{}, err
		}
	}
	rows, err := q.Query(ctx, watchSelect+" AND w.id = @id", pgx.NamedArgs{"viewer": viewer, "id": id})
	if err != nil {
		return Watch{}, err
	}
	ws, err := scanWatches(rows)
	if err != nil {
		return Watch{}, err
	}
	if len(ws) == 0 {
		return Watch{}, huma.Error404NotFound("watch not found")
	}
	if levelOf(ws[0].Permission) < need {
		msg := "a read-only share cannot change this watch"
		if need == levelOwner {
			msg = "only the watch owner can do this"
		}
		return Watch{}, huma.Error403Forbidden(msg)
	}
	return ws[0], nil
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// likePattern matches s anywhere, with LIKE's wildcards in s taken literally.
func likePattern(s string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(s) + "%"
}
