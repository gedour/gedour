// Package netguard builds HTTP clients that refuse to reach internal addresses (SSRF guard).
//
// The check runs in the dialer, after DNS resolution and on every connection (redirects included), so
// a hostname that resolves or re-resolves (DNS rebinding) to a private address is refused too.
package netguard

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"syscall"
	"time"
)

// ErrBlocked is wrapped by every refused connection or redirect.
var ErrBlocked = errors.New("destination not allowed")

// Ranges netip has no predicate for.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),     // "this network"
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved, broadcast
	// Local-use NAT64 (RFC 8215) is private by definition; Teredo (RFC 4380) tunnels to arbitrary IPv4.
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"),
}

// IPv6 ranges that carry an IPv4 address the packet ends up at: the IPv4 is checked instead.
var (
	nat64     = netip.MustParsePrefix("64:ff9b::/96") // RFC 6052 well-known prefix
	sixToFour = netip.MustParsePrefix("2002::/16")    // RFC 3056
)

func embeddedIPv4(ip netip.Addr) (netip.Addr, bool) {
	b := ip.As16()
	switch {
	case nat64.Contains(ip):
		return netip.AddrFrom4([4]byte(b[12:16])), true
	case sixToFour.Contains(ip):
		return netip.AddrFrom4([4]byte(b[2:6])), true
	}
	return netip.Addr{}, false
}

// Guard decides which addresses a client may connect to.
type Guard struct {
	allow []netip.Prefix
}

// New returns a guard that also allows the given CIDRs (GEDOUR_ALLOW_PRIVATE_NETS).
func New(allow []string) (*Guard, error) {
	g := &Guard{}
	for _, s := range allow {
		p, err := netip.ParsePrefix(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("allowed network %q: %w", s, err)
		}
		g.allow = append(g.allow, p.Masked())
	}
	return g, nil
}

// Allowed reports whether ip is a public unicast address or inside an allowlisted network.
func (g *Guard) Allowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	if slices.ContainsFunc(g.allow, func(p netip.Prefix) bool { return p.Contains(ip) }) {
		return true
	}
	if v4, ok := embeddedIPv4(ip); ok {
		return g.Allowed(v4)
	}
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() ||
		ip.IsMulticast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	return !slices.ContainsFunc(blockedPrefixes, func(p netip.Prefix) bool { return p.Contains(ip) })
}

// control is a net.Dialer.Control: address is the resolved "ip:port" about to be connected.
func (g *Guard) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrBlocked, address)
	}
	if !g.Allowed(ap.Addr()) {
		return fmt.Errorf("%w: %s is a private, loopback or reserved address", ErrBlocked, ap.Addr())
	}
	return nil
}

// Options shape the client.
type Options struct {
	Timeout      time.Duration
	MaxRedirects int
	// HTTPHosts may be reached over plain http (redirect targets included); others need https.
	HTTPHosts []string
	// StripOnCrossHost lists request headers dropped when a redirect leaves the original host.
	StripOnCrossHost []string
	// NoRedirects hands a 3xx back to the caller instead of following it.
	NoRedirects bool
}

// Client returns an HTTP client whose every connection goes through the guard.
func (g *Guard) Client(o Options) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: g.control}
	tr := &http.Transport{
		// No proxy: a proxy would make the dial check see the proxy, not the destination.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   o.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if o.NoRedirects {
				return http.ErrUseLastResponse
			}
			if len(via) > o.MaxRedirects {
				return fmt.Errorf("stopped after %d redirects", o.MaxRedirects)
			}
			if err := CheckScheme(req.URL.Scheme, req.URL.Hostname(), o.HTTPHosts); err != nil {
				return err
			}
			if !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
				for _, h := range o.StripOnCrossHost {
					req.Header.Del(h)
				}
			}
			return nil
		},
	}
}

// CheckScheme allows https, and http for the listed hosts only.
func CheckScheme(scheme, host string, httpHosts []string) error {
	switch {
	case scheme == "https":
		return nil
	case scheme == "http" && slices.Contains(httpHosts, strings.ToLower(host)):
		return nil
	}
	return fmt.Errorf("%w: %s://%s needs https", ErrBlocked, scheme, host)
}
