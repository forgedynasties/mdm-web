package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The admin-key routes answer 404 without the key, and run as the admin API with it.
func TestAsAdminAPI(t *testing.T) {
	h := &Handler{adminAPIKey: "k-123"}
	var who string
	next := func(w http.ResponseWriter, r *http.Request) {
		if s, ok := h.currentSession(r); ok {
			who = s.Username + "/" + s.Role
		}
	}
	for _, key := range []string{"", "wrong"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/v1/admin/backend/placements", nil)
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		h.asAdminAPI(next)(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("key %q: %d, want 404", key, rec.Code)
		}
	}
	req := httptest.NewRequest("GET", "/api/v1/admin/backend/placements", nil)
	req.Header.Set("X-API-Key", "k-123")
	h.asAdminAPI(next)(httptest.NewRecorder(), req)
	if who != "admin API/admin" {
		t.Errorf("ran as %q", who)
	}
	if (&Handler{}).asAdminAPI(next); true {
		rec := httptest.NewRecorder()
		r2 := httptest.NewRequest("GET", "/x", nil)
		r2.Header.Set("X-API-Key", "")
		(&Handler{}).asAdminAPI(next)(rec, r2)
		if rec.Code != http.StatusNotFound {
			t.Error("a server with no admin key let a request through")
		}
	}
}
