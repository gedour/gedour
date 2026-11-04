package config

import (
	"strings"
	"testing"
	"time"
)

func valid() Config {
	return Config{
		Role:            RoleAll,
		HTTPAddr:        ":8080",
		MetricsAddr:     ":9090",
		DatabaseURL:     "postgres://u@localhost:5432/db",
		LogLevel:        "info",
		LogFormat:       "auto",
		ShutdownTimeout: time.Second,

		DevUser:            "dev@local",
		SessionTTL:         time.Hour,
		SessionIdleTimeout: time.Minute,
	}
}

func withOIDC(c *Config) {
	c.DevUser = ""
	c.OIDCIssuer, c.OIDCClientID, c.OIDCClientSecret = "https://sso.example/realms/r", "gedour", "s"
	c.PublicURL = "https://gedour.example"
}

// 001-AC2: exactly one of OIDC or dev user, OIDC complete with a public URL; workers need neither.
func TestValidateAuth(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"dev user", func(*Config) {}, ""},
		{"oidc", withOIDC, ""},
		{"both", func(c *Config) { withOIDC(c); c.DevUser = "dev" }, "mutually exclusive"},
		{"neither", func(c *Config) { c.DevUser = "" }, "no authentication configured"},
		{"neither on worker", func(c *Config) { c.DevUser = ""; c.Role = RoleWorker }, ""},
		{"missing secret", func(c *Config) { withOIDC(c); c.OIDCClientSecret = "" }, "must be set together"},
		{"issuer only", func(c *Config) { c.DevUser = ""; c.OIDCIssuer = "https://sso.example" }, "must be set together"},
		{"client id only", func(c *Config) { c.DevUser = ""; c.OIDCClientID = "x" }, "must be set together"},
		{"no public url", func(c *Config) { withOIDC(c); c.PublicURL = "" }, "GEDOUR_PUBLIC_URL is required"},
		{"relative public url", func(c *Config) { withOIDC(c); c.PublicURL = "/gedour" }, "GEDOUR_PUBLIC_URL"},
		{"ftp public url", func(c *Config) { withOIDC(c); c.PublicURL = "ftp://x" }, "GEDOUR_PUBLIC_URL"},
		{"bad issuer", func(c *Config) { withOIDC(c); c.OIDCIssuer = "sso.example" }, "OIDC_ISSUER"},
		{"unparsable issuer", func(c *Config) { withOIDC(c); c.OIDCIssuer = "http://[::1" }, "OIDC_ISSUER"},
		{"zero ttl", func(c *Config) { c.SessionTTL = 0 }, "GEDOUR_SESSION_TTL"},
		{"zero idle", func(c *Config) { c.SessionIdleTimeout = 0 }, "GEDOUR_SESSION_IDLE_TIMEOUT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid()
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
	c := valid()
	if c.OIDC() {
		t.Fatal("dev user config must not report OIDC")
	}
	withOIDC(&c)
	if !c.OIDC() {
		t.Fatal("OIDC config must report OIDC")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"valid", func(*Config) {}, ""},
		{"web role", func(c *Config) { c.Role = RoleWeb }, ""},
		{"worker role", func(c *Config) { c.Role = RoleWorker }, ""},
		{"bad role", func(c *Config) { c.Role = "both" }, "role"},
		{"missing db url", func(c *Config) { c.DatabaseURL = "" }, "GEDOUR_DATABASE_URL is required"},
		{"bad db url", func(c *Config) { c.DatabaseURL = "postgres://u:p@host:notaport/db" }, "GEDOUR_DATABASE_URL"},
		{"empty addr", func(c *Config) { c.HTTPAddr = "" }, "must be set"},
		{"same addr", func(c *Config) { c.MetricsAddr = c.HTTPAddr }, "must differ"},
		{"bad log level", func(c *Config) { c.LogLevel = "loud" }, "GEDOUR_LOG_LEVEL"},
		{"empty log level", func(c *Config) { c.LogLevel = "" }, ""},
		{"bad log format", func(c *Config) { c.LogFormat = "xml" }, "log format"},
		{"zero timeout", func(c *Config) { c.ShutdownTimeout = 0 }, "SHUTDOWN_TIMEOUT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid()
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidateReportsAllErrors(t *testing.T) {
	c := valid()
	c.Role = "x"
	c.LogLevel = "loud"
	c.DatabaseURL = ""
	err := c.Validate()
	for _, want := range []string{"role", "GEDOUR_LOG_LEVEL", "GEDOUR_DATABASE_URL"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v lacks %q", err, want)
		}
	}
}

func TestLoad(t *testing.T) {
	t.Setenv("GEDOUR_ROLE", "worker")
	t.Setenv("GEDOUR_DATABASE_URL", "postgres://x@y/z")
	t.Setenv("GEDOUR_SHUTDOWN_TIMEOUT", "3s")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Role != RoleWorker || c.HTTPAddr != ":8080" || c.MetricsAddr != ":9090" || c.ShutdownTimeout != 3*time.Second {
		t.Fatalf("unexpected config: %+v", c)
	}
	if c.RunsWeb() || !c.RunsWorker() {
		t.Fatal("worker role must run the worker only")
	}

	if c.SessionTTL != 168*time.Hour || c.SessionIdleTimeout != 24*time.Hour {
		t.Fatalf("session defaults: %v %v", c.SessionTTL, c.SessionIdleTimeout)
	}

	t.Setenv("OIDC_ISSUER", "https://i")
	t.Setenv("OIDC_CLIENT_ID", "id")
	t.Setenv("OIDC_CLIENT_SECRET", "sec")
	t.Setenv("GEDOUR_PUBLIC_URL", "https://p")
	t.Setenv("GEDOUR_DEV_USER", "d")
	t.Setenv("GEDOUR_SESSION_TTL", "2h")
	t.Setenv("GEDOUR_SESSION_IDLE_TIMEOUT", "30m")
	if c, err = Load(); err != nil {
		t.Fatal(err)
	}
	if c.OIDCIssuer != "https://i" || c.OIDCClientID != "id" || c.OIDCClientSecret != "sec" || c.PublicURL != "https://p" ||
		c.DevUser != "d" || c.SessionTTL != 2*time.Hour || c.SessionIdleTimeout != 30*time.Minute {
		t.Fatalf("auth env not read: %+v", c)
	}

	for _, key := range []string{"GEDOUR_SHUTDOWN_TIMEOUT", "GEDOUR_SESSION_TTL", "GEDOUR_SESSION_IDLE_TIMEOUT"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "soon")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("want error naming %s, got %v", key, err)
			}
		})
	}
}

func TestRoles(t *testing.T) {
	tests := []struct {
		role        string
		web, worker bool
	}{
		{RoleAll, true, true},
		{RoleWeb, true, false},
		{RoleWorker, false, true},
	}
	for _, tt := range tests {
		c := Config{Role: tt.role}
		if c.RunsWeb() != tt.web || c.RunsWorker() != tt.worker {
			t.Errorf("role %s: web=%v worker=%v", tt.role, c.RunsWeb(), c.RunsWorker())
		}
	}
}
