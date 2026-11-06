package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// What a valid key's request logs passes whole, or a crash loses its attribution; what anyone can trigger stays capped.
func TestSampling(t *testing.T) {
	const n = 1000
	lines := func(msg string) int {
		core, logs := observer.New(zapcore.InfoLevel)
		log := zap.New(sampledExcept(core, &zap.SamplingConfig{Initial: 100, Thereafter: 100})).With(zap.String("f", "v"))
		for range n {
			log.Info(msg)
		}
		// With must reach whichever core writes the line.
		for _, e := range logs.All() {
			if e.ContextMap()["f"] != "v" {
				t.Fatalf("%s: fields %v", msg, e.ContextMap())
			}
		}
		return logs.Len()
	}
	exempt := []string{"mcp_request", "mcp_tool_call", "mcp_handler_panic", "mcp_dispatch_failed", "mcp_dispatch_unexpected_status"}
	if len(unsampled) != len(exempt) {
		t.Fatalf("%d exempt messages, want the %d named here", len(unsampled), len(exempt))
	}
	for _, msg := range exempt {
		if got := lines(msg); got != n {
			t.Errorf("%s: %d of %d lines", msg, got, n)
		}
	}
	for _, msg := range []string{"login_rejected", "oidc_unavailable", "healthz_db_unreachable", "session_lookup_failed", "mcp_key_lookup_failed", "mcp_method_refused", "other"} {
		// None at all would be a core dropping them, not a sampler.
		if got := lines(msg); got == 0 || got >= n/2 {
			t.Errorf("%s not capped, or lost: %d of %d lines", msg, got, n)
		}
	}
}

// A renamed line would fall back under the sampler with nothing to say so.
func TestUnsampledNamesExist(t *testing.T) {
	src, err := os.ReadFile("../../pkg/mcpserver/mcpserver.go")
	if err != nil {
		t.Fatal(err)
	}
	for msg := range unsampled {
		if !strings.Contains(string(src), fmt.Sprintf("%q", msg)) {
			t.Errorf("%s is logged nowhere", msg)
		}
	}
}

func TestLoggerWiring(t *testing.T) {
	log, err := newLogger("info", "json")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := log.Core().(exemptCore); !ok {
		t.Fatalf("production logger: %T", log.Core())
	}
	// Development has no sampler to exempt anything from.
	log, err = newLogger("info", "console")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := log.Core().(exemptCore); ok {
		t.Fatal("development logger sampled")
	}
}
