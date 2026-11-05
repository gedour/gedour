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
		EncryptionKey:      "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		WorkerPoll:         2 * time.Second,
		WorkerConcurrency:  8,
		TelegramAPIURL:     "https://api.telegram.org",
		SMTPTLS:            "starttls",
	}
}

func TestValidateWorker(t *testing.T) {
	for _, tt := range []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"defaults", func(*Config) {}, ""},
		{"allowlist", func(c *Config) { c.AllowPrivateNets = []string{"127.0.0.0/8", "fd00::/8"} }, ""},
		{"bad cidr", func(c *Config) { c.AllowPrivateNets = []string{"10.0.0.0"} }, "GEDOUR_ALLOW_PRIVATE_NETS"},
		{"zero poll", func(c *Config) { c.WorkerPoll = 0 }, "GEDOUR_WORKER_POLL"},
		{"no concurrency", func(c *Config) { c.WorkerConcurrency = 0 }, "GEDOUR_WORKER_CONCURRENCY"},
		{"web role ignores worker settings", func(c *Config) { c.Role, c.WorkerPoll = RoleWeb, 0 }, ""},
		{"web role checks the allowlist too", func(c *Config) { c.Role, c.AllowPrivateNets = RoleWeb, []string{"x"} }, "GEDOUR_ALLOW_PRIVATE_NETS"},
		{"telegram url", func(c *Config) { c.TelegramAPIURL = "api.telegram.org" }, "GEDOUR_TELEGRAM_API_URL"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := valid()
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("want %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidateSMTP(t *testing.T) {
	smtp := func(c *Config) { c.SMTPAddr, c.SMTPFrom = "smtp.example:587", "gedour <gedour@example.com>" }
	for _, tt := range []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"unset", func(*Config) {}, ""},
		{"configured", smtp, ""},
		{"with login", func(c *Config) { smtp(c); c.SMTPUsername, c.SMTPPassword = "u", "p" }, ""},
		{"bad tls", func(c *Config) { c.SMTPTLS = "ssl" }, "GEDOUR_SMTP_TLS"},
		{"no port", func(c *Config) { smtp(c); c.SMTPAddr = "smtp.example" }, "GEDOUR_SMTP_ADDR"},
		{"no from", func(c *Config) { smtp(c); c.SMTPFrom = "" }, "GEDOUR_SMTP_FROM"},
		{"bare name from", func(c *Config) { smtp(c); c.SMTPFrom = "gedour" }, "GEDOUR_SMTP_FROM"},
		{"half a login", func(c *Config) { smtp(c); c.SMTPUsername = "u" }, "GEDOUR_SMTP_USERNAME"},
		{"login in clear", func(c *Config) { smtp(c); c.SMTPTLS, c.SMTPUsername, c.SMTPPassword = "none", "u", "p" }, "in clear"},
		{"plain nets", func(c *Config) { smtp(c); c.SMTPTLS, c.SMTPPlainNets = "none", []string{"172.16.0.0/12"} }, ""},
		{"bad plain net", func(c *Config) { smtp(c); c.SMTPPlainNets = []string{"172.16.0.0"} }, "GEDOUR_SMTP_PLAIN_NETS"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := valid()
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("want %q, got %v", tt.wantErr, err)
			}
			if tt.wantErr == "" && c.SMTP() != (c.SMTPAddr != "") {
				t.Fatal("SMTP() disagrees with SMTPAddr")
			}
		})
	}
}

