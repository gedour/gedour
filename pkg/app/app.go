// Package app wires config, database, HTTP listeners and the worker into one process lifecycle.
package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/mail"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/auth"
	"github.com/gedour/gedour/pkg/collect"
	"github.com/gedour/gedour/pkg/config"
	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/netguard"
	"github.com/gedour/gedour/pkg/notify"
	"github.com/gedour/gedour/pkg/route"
	"github.com/gedour/gedour/pkg/script"
	"github.com/gedour/gedour/pkg/secret"
	"github.com/gedour/gedour/pkg/server"
	"github.com/gedour/gedour/pkg/watch"
	"github.com/gedour/gedour/pkg/worker"
)

// Run migrates the database, serves until ctx is cancelled, then drains within cfg.ShutdownTimeout.
func Run(ctx context.Context, cfg config.Config, log *zap.Logger, build server.BuildInfo, ui fs.FS) error {
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if !waitForDB(ctx, pool, log) {
		// Stopped (SIGTERM) before the database answered: a clean stop, not a failure.
		return nil
	}
	applied, err := db.Migrate(ctx, pool)
	if err != nil {
		return err
	}
	log.Info("migrations_applied", zap.Int("count", applied))

	box, err := secret.New(cfg.EncryptionKey)
	if err != nil {
		return err
	}
	ping := server.PingFunc(pool.Ping)
	reg := server.NewRegistry(build)
	guard, err := netguard.New(cfg.AllowPrivateNets)
	if err != nil {
		return err
	}
	sender, err := newSender(cfg, guard, build)
	if err != nil {
		return err
	}
	// The sandbox is this binary re-executed: os.Executable is /app in the image, bin/gedour locally.
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("sandbox executable: %w", err)
	}
	runner := script.NewRunner(script.Options{Exe: exe, Args: []string{script.Subcommand}, Client: sender.Client, Guard: guard,
		HTTPHosts: cfg.AllowHTTPHosts, UserAgent: sender.UserAgent, Metrics: script.NewMetrics(reg)})
	routeMetrics := route.NewMetrics(reg)
	routes := route.New(pool, log, box, sender, runner, guard, cfg.AllowHTTPHosts, routeMetrics)
	hubCtx, stopHub := context.WithCancel(ctx)
	defer stopHub()
	servers := []*http.Server{newServer(cfg.MetricsAddr, server.Metrics(log, reg, ping))}
	if cfg.RunsWeb() {
		authn, err := auth.New(cfg, pool, log)
		if err != nil {
			return err
		}
		if authn.DevMode() {
			// 001-AC2: loud on purpose, dev mode must never go unnoticed in a real deployment.
			log.Warn("dev_mode_enabled", zap.String("user", cfg.DevUser))
		}
		watches := watch.New(pool, log, box, cfg.AllowHTTPHosts)
		servers = append(servers, newServer(cfg.HTTPAddr, server.New(log, build, ping, ui, authn, pool, watches, routes, reg)))
		go routes.RunHub(hubCtx)
	}

	errCh := make(chan error, len(servers))
	for _, srv := range servers {
		ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", srv.Addr)
		if err != nil {
			shutdown(log, servers, cfg.ShutdownTimeout)
			return fmt.Errorf("listen %s: %w", srv.Addr, err)
		}
		log.Info("listening", zap.String("addr", srv.Addr))
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("serve %s: %w", srv.Addr, err)
			}
		}()
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	var workers sync.WaitGroup
	if cfg.RunsWorker() {
		w, err := newWorker(cfg, pool, log, box, guard, build, reg)
		if err != nil {
			shutdown(log, servers, cfg.ShutdownTimeout)
			return err
		}
		workers.Go(func() { w.Run(workerCtx) })
		workers.Go(func() { route.NewRouter(pool, log, routeMetrics, cfg.WorkerPoll).Run(workerCtx) })
		workers.Go(func() {
			route.NewDeliverer(pool, log, box, sender, runner, routeMetrics, cfg.WorkerPoll, cfg.WorkerConcurrency, cfg.ShutdownTimeout).Run(workerCtx)
		})
	}

	select {
	case <-ctx.Done():
		err = nil
	case err = <-errCh:
	}
	log.Info("shutting_down")
	stopWorker()
	// Ends the inbox streams, which would otherwise hold Shutdown until its deadline.
	stopHub()
	shutdown(log, servers, cfg.ShutdownTimeout)
	// The worker drains within the same timeout; the pool closes only after it.
	workers.Wait()
	return err
}

