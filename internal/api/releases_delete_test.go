package api

import "testing"

func TestGlobMatch(t *testing.T) {
	for _, c := range []struct {
		pat, s string
		want   bool
	}{
		{"v2.2.001-aio-home-*", "v2.2.001-aio-home-22-adb", true},
		{"v2.2.001-aio-home-*", "v2.2.001-aio-home-", true},
		{"v2.2.001-aio-home-*", "v2.2.002-aio-home-1", false},
		{"*-adb", "v2.1.040-adb", true},
		{"*-adb", "v2.1.040", false},
		{"v2.1.0*-aio-glance-*", "v2.1.025-aio-glance-23", true},
		{"v2.1.0*-aio-glance-*", "v2.1.025-aio-home-23", false},
		{"v2.1.040-adb", "v2.1.040-adb", true},
		{"v2.1.040-adb", "v2.1.040-adb2", false},
	} {
		if got := globMatch(c.pat, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pat, c.s, got, c.want)
		}
	}
}
