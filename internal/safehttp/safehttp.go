// Package safehttp provides an HTTP client hardened against SSRF for the handful of
// places the server fetches an operator- or config-supplied URL (APK metadata, OTA
// package inspect, AI provider base URL, alert webhooks).
//
// The core protection is a dialer Control hook that inspects the *resolved* IP at
// connect time and refuses to dial loopback/private/link-local/unspecified targets.
// Because it runs after DNS resolution on the concrete address — and on every redirect
// hop's dial — it closes the DNS-rebinding / TOCTOU window that a pre-flight
// net.LookupHost check alone would leave open (a name that resolves to a public IP
// during validation and a private IP at dial time).
package safehttp

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// maxRedirects caps redirect chains. Each hop is re-dialed through the same Control
// hook, so a redirect to a blocked IP is refused at dial time regardless of this cap.
const maxRedirects = 5

// allowlist holds operator-configured IPs/CIDRs that are exempt from the private-address
// block — e.g. an internal OTA package host on the LAN (10.x). It only ever WIDENS access
// to explicitly-trusted ranges; everything not listed stays guarded. Empty by default.
var (
	allowMu     sync.RWMutex
	allowedNets []*net.IPNet
)

// SetAllowlist configures the SSRF exemption list from entries that are either a single
// IP ("10.32.1.113") or a CIDR ("10.32.0.0/22"). Invalid/empty entries are ignored. Call
// once at startup; safe to call again to replace the list.
func SetAllowlist(entries []string) {
	var nets []*net.IPNet
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if strings.Contains(e, "/") {
			if _, n, err := net.ParseCIDR(e); err == nil {
				nets = append(nets, n)
			}
			continue
		}
		if ip := net.ParseIP(e); ip != nil {
			if v4 := ip.To4(); v4 != nil {
				ip = v4
			}
			bits := len(ip) * 8
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		}
	}
	allowMu.Lock()
	allowedNets = nets
	allowMu.Unlock()
}

func isAllowed(ip net.IP) bool {
	allowMu.RLock()
	defer allowMu.RUnlock()
	for _, n := range allowedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// isBlockedIP reports whether dialing ip would reach a non-public destination we must
// never let an operator-supplied URL target (cloud metadata, localhost, RFC1918, etc.).
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true // unresolved / unparseable — fail closed
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	} else if v4 := embeddedV4(ip); v4 != nil {
		ip = v4 // NAT64 / 6to4: judge the IPv4 address it reaches
	}
	if isAllowed(ip) {
		return false // operator-trusted internal host (e.g. LAN OTA server)
	}
	for _, n := range extraBlocked {
		if n.Contains(ip) {
			return true
		}
	}
	return ip.IsLoopback() || // 127.0.0.0/8, ::1
		ip.IsPrivate() || // 10/8, 172.16/12, 192.168/16, fc00::/7
		ip.IsLinkLocalUnicast() || // 169.254/16 (incl. 169.254.169.254 metadata), fe80::/10
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() // 0.0.0.0, ::
}

// controlBlockPrivate is the net.Dialer.Control hook: it is called with the concrete
// resolved address just before the socket connects, so it sees the real IP even when
// DNS rebinding tried to swap it after validation.
func controlBlockPrivate(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("safehttp: bad dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if isBlockedIP(ip) {
		return fmt.Errorf("safehttp: refusing to connect to non-public address %s", host)
	}
	return nil
}

// Client returns an *http.Client that blocks SSRF to internal addresses, bounds
// redirects, and applies the given overall timeout. Reuse a returned client; it is
// safe for concurrent use.
func Client(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   controlBlockPrivate,
	}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("safehttp: stopped after %d redirects", maxRedirects)
			}
			return nil
		},
	}
}

// CheckURL validates a URL's shape before it is fetched: it must parse, use http or
// (when requireHTTPS) only https, and carry a host. The dial-time IP block is the
// real SSRF gate; this rejects obviously-wrong schemes (file://, gopher://, …) early
// and with a clear message. It does NOT resolve DNS (that would be a TOCTOU check).
func CheckURL(raw string, requireHTTPS bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if requireHTTPS {
		if scheme != "https" {
			return fmt.Errorf("URL must use https, got %q", u.Scheme)
		}
	} else if scheme != "http" && scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("URL has no host")
	}
	return nil
}

// LimitedReadAll reads from r up to limit bytes, erroring if the body would exceed it.
// Use for responses from operator/config-controlled hosts so an oversized reply can't
// exhaust memory.
func LimitedReadAll(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response exceeds %d-byte limit", limit)
	}
	return b, nil
}

// extraBlocked are non-public ranges the net.IP predicates don't cover (security audit,
// 26 Oct 2026): "this network", carrier-grade NAT (also the tailnet's range), IETF
// protocol assignments, benchmarking, and the reserved class E block.
var extraBlocked = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// embeddedV4 returns the IPv4 address inside a NAT64 (64:ff9b::/96, 64:ff9b:1::/48) or
// 6to4 (2002::/16) IPv6 address, which would otherwise route to a blocked IPv4 host.
func embeddedV4(ip net.IP) net.IP {
	ip16 := ip.To16()
	if ip16 == nil {
		return nil
	}
	switch {
	case ip16[0] == 0x00 && ip16[1] == 0x64 && ip16[2] == 0xff && ip16[3] == 0x9b:
		return net.IPv4(ip16[12], ip16[13], ip16[14], ip16[15]).To4()
	case ip16[0] == 0x20 && ip16[1] == 0x02:
		return net.IPv4(ip16[2], ip16[3], ip16[4], ip16[5]).To4()
	}
	return nil
}
