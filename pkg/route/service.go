// Package route turns events into inbox items and channel deliveries: the rule and channel API, the
// router and delivery loops of the worker role, and the live inbox stream of the web role.
package route

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/netguard"
	"github.com/gedour/gedour/pkg/notify"
	"github.com/gedour/gedour/pkg/ratelimit"
	"github.com/gedour/gedour/pkg/script"
	"github.com/gedour/gedour/pkg/secret"
)

// Previews per user: the live editor sends one per pause in typing; a script cannot loop on them.
const (
	previewRate  = 2.0
	previewBurst = 5.0
)

// Service serves channels, rules, deliveries and the inbox.
type Service struct {
	pool      *pgxpool.Pool
	log       *zap.Logger
	box       *secret.Box
	sender    *notify.Sender
	runner    *script.Runner
	guard     *netguard.Guard
	httpHosts []string
	metrics   *Metrics
	hub       *Hub
	viewer    func(context.Context) (string, bool)
	alive     func(*http.Request) bool
	previews  *ratelimit.Limiter
	dryRuns   *ratelimit.Limiter
	// Heartbeat is the SSE keep-alive and session re-check period; tests shorten it.
	Heartbeat time.Duration
}

// New builds the service; sender and runner also serve channel tests (runner the script dry runs),
// guard the save-time webhook and Nextcloud Talk URL checks.
func New(pool *pgxpool.Pool, log *zap.Logger, box *secret.Box, sender *notify.Sender, runner *script.Runner, guard *netguard.Guard, httpHosts []string, m *Metrics) *Service {
	return &Service{pool: pool, log: log, box: box, sender: sender, runner: runner, guard: guard, httpHosts: httpHosts, metrics: m,
		hub: newHub(pool, log, m), Heartbeat: 25 * time.Second, previews: ratelimit.New(previewRate, previewBurst),
		dryRuns: ratelimit.New(dryRunRate, dryRunBurst)}
}

// Register mounts the API on api and the inbox stream on mux. viewer returns the authenticated user's ID;
// alive re-checks a long-lived request's session.
func (s *Service) Register(api huma.API, mux *http.ServeMux, viewer func(context.Context) (string, bool), alive func(*http.Request) bool) {
	s.viewer, s.alive = viewer, alive
	s.registerChannels(api)
	s.registerScripts(api)
	s.registerRules(api)
	s.registerDeliveries(api)
	s.registerInbox(api)
	mux.HandleFunc("GET /api/v1/inbox/stream", s.stream)
}

// RunHub holds the LISTEN connection that feeds the inbox streams of this replica, until ctx ends.
func (s *Service) RunHub(ctx context.Context) { s.hub.Run(ctx) }

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
	s.log.Error("route_api_failed", zap.String("operation", op), zap.Error(err))
	return huma.Error500InternalServerError("internal error")
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// Metrics are the routing and delivery metrics (001-AC31), shared by every loop of a process.
type Metrics struct {
	routed      prometheus.Counter
	evalErrors  prometheus.Counter
	deliveries  *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	subscribers prometheus.Gauge
}

// NewMetrics registers the routing metrics on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		routed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gedour_events_routed_total",
			Help: "Events the router evaluated against the rules, matching or not.",
		}),
		evalErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gedour_rule_eval_errors_total",
			Help: "Rule filter evaluations that failed; the rule counts as not matching for that event.",
		}),
		deliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gedour_deliveries_total",
			Help: "Delivery attempts by channel type and outcome (sent, failed: retry scheduled, dead: no retry left).",
		}, []string{"channel_type", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gedour_delivery_duration_seconds",
			Help:    "Duration of delivery attempts, by channel type.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"channel_type"}),
		subscribers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gedour_sse_subscribers",
			Help: "Open inbox streams on this replica.",
		}),
	}
	reg.MustRegister(m.routed, m.evalErrors, m.deliveries, m.duration, m.subscribers)
	return m
}
