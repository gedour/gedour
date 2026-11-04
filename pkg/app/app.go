// Package app wires config, database, HTTP listeners and the worker into one process lifecycle.
package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/auth"
	"github.com/gedour/gedour/pkg/config"
	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/server"
)

// Run migrates the database, serves until ctx is cancelled, then drains within cfg.ShutdownTimeout.
func Run(ctx context.Context, cfg config.Config, log *zap.Logger, build server.BuildInfo, ui fs.FS) error {
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	applied, err := db.Migrate(ctx, pool)
	if err != nil {
		return err
	}
	log.Info("migrations_applied", zap.Int("count", applied))

	ping := server.PingFunc(pool.Ping)
	servers := []*http.Server{newServer(cfg.MetricsAddr, server.Metrics(log, server.NewRegistry(build), ping))}
	if cfg.RunsWeb() {
		authn, err := auth.New(cfg, pool, log)
		if err != nil {
			return err
		}
		if authn.DevMode() {
			// 001-AC2: loud on purpose, dev mode must never go unnoticed in a real deployment.
			log.Warn("dev_mode_enabled", zap.String("user", cfg.DevUser))
		}
		servers = append(servers, newServer(cfg.HTTPAddr, server.New(log, build, ping, ui, authn, pool)))
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
	if cfg.RunsWorker() {
		// ponytail: no jobs before phase 4; the worker only marks its lifetime.
		log.Info("worker_started")
	}

	select {
	case <-ctx.Done():
		err = nil
	case err = <-errCh:
	}
	log.Info("shutting_down")
	shutdown(log, servers, cfg.ShutdownTimeout)
	return err
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
