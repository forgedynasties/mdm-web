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
	"syscall"
	"time"
)

// maxRedirects caps redirect chains. Each hop is re-dialed through the same Control
// hook, so a redirect to a blocked IP is refused at dial time regardless of this cap.
const maxRedirects = 5

// isBlockedIP reports whether dialing ip would reach a non-public destination we must
// never let an operator-supplied URL target (cloud metadata, localhost, RFC1918, etc.).
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true // unresolved / unparseable — fail closed
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
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
