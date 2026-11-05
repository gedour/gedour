package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/gedour/gedour/pkg/netguard"
)

// A connection the sender's transport dialed but never used waits in its pool. If the fake's read timeouts
// fire first, a send can pick the connection as it closes: a reset POST, which Go never retries.
func TestServerOutlastsSenderConnections(t *testing.T) {
	g, err := netguard.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	idle := g.Client(netguard.Options{}).Transport.(*http.Transport).IdleConnTimeout
	srv := newServer("")
	for name, d := range map[string]time.Duration{
		"ReadHeaderTimeout": srv.ReadHeaderTimeout,
		"ReadTimeout":       srv.ReadTimeout,
		"IdleTimeout":       srv.IdleTimeout,
	} {
		if d != 0 && d <= idle {
			t.Errorf("%s %s, want more than the sender's %s idle timeout", name, d, idle)
		}
	}
}