// Retry delays while PostgreSQL is unreachable at startup.
const (
	dbRetryMin = 500 * time.Millisecond
	dbRetryMax = 10 * time.Second
)

// waitForDB pings until PostgreSQL answers, so a pod started during an outage recovers by itself;
// the orchestrator's startup probe bounds the wait. It reports false when ctx ended first.
func waitForDB(ctx context.Context, pool *pgxpool.Pool, log *zap.Logger) bool {
	delay := dbRetryMin
	for attempt := 1; ; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := pool.Ping(pingCtx)
		cancel()
		if err == nil {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		log.Warn("db_unreachable", zap.Int("attempt", attempt), zap.Duration("retry_in", delay), zap.Error(err))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
		delay = min(delay*2, dbRetryMax)
	}
}

// newSender builds the channel sender; webhooks, Telegram and Nextcloud Talk go through the guard and follow no redirect.
func newSender(cfg config.Config, guard *netguard.Guard, build server.BuildInfo) (*notify.Sender, error) {
	s := &notify.Sender{
		Client:      guard.Client(netguard.Options{Timeout: notify.SendTimeout, HTTPHosts: cfg.AllowHTTPHosts, NoRedirects: true}),
		UserAgent:   "gedour/" + build.Version,
		TelegramURL: cfg.TelegramAPIURL,
	}
	if !cfg.SMTP() {
		return s, nil
	}
	from, err := mail.ParseAddress(cfg.SMTPFrom)
	if err != nil {
		return nil, fmt.Errorf("GEDOUR_SMTP_FROM: %w", err)
	}
	nets := make([]netip.Prefix, 0, len(cfg.SMTPPlainNets))
	for _, n := range cfg.SMTPPlainNets {
		p, err := netip.ParsePrefix(n)
		if err != nil {
			return nil, fmt.Errorf("GEDOUR_SMTP_PLAIN_NETS: %w", err)
		}
		nets = append(nets, p.Masked())
	}
	s.Mailer = &notify.Mailer{Addr: cfg.SMTPAddr, Username: cfg.SMTPUsername, Password: cfg.SMTPPassword, From: from,
		TLS: cfg.SMTPTLS, PlainNets: nets}
	return s, nil
}

func newWorker(cfg config.Config, pool *pgxpool.Pool, log *zap.Logger, box *secret.Box, guard *netguard.Guard, build server.BuildInfo, reg prometheus.Registerer) (*worker.Worker, error) {
	client := guard.Client(netguard.Options{Timeout: 30 * time.Second, MaxRedirects: 5, HTTPHosts: cfg.AllowHTTPHosts,
		StripOnCrossHost: []string{"Authorization", "Private-Token"}})
	return worker.New(pool, log, box, client, collect.Collectors(), worker.NewMetrics(reg), worker.Options{
		Poll: cfg.WorkerPoll, Concurrency: cfg.WorkerConcurrency, DrainTimeout: cfg.ShutdownTimeout,
		UserAgent: "gedour/" + build.Version, HTTPHosts: cfg.AllowHTTPHosts,
	})
}

func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

func shutdown(log *zap.Logger, servers []*http.Server, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
			// Drain deadline hit: force-close rather than outlive the orchestrator's grace period.
			log.Error("shutdown_failed", zap.String("addr", srv.Addr), zap.Error(err))
			_ = srv.Close()
		}
	}
}
