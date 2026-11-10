// Package config loads and validates gedour's runtime configuration.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"strconv"
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

// HTTPWriteTimeout bounds writing one response of the HTTP listeners; a handler doing slow work keeps under it.
const HTTPWriteTimeout = 30 * time.Second

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
	// PublicURL is the comma-separated browser-facing base URLs; the OIDC redirect is <url>/auth/callback.
	PublicURL string
	// DevUser enables single-user mode: every request runs as this user, no login.
	DevUser            string
	SessionTTL         time.Duration
	SessionIdleTimeout time.Duration
	// OIDCRolesClaim names the ID token claim holding the roles: a top-level key, else a dotted path.
	OIDCRolesClaim string
	// OIDCRequiredRole, when set, is needed to sign in; OIDCAdminRole, when set, makes an admin and also grants sign-in.
	OIDCRequiredRole string
	OIDCAdminRole    string
	// APIKeySigninMaxAge is how long after the owner's last sign-in an API key keeps working.
	APIKeySigninMaxAge time.Duration

	// EncryptionKey is base64 of the 32-byte AES key sealing credential secrets.
	EncryptionKey string
	// AllowHTTPHosts are hosts outbound requests may reach over plain http (watches, webhooks, Nextcloud Talk, script fetches).
	AllowHTTPHosts []string
	// AllowPrivateNets are CIDRs outbound requests may reach despite the SSRF guard.
	AllowPrivateNets []string

	// WorkerPoll is how often an idle worker looks for due watches.
	WorkerPoll time.Duration
	// WorkerConcurrency caps the runs one worker process executes at once.
	WorkerConcurrency int

	// TelegramAPIURL is the Bot API base URL (a stub in tests).
	TelegramAPIURL string
	// SMTPAddr (host:port) enables email channels; the other SMTP fields configure that server.
	SMTPAddr     string
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string
	// SMTPTLS is starttls, tls or none (loopback or SMTPPlainNets only).
	SMTPTLS string
	// SMTPPlainNets are the CIDRs TLS "none" may reach besides loopback.
	SMTPPlainNets []string
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
	keyAge, err := getDuration("GEDOUR_API_KEY_SIGNIN_MAX_AGE", "720h")
	if err != nil {
		return Config{}, err
	}
	poll, err := getDuration("GEDOUR_WORKER_POLL", "2s")
	if err != nil {
		return Config{}, err
	}
	concurrency, err := strconv.Atoi(getEnv("GEDOUR_WORKER_CONCURRENCY", "8"))
	if err != nil {
		return Config{}, fmt.Errorf("parse GEDOUR_WORKER_CONCURRENCY: %w", err)
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
		OIDCRolesClaim:     strings.TrimSpace(getEnv("GEDOUR_OIDC_ROLES_CLAIM", "roles")),
		OIDCRequiredRole:   strings.TrimSpace(os.Getenv("GEDOUR_OIDC_REQUIRED_ROLE")),
		OIDCAdminRole:      strings.TrimSpace(os.Getenv("GEDOUR_OIDC_ADMIN_ROLE")),
		APIKeySigninMaxAge: keyAge,
		EncryptionKey:      os.Getenv("GEDOUR_ENCRYPTION_KEY"),
		AllowHTTPHosts:     splitList(os.Getenv("GEDOUR_ALLOW_HTTP_HOSTS")),
		AllowPrivateNets:   splitList(os.Getenv("GEDOUR_ALLOW_PRIVATE_NETS")),
		WorkerPoll:         poll,
		WorkerConcurrency:  concurrency,
		TelegramAPIURL:     getEnv("GEDOUR_TELEGRAM_API_URL", "https://api.telegram.org"),
		SMTPAddr:           os.Getenv("GEDOUR_SMTP_ADDR"),
		SMTPUsername:       os.Getenv("GEDOUR_SMTP_USERNAME"),
		SMTPPassword:       os.Getenv("GEDOUR_SMTP_PASSWORD"),
		SMTPFrom:           os.Getenv("GEDOUR_SMTP_FROM"),
		SMTPTLS:            getEnv("GEDOUR_SMTP_TLS", "starttls"),
		SMTPPlainNets:      splitList(os.Getenv("GEDOUR_SMTP_PLAIN_NETS")),
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
	if c.RunsWorker() && (c.WorkerPoll <= 0 || c.WorkerConcurrency < 1) {
		errs = append(errs, errors.New("GEDOUR_WORKER_POLL must be positive and GEDOUR_WORKER_CONCURRENCY at least 1"))
	}
	// Both roles send: the worker deliveries, web the channel and rule tests.
	for _, n := range c.AllowPrivateNets {
		if _, err := netip.ParsePrefix(n); err != nil {
			errs = append(errs, fmt.Errorf("GEDOUR_ALLOW_PRIVATE_NETS: %w", err))
		}
	}
	if err := absoluteURL(c.TelegramAPIURL); err != nil {
		errs = append(errs, fmt.Errorf("GEDOUR_TELEGRAM_API_URL: %w", err))
	}
	errs = append(errs, c.validateSMTP()...)
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
		}
		errs = append(errs, c.validatePublicURLs()...)
		if strings.TrimSpace(c.OIDCRolesClaim) == "" {
			errs = append(errs, errors.New("GEDOUR_OIDC_ROLES_CLAIM must not be blank"))
		}
	}
	if c.SessionTTL <= 0 || c.SessionIdleTimeout <= 0 {
		errs = append(errs, errors.New("GEDOUR_SESSION_TTL and GEDOUR_SESSION_IDLE_TIMEOUT must be positive"))
	}
	if c.APIKeySigninMaxAge <= 0 {
		errs = append(errs, errors.New("GEDOUR_API_KEY_SIGNIN_MAX_AGE must be positive"))
	}
	return errs
}

