package netguard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestAllowed(t *testing.T) {
	g, err := New([]string{"10.20.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", false},
		{"127.8.9.10", false},
		{"10.9.2.3", false},
		{"172.16.0.1", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false}, // cloud metadata
		{"100.64.0.1", false},
		{"0.0.0.0", false},
		{"224.0.0.1", false},
		{"255.255.255.255", false},
		{"::1", false},
		{"::", false},
		{"fd00::1", false},
		{"fd00:ec2::254", false}, // AWS IPv6 metadata
		{"fe80::1", false},
		{"ff02::1", false},
		{"::ffff:127.0.0.1", false}, // IPv4-mapped loopback
		{"::ffff:10.0.0.1", false},
		{"64:ff9b::a9fe:a9fe", false},                   // NAT64 of 169.254.169.254
		{"64:ff9b::7f00:1", false},                      // NAT64 of 127.0.0.1
		{"64:ff9b::a00:1", false},                       // NAT64 of 10.0.0.1
		{"64:ff9b:1::808:808", false},                   // local-use NAT64 prefix
		{"2002:7f00:1::1", false},                       // 6to4 of 127.0.0.1
		{"2002:a9fe:a9fe::1", false},                    // 6to4 of the metadata address
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", false}, // Teredo
		{"64:ff9b::808:808", true},                      // NAT64 of 8.8.8.8
		{"2002:808:808::1", true},                       // 6to4 of 8.8.8.8
		{"2001:db8::1", true},                           // not Teredo: 2001:db8::/32 is another /32
		{"8.8.8.8", true},
		{"140.82.112.3", true},
		{"2606:4700::1111", true},
		{"10.20.3.4", true}, // allowlisted
		{"::ffff:10.20.3.4", true},
		{"64:ff9b::a14:304", true}, // NAT64 of allowlisted 10.20.3.4
	} {
		if got := g.Allowed(netip.MustParseAddr(tt.ip)); got != tt.want {
			t.Errorf("%s: want %v got %v", tt.ip, tt.want, got)
		}
	}
	if _, err := New([]string{"not-a-cidr"}); err == nil {
		t.Fatal("bad CIDR accepted")
	}
}

func get(t *testing.T, c *http.Client, url string, hdr http.Header) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = hdr
	resp, err := c.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	return resp, err
}

func TestClientDialGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	opts := Options{Timeout: 5 * time.Second, MaxRedirects: 3, HTTPHosts: []string{"127.0.0.1", "localhost", "[::1]", "10.0.0.1", "169.254.169.254", "fd00::1"}}

	strict, _ := New(nil)
	// localhost resolves to loopback: the check runs after DNS, so names do not slip through.
	for _, u := range []string{srv.URL, "http://localhost:1/", "http://[::1]:1/", "http://10.0.0.1:1/", "http://169.254.169.254/latest/meta-data/", "http://[fd00::1]:1/"} {
		if _, err := get(t, strict.Client(opts), u, nil); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: want ErrBlocked, got %v", u, err)
		}
	}
	allowed, _ := New([]string{"127.0.0.0/8"})
	resp, err := get(t, allowed.Client(opts), srv.URL, nil)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("allowlisted: %v", err)
	}
}

func TestClientRedirects(t *testing.T) {
	var gotAuth, gotToken string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotToken = r.Header.Get("Authorization"), r.Header.Get("Private-Token")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	redirect := func(to string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, to, http.StatusFound)
		}))
	}
	g, _ := New([]string{"127.0.0.0/8"})
	opts := Options{Timeout: 5 * time.Second, MaxRedirects: 3, HTTPHosts: []string{"127.0.0.1"}, StripOnCrossHost: []string{"Authorization", "Private-Token"}}
	hdr := func() http.Header { return http.Header{"Authorization": {"Bearer s3cret"}, "Private-Token": {"glpat"}} }

	// A redirect to a private address is refused at dial time, even from an allowed origin.
	toMeta := redirect("http://169.254.169.254/latest/meta-data/")
	defer toMeta.Close()
	g2, _ := New([]string{"127.0.0.0/8"})
	if _, err := get(t, g2.Client(Options{Timeout: 5 * time.Second, MaxRedirects: 3, HTTPHosts: []string{"127.0.0.1", "169.254.169.254"}}), toMeta.URL, nil); !errors.Is(err, ErrBlocked) {
		t.Fatalf("redirect to metadata: %v", err)
	}
	// A plain-http redirect target that is not allowlisted is refused.
	toHTTP := redirect("http://example.invalid/")
	defer toHTTP.Close()
	if _, err := get(t, g.Client(opts), toHTTP.URL, nil); !errors.Is(err, ErrBlocked) {
		t.Fatalf("http downgrade: %v", err)
	}
	// Cross-host (other port = other host) drops credentials; same host keeps them.
	cross := redirect(target.URL + "/x")
	defer cross.Close()
	if _, err := get(t, g.Client(opts), cross.URL, hdr()); err != nil || gotAuth != "" || gotToken != "" {
		t.Fatalf("cross-host kept credentials: %v %q %q", err, gotAuth, gotToken)
	}
	if _, err := get(t, g.Client(opts), target.URL, hdr()); err != nil || gotAuth == "" || gotToken == "" {
		t.Fatalf("direct lost credentials: %v", err)
	}
	loop := httptest.NewServer(nil)
	loop.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, loop.URL, http.StatusFound) })
	defer loop.Close()
	if _, err := get(t, g.Client(opts), loop.URL, nil); err == nil {
		t.Fatal("redirect loop not stopped")
	}
	// 001-AC16: a webhook client hands the 3xx back instead of following it.
	hits := 0
	counted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer counted.Close()
	toCounted := redirect(counted.URL)
	defer toCounted.Close()
	resp, err := get(t, g.Client(Options{Timeout: 5 * time.Second, NoRedirects: true}), toCounted.URL, nil)
	if err != nil || resp.StatusCode != http.StatusFound || hits != 0 {
		t.Fatalf("no-redirect client: %v %v, target hit %d times", resp, err, hits)
	}
}
