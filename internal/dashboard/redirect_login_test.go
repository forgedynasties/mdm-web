package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

// A signed-out answer that a person ends up looking at (a framed page, an in-app
// browser) must take them to the login page, not show the word "Unauthorized".
func TestRedirectLoginBodyForPeople(t *testing.T) {
	r := httptest.NewRequest("GET", "/commands", nil)
	r.Header.Set("Sec-Fetch-Dest", "iframe")
	w := httptest.NewRecorder()
	redirectLogin(w, r)
	body := w.Body.String()
	if w.Code != http.StatusUnauthorized || w.Header().Get("X-Auth-Required") != "1" {
		t.Fatalf("scripts need 401 + X-Auth-Required, got %d %v", w.Code, w.Header())
	}
	if !strings.Contains(body, "location.replace('/login')") || !strings.Contains(body, `url=/login`) {
		t.Fatalf("body does not send the page to /login: %q", body)
	}
	if strings.TrimSpace(body) == "Unauthorized" {
		t.Fatal("body is the bare word Unauthorized")
	}
}
