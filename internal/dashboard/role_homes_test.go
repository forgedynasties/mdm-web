package dashboard

import (
	"strings"
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

// The old Setup addresses redirect for good, query string kept (19 Nov 2026).
func TestOldSetupRedirects(t *testing.T) {
	for _, c := range []struct{ from, to string }{
		{"/visits?view=how", "/setup?view=how"}, {"/visits", "/setup"},
		{"/my-visits?venue=x", "/setup/mine?venue=x"}, {"/my-visits/enroll", "/setup/mine/enroll"},
	} {
		base := "/setup"
		switch {
		case strings.HasPrefix(c.from, "/my-visits/enroll"):
			base = "/setup/mine/enroll"
		case strings.HasPrefix(c.from, "/my-visits"):
			base = "/setup/mine"
		}
		rec := httptest.NewRecorder()
		oldSetupURL(base).ServeHTTP(rec, httptest.NewRequest("GET", c.from, nil))
		if rec.Code != 301 || rec.Header().Get("Location") != c.to {
			t.Errorf("%s: %d %q, want 301 %q", c.from, rec.Code, rec.Header().Get("Location"), c.to)
		}
	}
}

// A curated role's refused fetch must not follow a redirect home: the alerts popover
// put the whole Setup page inside a device page that way.
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
