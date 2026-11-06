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

// Service serves the watch and credential operations.
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

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ReadableSQL is the access rule of watchSelect as a predicate on the watch aliased w, for the
// user given as a SQL expression: the owner only. The router and rule checks use it (001-AC14).
func ReadableSQL(w, user string) string {
	return "(" + w + ".owner_id = " + user + ")"
}

// watchSelect returns the viewer's watches.
const watchSelect = `SELECT w.id::text, w.name, w.kind, w.config, w.credential_id::text, w.schedule, w.paused,
		w.created_at, w.updated_at,
		w.next_run_at, COALESCE(w.leased_until > now(), false), w.last_run_at, w.last_status, w.last_error, w.consecutive_failures
	FROM watches w
	WHERE w.owner_id = @viewer`

func scanWatches(rows pgx.Rows) ([]Watch, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Watch, error) {
		var w Watch
		err := r.Scan(&w.ID, &w.Name, &w.Kind, &w.Config, &w.CredentialID, &w.Schedule, &w.Paused,
			&w.CreatedAt, &w.UpdatedAt,
			&w.NextRunAt, &w.Running, &w.LastRunAt, &w.LastStatus, &w.LastError, &w.ConsecutiveFailures)
		return w, err
	})
}

// authorize is the one access check of every watch route: 404 when the viewer does not own the watch,
// so its existence never leaks. lock holds the row for the tx. 009-AC6.
func authorize(ctx context.Context, q querier, viewer, id string, lock bool) (Watch, error) {
	sql := watchSelect + " AND w.id = @id"
	if lock {
		sql += " FOR UPDATE OF w"
	}
	rows, err := q.Query(ctx, sql, pgx.NamedArgs{"viewer": viewer, "id": id})
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
