package safehttp

import (
	"net"
	"testing"
)

func TestBlockedRangesFromTheAudit(t *testing.T) {
	for addr, want := range map[string]bool{
		"100.113.189.96":       true,  // tailnet / CGNAT
		"0.1.2.3":              true,  // "this network"
		"198.18.0.1":           true,  // benchmarking
		"240.0.0.1":            true,  // reserved
		"64:ff9b::a9fe:a9fe":   true,  // NAT64 to 169.254.169.254 (metadata)
		"2002:a00:1::1":        true,  // 6to4 to 10.0.0.1
		"64:ff9b::808:808":     false, // NAT64 to 8.8.8.8: public
		"8.8.8.8":              false,
		"2606:4700:4700::1111": false,
	} {
		if got := isBlockedIP(net.ParseIP(addr)); got != want {
			t.Errorf("isBlockedIP(%s) = %v, want %v", addr, got, want)
		}
	}
}
