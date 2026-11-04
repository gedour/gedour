// Package config loads and validates gedour's runtime configuration.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap/zapcore"

	"github.com/gedour/gedour/pkg/secret"
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

	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string
	// PublicURL is the browser-facing base URL; the OIDC redirect is PublicURL + /auth/callback.
	PublicURL string
	// DevUser enables single-user mode: every request runs as this user, no login.
	DevUser            string
	SessionTTL         time.Duration
	SessionIdleTimeout time.Duration

	// EncryptionKey is base64 of the 32-byte AES key sealing credential secrets.
	EncryptionKey string
	// AllowHTTPHosts are hosts a watch URL may reach over plain http (tests, lab mirrors).
	AllowHTTPHosts []string
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getDuration(key, fallback string) (time.Duration, error) {
	d, err := time.ParseDuration(getEnv(key, fallback))
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	return d, nil
}

// Load reads the configuration from the environment; call Validate before use.
func Load() (Config, error) {
	timeout, err := getDuration("GEDOUR_SHUTDOWN_TIMEOUT", "15s")
	if err != nil {
		return Config{}, err
	}
	ttl, err := getDuration("GEDOUR_SESSION_TTL", "168h")
	if err != nil {
		return Config{}, err
	}
	idle, err := getDuration("GEDOUR_SESSION_IDLE_TIMEOUT", "24h")
	if err != nil {
		return Config{}, err
	}
	return Config{
		Role:            getEnv("GEDOUR_ROLE", RoleAll),
		HTTPAddr:        getEnv("GEDOUR_HTTP_ADDR", ":8080"),
		MetricsAddr:     getEnv("GEDOUR_METRICS_ADDR", ":9090"),
		DatabaseURL:     os.Getenv("GEDOUR_DATABASE_URL"),
		LogLevel:        getEnv("GEDOUR_LOG_LEVEL", "info"),
		LogFormat:       getEnv("GEDOUR_LOG_FORMAT", "auto"),
		ShutdownTimeout: timeout,

		OIDCIssuer:         os.Getenv("OIDC_ISSUER"),
		OIDCClientID:       os.Getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret:   os.Getenv("OIDC_CLIENT_SECRET"),
		PublicURL:          os.Getenv("GEDOUR_PUBLIC_URL"),
		DevUser:            os.Getenv("GEDOUR_DEV_USER"),
		SessionTTL:         ttl,
		SessionIdleTimeout: idle,
		EncryptionKey:      os.Getenv("GEDOUR_ENCRYPTION_KEY"),
		AllowHTTPHosts:     splitList(os.Getenv("GEDOUR_ALLOW_HTTP_HOSTS")),
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
	// Both roles read credentials: web seals them, the worker opens them.
	if _, err := secret.ParseKey(c.EncryptionKey); err != nil {
		errs = append(errs, fmt.Errorf("GEDOUR_ENCRYPTION_KEY: %w", err))
	}
	// A worker serves no user, so it needs no authentication settings.
	if c.RunsWeb() {
		errs = append(errs, c.validateAuth()...)
	}
	return errors.Join(errs...)
}

// 001-AC2: exactly one of OIDC or the dev user.
func (c Config) validateAuth() []error {
	var errs []error
	set := 0
	for _, v := range []string{c.OIDCIssuer, c.OIDCClientID, c.OIDCClientSecret} {
		if v != "" {
			set++
		}
	}
	switch {
	case set > 0 && c.DevUser != "":
		errs = append(errs, errors.New("GEDOUR_DEV_USER and OIDC_* are mutually exclusive: unset one"))
	case set == 0 && c.DevUser == "":
		errs = append(errs, errors.New("no authentication configured: set OIDC_ISSUER, OIDC_CLIENT_ID, "+
			"OIDC_CLIENT_SECRET and GEDOUR_PUBLIC_URL, or GEDOUR_DEV_USER for local single-user mode"))
	case set > 0 && set < 3:
		errs = append(errs, errors.New("OIDC_ISSUER, OIDC_CLIENT_ID and OIDC_CLIENT_SECRET must be set together"))
	}
	if set > 0 {
		if err := absoluteURL(c.OIDCIssuer); c.OIDCIssuer != "" && err != nil {
			errs = append(errs, fmt.Errorf("OIDC_ISSUER: %w", err))
		}
		if c.PublicURL == "" {
			errs = append(errs, errors.New("GEDOUR_PUBLIC_URL is required with OIDC"))
		} else if err := absoluteURL(c.PublicURL); err != nil {
			errs = append(errs, fmt.Errorf("GEDOUR_PUBLIC_URL: %w", err))
		}
	}
	if c.SessionTTL <= 0 || c.SessionIdleTimeout <= 0 {
		errs = append(errs, errors.New("GEDOUR_SESSION_TTL and GEDOUR_SESSION_IDLE_TIMEOUT must be positive"))
	}
	return errs
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, strings.ToLower(v))
		}
	}
	return out
}

func absoluteURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q: must be an absolute http(s) URL", raw)
	}
	return nil
}

// OIDC reports whether login goes through an OIDC provider (otherwise dev single-user mode).
func (c Config) OIDC() bool { return c.DevUser == "" }

// RunsWeb reports whether this process serves the HTTP API and UI.
func (c Config) RunsWeb() bool { return c.Role == RoleAll || c.Role == RoleWeb }

// RunsWorker reports whether this process runs background jobs.
func (c Config) RunsWorker() bool { return c.Role == RoleAll || c.Role == RoleWorker }
