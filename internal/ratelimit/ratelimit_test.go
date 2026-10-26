package ratelimit

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	for _, c := range []struct {
		name, remote, realIP, xff, want string
	}{
		{"from our proxy: X-Real-IP", "172.18.0.5:41000", "203.0.113.7", "6.6.6.6, 203.0.113.7, 172.31.4.9", "203.0.113.7"},
		{"forged X-Forwarded-For is never read", "172.18.0.5:41000", "", "6.6.6.6", "172.18.0.5"},
		{"direct caller can't claim an address", "198.51.100.4:5555", "6.6.6.6", "6.6.6.6", "198.51.100.4"},
		{"junk X-Real-IP from the proxy is ignored", "127.0.0.1:9", "not-an-ip", "", "127.0.0.1"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.realIP != "" {
			r.Header.Set("X-Real-IP", c.realIP)
		}
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := ClientIP(r); got != c.want {
			t.Errorf("%s: ClientIP = %q, want %q", c.name, got, c.want)
		}
	}
}
