package server

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/gedour/gedour/pkg/auth"
	"github.com/gedour/gedour/pkg/config"
	"github.com/gedour/gedour/pkg/secret"
	"github.com/gedour/gedour/pkg/watch"
)

var update = flag.Bool("update", false, "rewrite api/openapi.json from the handlers (make openapi)")

const (
	openAPIFile = "../../api/openapi.json"
	testKey     = "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc="
)

func testWatches(t *testing.T, pool *pgxpool.Pool) *watch.Service {
	t.Helper()
	box, err := secret.New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	return watch.New(pool, zaptest.NewLogger(t), box, []string{"127.0.0.1"})
}

// renderOpenAPI is the document the handlers serve, pretty printed, with a fixed version so the
// committed file does not change on every release.
func renderOpenAPI(t *testing.T) []byte {
	t.Helper()
	authn, err := auth.New(config.Config{OIDCIssuer: "http://127.0.0.1:1", OIDCClientID: "c", OIDCClientSecret: "s",
		PublicURL: "https://app.test", SessionTTL: time.Hour, SessionIdleTimeout: time.Hour}, nil, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	h := New(zap.NewNop(), BuildInfo{Version: "v0.0.0"}, pingOK, fstest.MapFS{}, authn, nil, testWatches(t, nil))
	rec := get(t, h, "/api/openapi.json")
	if rec.Code != http.StatusOK {
		t.Fatalf("openapi: %d", rec.Code)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, rec.Body.Bytes(), "", "  "); err != nil {
		t.Fatal(err)
	}
	out.WriteByte('\n')
	return out.Bytes()
}

// 001-AC30: the committed api/openapi.json is what the handlers produce; `make openapi` rewrites it.
func TestOpenAPIDrift(t *testing.T) {
	got := renderOpenAPI(t)
	if !bytes.Equal(got, renderOpenAPI(t)) {
		t.Fatal("the OpenAPI document is not stable across renders")
	}
	if *update {
		//nolint:gosec // a committed source file, world-readable like the rest of the tree
		if err := os.WriteFile(openAPIFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(openAPIFile)
	if err != nil {
		t.Fatalf("%v: run `make openapi`", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("api/openapi.json is stale: run `make openapi` and commit the result")
	}
}