// Each entry is selected by its host, so two entries on one host would be ambiguous.
func (c Config) validatePublicURLs() []error {
	var errs []error
	seen := map[string]bool{}
	for _, raw := range c.PublicURLs() {
		if err := absoluteURL(raw); err != nil {
			errs = append(errs, fmt.Errorf("GEDOUR_PUBLIC_URL: %w", err))
			continue
		}
		u, _ := url.Parse(raw)
		host := strings.ToLower(u.Host)
		if seen[host] {
			errs = append(errs, fmt.Errorf("GEDOUR_PUBLIC_URL: host %q listed twice", u.Host))
		}
		seen[host] = true
	}
	return errs
}

// PublicURLs splits GEDOUR_PUBLIC_URL, keeping blank entries so validation rejects them.
func (c Config) PublicURLs() []string {
	if c.PublicURL == "" {
		return nil
	}
	out := strings.Split(c.PublicURL, ",")
	for i := range out {
		out[i] = strings.TrimSpace(out[i])
	}
	return out
}

func (c Config) validateSMTP() []error {
	var errs []error
	switch c.SMTPTLS {
	case "starttls", "tls", "none":
	default:
		errs = append(errs, fmt.Errorf("GEDOUR_SMTP_TLS %q: must be starttls, tls or none", c.SMTPTLS))
	}
	if c.SMTPAddr == "" {
		return errs
	}
	if _, port, err := net.SplitHostPort(c.SMTPAddr); err != nil || port == "" {
		errs = append(errs, fmt.Errorf("GEDOUR_SMTP_ADDR %q: must be host:port", c.SMTPAddr))
	}
	if a, err := mail.ParseAddress(c.SMTPFrom); err != nil || !strings.Contains(a.Address, "@") {
		errs = append(errs, fmt.Errorf("GEDOUR_SMTP_FROM %q: must be an email address, e.g. gedour <gedour@example.com>", c.SMTPFrom))
	}
	if (c.SMTPUsername == "") != (c.SMTPPassword == "") {
		errs = append(errs, errors.New("GEDOUR_SMTP_USERNAME and GEDOUR_SMTP_PASSWORD must be set together"))
	}
	if c.SMTPTLS == "none" && c.SMTPUsername != "" {
		errs = append(errs, errors.New("GEDOUR_SMTP_TLS=none would send GEDOUR_SMTP_PASSWORD in clear: use starttls or tls"))
	}
	for _, n := range c.SMTPPlainNets {
		if _, err := netip.ParsePrefix(n); err != nil {
			errs = append(errs, fmt.Errorf("GEDOUR_SMTP_PLAIN_NETS: %w", err))
		}
	}
	return errs
}

// SMTP reports whether email channels can be used.
func (c Config) SMTP() bool { return c.SMTPAddr != "" }

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
