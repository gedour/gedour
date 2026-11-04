package worker

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the collector metrics (001-AC31), shared by every worker of a process.
type Metrics struct {
	runs     *prometheus.CounterVec
	duration *prometheus.HistogramVec
	events   *prometheus.CounterVec
	due      prometheus.Gauge
}

// NewMetrics registers the collector metrics on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		runs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gedour_collector_runs_total",
			Help: "Watch runs by kind and outcome (ok, error, rate_limited; discarded when the claim was lost or the target changed).",
		}, []string{"kind", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gedour_collector_run_duration_seconds",
			Help:    "Duration of watch runs, upstream requests and storage included.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"kind"}),
		events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gedour_events_total",
			Help: "Events recorded, by watch kind and event type.",
		}, []string{"kind", "type"}),
		due: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gedour_watches_due",
			Help: "Unpaused watches whose next run time has passed and that no run holds, as seen at the last poll.",
		}),
	}
	reg.MustRegister(m.runs, m.duration, m.events, m.due)
	return m
}
