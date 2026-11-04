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

	t.Setenv("GEDOUR_SHUTDOWN_TIMEOUT", "soon")
	if _, err := Load(); err == nil {
		t.Fatal("want error on bad duration")
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