func withOIDC(c *Config) {
	c.DevUser = ""
	c.OIDCIssuer, c.OIDCClientID, c.OIDCClientSecret = "https://sso.example/realms/r", "gedour", "s"
	c.PublicURL = "https://gedour.example"
	c.OIDCRolesClaim = "roles"
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
		{"public url list", func(c *Config) { withOIDC(c); c.PublicURL = "https://a.example.com, https://b.example.com/gedour" }, ""},
		{"same host other port", func(c *Config) { withOIDC(c); c.PublicURL = "https://a.example.com,https://a.example.com:8443" }, ""},
		{"blank public url entry", func(c *Config) { withOIDC(c); c.PublicURL = "https://a.example.com,,https://b.example.com" }, `"": must be an absolute`},
		{"trailing comma", func(c *Config) { withOIDC(c); c.PublicURL = "https://a.example.com, " }, `"": must be an absolute`},
		{"only commas", func(c *Config) { withOIDC(c); c.PublicURL = " , " }, `"": must be an absolute`},
		{"bad second entry", func(c *Config) { withOIDC(c); c.PublicURL = "https://a.example.com,ftp://x" }, `"ftp://x"`},
		{"duplicate public url", func(c *Config) { withOIDC(c); c.PublicURL = "https://a.example.com,https://a.example.com" }, "listed twice"},
		{"duplicate host", func(c *Config) { withOIDC(c); c.PublicURL = "https://a.example.com,http://A.Example.com/x" }, "listed twice"},
		{"bad issuer", func(c *Config) { withOIDC(c); c.OIDCIssuer = "sso.example" }, "OIDC_ISSUER"},
		{"unparsable issuer", func(c *Config) { withOIDC(c); c.OIDCIssuer = "http://[::1" }, "OIDC_ISSUER"},
		{"blank roles claim", func(c *Config) { withOIDC(c); c.OIDCRolesClaim = " " }, "GEDOUR_OIDC_ROLES_CLAIM"},
		{"dotted roles claim", func(c *Config) { withOIDC(c); c.OIDCRolesClaim = "realm_access.roles" }, ""},
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
		{"no encryption key", func(c *Config) { c.EncryptionKey = "" }, "GEDOUR_ENCRYPTION_KEY"},
		{"no encryption key on worker", func(c *Config) { c.Role = RoleWorker; c.EncryptionKey = "" }, "GEDOUR_ENCRYPTION_KEY"},
		{"short encryption key", func(c *Config) { c.EncryptionKey = "AAAA" }, "GEDOUR_ENCRYPTION_KEY"},
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

	if c.WorkerPoll != 2*time.Second || c.WorkerConcurrency != 8 || c.AllowPrivateNets != nil {
		t.Fatalf("worker defaults: %+v", c)
	}
	if c.TelegramAPIURL != "https://api.telegram.org" || c.SMTPTLS != "starttls" || c.SMTP() {
		t.Fatalf("channel defaults: %+v", c)
	}
	if c.SessionTTL != 168*time.Hour || c.SessionIdleTimeout != 24*time.Hour {
		t.Fatalf("session defaults: %v %v", c.SessionTTL, c.SessionIdleTimeout)
	}
	if c.OIDCRolesClaim != "roles" || c.OIDCRequiredRole != "" || c.OIDCAdminRole != "" {
		t.Fatalf("role defaults: %+v", c)
	}

	t.Setenv("OIDC_ISSUER", "https://i")
	t.Setenv("OIDC_CLIENT_ID", "id")
	t.Setenv("OIDC_CLIENT_SECRET", "sec")
	t.Setenv("GEDOUR_PUBLIC_URL", "https://p")
	t.Setenv("GEDOUR_DEV_USER", "d")
	t.Setenv("GEDOUR_SESSION_TTL", "2h")
	t.Setenv("GEDOUR_SESSION_IDLE_TIMEOUT", "30m")
	t.Setenv("GEDOUR_OIDC_ROLES_CLAIM", " realm_access.roles ")
	t.Setenv("GEDOUR_OIDC_REQUIRED_ROLE", "\tgedour-user ")
	t.Setenv("GEDOUR_OIDC_ADMIN_ROLE", " gedour-admin\n")
	t.Setenv("GEDOUR_ENCRYPTION_KEY", "k")
	t.Setenv("GEDOUR_ALLOW_HTTP_HOSTS", " Localhost , ,127.0.0.1")
	t.Setenv("GEDOUR_ALLOW_PRIVATE_NETS", "127.0.0.0/8, 10.0.0.0/8")
	t.Setenv("GEDOUR_WORKER_POLL", "500ms")
	t.Setenv("GEDOUR_WORKER_CONCURRENCY", "3")
	t.Setenv("GEDOUR_TELEGRAM_API_URL", "http://127.0.0.1:1")
	t.Setenv("GEDOUR_SMTP_ADDR", "mail:25")
	t.Setenv("GEDOUR_SMTP_USERNAME", "su")
	t.Setenv("GEDOUR_SMTP_PASSWORD", "sp")
	t.Setenv("GEDOUR_SMTP_FROM", "g@x.test")
	t.Setenv("GEDOUR_SMTP_TLS", "none")
	t.Setenv("GEDOUR_SMTP_PLAIN_NETS", "10.0.0.0/8, 172.16.0.0/12")
	if c, err = Load(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.AllowPrivateNets, "|") != "127.0.0.0/8|10.0.0.0/8" || c.WorkerPoll != 500*time.Millisecond || c.WorkerConcurrency != 3 {
		t.Fatalf("worker env not read: %+v", c)
	}
	if c.TelegramAPIURL != "http://127.0.0.1:1" || c.SMTPAddr != "mail:25" || c.SMTPUsername != "su" || c.SMTPPassword != "sp" ||
		c.SMTPFrom != "g@x.test" || c.SMTPTLS != "none" || strings.Join(c.SMTPPlainNets, "|") != "10.0.0.0/8|172.16.0.0/12" {
		t.Fatalf("channel env not read: %+v", c)
	}
	if c.EncryptionKey != "k" || strings.Join(c.AllowHTTPHosts, "|") != "localhost|127.0.0.1" {
		t.Fatalf("encryption env not read: %q %q", c.EncryptionKey, c.AllowHTTPHosts)
	}
	if c.OIDCIssuer != "https://i" || c.OIDCClientID != "id" || c.OIDCClientSecret != "sec" || c.PublicURL != "https://p" ||
		c.DevUser != "d" || c.SessionTTL != 2*time.Hour || c.SessionIdleTimeout != 30*time.Minute {
		t.Fatalf("auth env not read: %+v", c)
	}
	if c.OIDCRolesClaim != "realm_access.roles" || c.OIDCRequiredRole != "gedour-user" || c.OIDCAdminRole != "gedour-admin" {
		t.Fatalf("role env not read or not trimmed: %+v", c)
	}
	t.Run("blank roles claim", func(t *testing.T) {
		t.Setenv("GEDOUR_OIDC_ROLES_CLAIM", "  ")
		if c, err := Load(); err != nil || c.OIDCRolesClaim != "" {
			t.Fatalf("want a blank claim left for Validate, got %q %v", c.OIDCRolesClaim, err)
		}
	})

	for _, key := range []string{"GEDOUR_SHUTDOWN_TIMEOUT", "GEDOUR_SESSION_TTL", "GEDOUR_SESSION_IDLE_TIMEOUT", "GEDOUR_WORKER_POLL", "GEDOUR_WORKER_CONCURRENCY"} {
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
