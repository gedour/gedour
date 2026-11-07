package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/gedour/gedour/pkg/config"
	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/netguard"
	"github.com/gedour/gedour/pkg/notify/webpush"
	"github.com/gedour/gedour/pkg/secret"
	"github.com/gedour/gedour/pkg/server"
	"github.com/gedour/gedour/pkg/testpg"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testpg.Terminate()
	os.Exit(code)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

func cfgFor(t *testing.T, role, dbURL string) config.Config {
	return config.Config{
		Role:            role,
		HTTPAddr:        freeAddr(t),
		MetricsAddr:     freeAddr(t),
		DatabaseURL:     dbURL,
		LogFormat:       "json",
		ShutdownTimeout: 5 * time.Second,

		DevUser:            "dev@local",
		SessionTTL:         time.Hour,
		SessionIdleTimeout: time.Hour,
		APIKeySigninMaxAge: time.Hour,
		EncryptionKey:      "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		WorkerPoll:         50 * time.Millisecond,
		WorkerConcurrency:  2,
	}
}

func getStatus(url string) (int, string) {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func waitFor(t *testing.T, url string, code int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if c, _ := getStatus(url); c == code {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never answered %d", url, code)
}

// 001-AC27: the real process serves /healthz 200 with PG up, exposes /metrics, and shuts down gracefully.
func TestRunServesAndShutsDown(t *testing.T) {
	cfg := cfgFor(t, config.RoleAll, testpg.NewDB(t))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	core, logs := observer.New(zap.InfoLevel)
	go func() {
		done <- Run(ctx, cfg, zap.New(core), server.BuildInfo{Version: "v9.9.9", CommitHash: "c0ffee0"}, fstest.MapFS{"index.html": {Data: []byte("SPA")}})
	}()

	waitFor(t, "http://"+cfg.HTTPAddr+"/healthz", http.StatusOK)
	if code, body := getStatus("http://" + cfg.MetricsAddr + "/metrics"); code != http.StatusOK || !strings.Contains(body, `gedour_build_info{commit="c0ffee0",version="v9.9.9"} 1`) ||
		!strings.Contains(body, "gedour_watches_due 0") || !strings.Contains(body, "gedour_sse_subscribers 0") ||
		!strings.Contains(body, "gedour_events_routed_total 0") {
		t.Fatalf("metrics: %d", code)
	}
	if code, body := getStatus("http://" + cfg.HTTPAddr + "/"); code != http.StatusOK || body != "SPA" {
		t.Fatalf("spa: %d %q", code, body)
	}
	// 001-AC2: dev mode is announced at startup, and the API answers as the dev user.
	if w := logs.FilterMessage("dev_mode_enabled").All(); len(w) != 1 || w[0].Level != zap.WarnLevel {
		t.Fatalf("dev_mode_enabled warning: %+v", w)
	}
	if code, body := getStatus("http://" + cfg.HTTPAddr + "/api/v1/me"); code != http.StatusOK || !strings.Contains(body, `"dev@local"`) {
		t.Fatalf("me: %d %s", code, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if code, _ := getStatus("http://" + cfg.HTTPAddr + "/healthz"); code != 0 {
		t.Fatal("listener still open after shutdown")
	}
}

// A VAPID row sealed under another encryption key leaves Web Push off; the process still starts.
func TestRunWithUnreadableVAPIDKey(t *testing.T) {
	cfg := cfgFor(t, config.RoleWorker, testpg.NewDB(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	foreign, _ := secret.New("CAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAg=")
	if _, err := webpush.LoadOrCreate(ctx, pool, foreign); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.InfoLevel)
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, zap.New(core), server.BuildInfo{}, fstest.MapFS{}) }()
	waitFor(t, "http://"+cfg.MetricsAddr+"/healthz", http.StatusOK)
	if l := logs.FilterMessage("vapid_key_unreadable").All(); len(l) != 1 || l[0].Level != zap.ErrorLevel {
		t.Errorf("vapid_key_unreadable: %+v", l)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunWorkerOnly(t *testing.T) {
	cfg := cfgFor(t, config.RoleWorker, testpg.NewDB(t))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, zap.NewNop(), server.BuildInfo{}, fstest.MapFS{}) }()

	waitFor(t, "http://"+cfg.MetricsAddr+"/healthz", http.StatusOK)
	if code, _ := getStatus("http://" + cfg.HTTPAddr + "/healthz"); code != 0 {
		t.Fatal("worker role must not open the main listener")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNewSender(t *testing.T) {
	cfg := config.Config{TelegramAPIURL: "https://api.telegram.org", SMTPPlainNets: []string{"10.9.2.3/16"}}
	guard, _ := netguard.New(nil)
	s, err := newSender(cfg, guard, server.BuildInfo{Version: "v1"})
	if err != nil || s.Mailer != nil || s.UserAgent != "gedour/v1" {
		t.Fatalf("without SMTP: %+v %v", s, err)
	}
	for _, tt := range []struct{ role, public, sub string }{
		{config.RoleWorker, "", vapidContact},
		{config.RoleAll, "https://gedour.example,https://other.example", "https://gedour.example"},
		{config.RoleAll, "http://localhost:8080", vapidContact},
		{config.RoleAll, "HTTPS://Gedour.example", "HTTPS://Gedour.example"},
	} {
		c := config.Config{Role: tt.role, PublicURL: tt.public, AllowHTTPHosts: []string{"127.0.0.1"}}
		ws, err := newSender(c, guard, server.BuildInfo{})
		if err != nil || ws.VAPIDSubject != tt.sub || !slices.Equal(ws.HTTPHosts, c.AllowHTTPHosts) {
			t.Errorf("public %q: subject %q, http hosts %v, %v", tt.public, ws.VAPIDSubject, ws.HTTPHosts, err)
		}
	}
	cfg.SMTPAddr, cfg.SMTPFrom, cfg.SMTPTLS = "mail:25", "gedour <g@x.test>", "none"
	if s, err = newSender(cfg, guard, server.BuildInfo{}); err != nil || s.Mailer == nil || s.Mailer.From.Address != "g@x.test" ||
		len(s.Mailer.PlainNets) != 1 || s.Mailer.PlainNets[0].String() != "10.9.0.0/16" {
		t.Fatalf("with SMTP: %+v %v", s.Mailer, err)
	}
	cfg.SMTPFrom = "bad"
	if _, err := newSender(cfg, guard, server.BuildInfo{}); err == nil {
		t.Fatal("want a from error")
	}
	cfg.SMTPFrom, cfg.SMTPPlainNets = "g@x.test", []string{"x"}
	if _, err := newSender(cfg, guard, server.BuildInfo{}); err == nil {
		t.Fatal("want a network error")
	}
}

func TestRunErrors(t *testing.T) {
	// SIGTERM while waiting for the database is a clean stop: nil, so main logs no run_failed.
	t.Run("db unreachable until stopped", func(t *testing.T) {
		cfg := cfgFor(t, config.RoleAll, "postgres://u:p@127.0.0.1:1/db?connect_timeout=1")
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		core, logs := observer.New(zap.WarnLevel)
		if err := Run(ctx, cfg, zap.New(core), server.BuildInfo{}, fstest.MapFS{}); err != nil {
			t.Fatalf("want nil on stop, got %v", err)
		}
		if n := logs.FilterMessage("db_unreachable").Len(); n < 2 {
			t.Fatalf("want retries, got %d db_unreachable logs", n)
		}
	})
	t.Run("bad url", func(t *testing.T) {
		cfg := cfgFor(t, config.RoleAll, "postgres://u@h:notaport/db")
		if err := Run(context.Background(), cfg, zap.NewNop(), server.BuildInfo{}, fstest.MapFS{}); err == nil {
			t.Fatal("want connect error")
		}
	})
	t.Run("bad private nets", func(t *testing.T) {
		cfg := cfgFor(t, config.RoleWorker, testpg.NewDB(t))
		cfg.AllowPrivateNets = []string{"nope"}
		if err := Run(context.Background(), cfg, zap.NewNop(), server.BuildInfo{}, fstest.MapFS{}); err == nil || !strings.Contains(err.Error(), "allowed network") {
			t.Fatalf("want guard error, got %v", err)
		}
	})
	t.Run("bad key", func(t *testing.T) {
		cfg := cfgFor(t, config.RoleWorker, testpg.NewDB(t))
		cfg.EncryptionKey = "short"
		if err := Run(context.Background(), cfg, zap.NewNop(), server.BuildInfo{}, fstest.MapFS{}); err == nil {
			t.Fatal("want key error")
		}
	})
	t.Run("port taken", func(t *testing.T) {
		ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		cfg := cfgFor(t, config.RoleAll, testpg.NewDB(t))
		cfg.HTTPAddr = ln.Addr().String()
		if err := Run(context.Background(), cfg, zap.NewNop(), server.BuildInfo{}, fstest.MapFS{}); err == nil || !strings.Contains(err.Error(), "listen") {
			t.Fatalf("want listen error, got %v", err)
		}
	})
}

// pgProxy forwards TCP to PostgreSQL; closed, the database looks down (connection refused).
type pgProxy struct {
	addr, target string
	mu           sync.Mutex
	ln           net.Listener
	conns        []net.Conn
	closed       bool
}

func (p *pgProxy) open(t *testing.T) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", p.addr)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.ln, p.closed = ln, false
	p.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", p.target)
			if err != nil {
				_ = c.Close()
				continue
			}
			p.mu.Lock()
			if p.closed {
				// close() ran while this connection was being dialled: it must not outlive it.
				p.mu.Unlock()
				_ = c.Close()
				_ = up.Close()
				continue
			}
			p.conns = append(p.conns, c, up)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
			go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
		}
	}()
}

func (p *pgProxy) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.ln != nil {
		_ = p.ln.Close()
	}
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

// 001-AC27: a pod started during a PostgreSQL outage becomes healthy once the database is back, without a
// restart; a later outage fails /healthz (readiness) but never /livez (liveness).
func TestRunWaitsForDatabase(t *testing.T) {
	u, err := url.Parse(testpg.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	proxy := &pgProxy{addr: freeAddr(t), target: u.Host}
	defer proxy.close()
	u.Host = proxy.addr
	cfg := cfgFor(t, config.RoleAll, u.String())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	core, logs := observer.New(zap.WarnLevel)
	go func() { done <- Run(ctx, cfg, zap.New(core), server.BuildInfo{}, fstest.MapFS{}) }()

	deadline := time.Now().Add(10 * time.Second)
	for logs.FilterMessage("db_unreachable").Len() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no db_unreachable retries while the database was down")
		}
		select {
		case err := <-done:
			t.Fatalf("Run gave up with the database down: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	proxy.open(t)
	waitFor(t, "http://"+cfg.HTTPAddr+"/healthz", http.StatusOK)
	waitFor(t, "http://"+cfg.MetricsAddr+"/livez", http.StatusOK)

	proxy.close()
	waitFor(t, "http://"+cfg.MetricsAddr+"/healthz", http.StatusServiceUnavailable)
	if code, _ := getStatus("http://" + cfg.MetricsAddr + "/livez"); code != http.StatusOK {
		t.Fatalf("livez with the database down: %d", code)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v", err)
	}
}
