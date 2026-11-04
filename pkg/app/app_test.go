package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/gedour/gedour/pkg/config"
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
		!strings.Contains(body, "gedour_watches_due 0") {
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

func TestRunErrors(t *testing.T) {
	t.Run("db unreachable", func(t *testing.T) {
		cfg := cfgFor(t, config.RoleAll, "postgres://u:p@127.0.0.1:1/db?connect_timeout=1")
		if err := Run(context.Background(), cfg, zap.NewNop(), server.BuildInfo{}, fstest.MapFS{}); err == nil {
			t.Fatal("want migration error")
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
