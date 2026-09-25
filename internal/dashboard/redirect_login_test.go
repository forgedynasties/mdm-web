package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// redirectLogin once called itself for a plain navigation (a replace-all of the old
// http.Redirect line caught its own body), so any logged-out page load overflowed the
// stack and took the whole server down. Each caller shape must get its answer.
func TestRedirectLogin(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		code    int
		check   func(http.Header) bool
	}{
		{"navigation", map[string]string{"Sec-Fetch-Dest": "document"}, http.StatusFound,
			func(h http.Header) bool { return h.Get("Location") == "/login" }},
		{"no fetch metadata", nil, http.StatusFound,
			func(h http.Header) bool { return h.Get("Location") == "/login" }},
		{"htmx", map[string]string{"HX-Request": "true"}, http.StatusOK,
			func(h http.Header) bool { return h.Get("HX-Redirect") == "/login" }},
		{"fetch", map[string]string{"Sec-Fetch-Dest": "empty"}, http.StatusUnauthorized,
			func(h http.Header) bool { return h.Get("X-Auth-Required") == "1" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/server", nil)
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			redirectLogin(w, r)
			if w.Code != c.code || !c.check(w.Header()) {
				t.Fatalf("got %d %v, want %d", w.Code, w.Header(), c.code)
			}
		})
	}
}
