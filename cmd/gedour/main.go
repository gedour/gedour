// Command gedour runs the gedour watch service (web and/or worker role).
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/term"

	"github.com/gedour/gedour/pkg/app"
	"github.com/gedour/gedour/pkg/config"
	"github.com/gedour/gedour/pkg/script"
	"github.com/gedour/gedour/pkg/server"
	"github.com/gedour/gedour/web"
)

var (
	Version        = "v0.0.0"
	CommitHash     = "0000000"
	BuildTimestamp = "1970-01-01T00:00:00"
	Builder        = "unknown"
	ProjectURL     = "unknown"
)

func newLogger(levelName, format string) (*zap.Logger, error) {
	level, err := zapcore.ParseLevel(levelName)
	if err != nil {
		return nil, fmt.Errorf("parse log level: %w", err)
	}
	var cfg zap.Config
	if format == "console" || (format == "auto" && term.IsTerminal(int(os.Stdout.Fd()))) {
		cfg = zap.NewDevelopmentConfig()
		cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	} else {
		cfg = zap.NewProductionConfig()
	}
	cfg.Level = zap.NewAtomicLevelAt(level)
	cfg.EncoderConfig.TimeKey = "timestamp"
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	return cfg.Build()
}

func main() {
	// `gedour sandbox` is the script child: it must run before any config or logger touches the process.
	script.MaybeSandbox()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gedour: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	flag.StringVar(&cfg.Role, "role", cfg.Role, "process role: all, web or worker (env GEDOUR_ROLE)")
	flag.Parse()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	logger, err := newLogger(cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		return err
	}
	defer func() { _ = logger.Sync() }()
	// pkg/db logs through the global logger: InTx has no logger of its own.
	zap.ReplaceGlobals(logger)

	build := server.BuildInfo{Version: Version, CommitHash: CommitHash, BuildTimestamp: BuildTimestamp, Builder: Builder, ProjectURL: ProjectURL}
	logger.Info("startup",
		zap.String("version", Version),
		zap.String("commit_hash", CommitHash),
		zap.String("build_timestamp", BuildTimestamp),
		zap.String("builder", Builder),
		zap.String("project_url", ProjectURL),
		zap.String("role", cfg.Role),
	)

	ui, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		return fmt.Errorf("embedded ui: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx, cfg, logger, build, ui); err != nil {
		logger.Error("run_failed", zap.Error(err))
		return err
	}
	logger.Info("stopped")
	return nil
}
