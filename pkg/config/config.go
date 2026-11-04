// Package config loads and validates gedour's runtime configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap/zapcore"
)

// Roles a process can run.
const (
	RoleAll    = "all"
	RoleWeb    = "web"
	RoleWorker = "worker"
)

// Config is the process configuration, read from GEDOUR_* environment variables.
type Config struct {
	Role            string
	HTTPAddr        string
	MetricsAddr     string
	DatabaseURL     string
	LogLevel        string
	LogFormat       string
	ShutdownTimeout time.Duration
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Load reads the configuration from the environment; call Validate before use.
func Load() (Config, error) {
	timeout, err := time.ParseDuration(getEnv("GEDOUR_SHUTDOWN_TIMEOUT", "15s"))
	if err != nil {
		return Config{}, fmt.Errorf("parse GEDOUR_SHUTDOWN_TIMEOUT: %w", err)
	}
	return Config{
		Role:            getEnv("GEDOUR_ROLE", RoleAll),
		HTTPAddr:        getEnv("GEDOUR_HTTP_ADDR", ":8080"),
		MetricsAddr:     getEnv("GEDOUR_METRICS_ADDR", ":9090"),
		DatabaseURL:     os.Getenv("GEDOUR_DATABASE_URL"),
		LogLevel:        getEnv("GEDOUR_LOG_LEVEL", "info"),
		LogFormat:       getEnv("GEDOUR_LOG_FORMAT", "auto"),
		ShutdownTimeout: timeout,
	}, nil
}

// Validate reports every invalid field at once.
func (c Config) Validate() error {
	var errs []error
	switch c.Role {
	case RoleAll, RoleWeb, RoleWorker:
	default:
		errs = append(errs, fmt.Errorf("role %q: must be one of all, web, worker", c.Role))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("GEDOUR_DATABASE_URL is required"))
	} else if _, err := pgxpool.ParseConfig(c.DatabaseURL); err != nil {
		errs = append(errs, fmt.Errorf("GEDOUR_DATABASE_URL: %w", err))
	}
	if c.HTTPAddr == "" || c.MetricsAddr == "" {
		errs = append(errs, errors.New("GEDOUR_HTTP_ADDR and GEDOUR_METRICS_ADDR must be set"))
	} else if c.HTTPAddr == c.MetricsAddr {
		errs = append(errs, errors.New("GEDOUR_HTTP_ADDR and GEDOUR_METRICS_ADDR must differ"))
	}
	if _, err := zapcore.ParseLevel(c.LogLevel); err != nil {
		errs = append(errs, fmt.Errorf("GEDOUR_LOG_LEVEL: %w", err))
	}
	switch c.LogFormat {
	case "auto", "console", "json":
	default:
		errs = append(errs, fmt.Errorf("log format %q: must be auto, console or json", c.LogFormat))
	}
	if c.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("GEDOUR_SHUTDOWN_TIMEOUT must be positive"))
	}
	return errors.Join(errs...)
}

// RunsWeb reports whether this process serves the HTTP API and UI.
func (c Config) RunsWeb() bool { return c.Role == RoleAll || c.Role == RoleWeb }

// RunsWorker reports whether this process runs background jobs.
func (c Config) RunsWorker() bool { return c.Role == RoleAll || c.Role == RoleWorker }
