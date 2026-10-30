package dashboard

import (
	"net/http/httptest"
	"testing"
)

func TestDockShows(t *testing.T) {
	for _, c := range []struct {
		role, key string
		want      bool
	}{
		{"field", "myvisits", true}, {"field", "today", true}, {"field", "enroll", true}, {"field", "fleet", false},
		{"admin", "enroll", false}, {"admin", "myvisits", false}, {"admin", "setup", true}, {"admin", "fleet", true},
		{"fleet_admin", "setup", false},
	} {
		if got := dockShows(c.role, c.key); got != c.want {
			t.Errorf("dockShows(%q, %q) = %v, want %v", c.role, c.key, got, c.want)
		}
	}
}

// A curated role's refused fetch must not follow a redirect home: the alerts popover
// put the whole My visits page inside a device page that way.
func TestIsNavigation(t *testing.T) {
	r := func(h map[string]string) bool {
		req := httptest.NewRequest("GET", "/alerts/recent", nil)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		return isNavigation(req)
	}
	if !r(nil) || !r(map[string]string{"Sec-Fetch-Mode": "navigate"}) || !r(map[string]string{"HX-Request": "true", "HX-Boosted": "true"}) {
		t.Error("page loads should count as navigation")
	}
	if r(map[string]string{"HX-Request": "true"}) || r(map[string]string{"Sec-Fetch-Mode": "cors"}) || r(map[string]string{"Sec-Fetch-Mode": "no-cors"}) {
		t.Error("fragments and fetches should not")
	}
}
