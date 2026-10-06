package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeviceAuthAcceptsEitherSharedKey(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	none := func(context.Context, string) (string, bool) { return "", false }
	h := DeviceAuth([]string{"public-default", "enroll-secret-0123456789abcdef0123456789", ""}, none, ok)
	for key, want := range map[string]int{
		"public-default": http.StatusNoContent,
		"enroll-secret-0123456789abcdef0123456789": http.StatusNoContent,
		"":      http.StatusUnauthorized,
		"wrong": http.StatusUnauthorized,
	} {
		req := httptest.NewRequest("POST", "/api/v1/checkin", nil)
		req.RemoteAddr = "192.0.2.1:1234"
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("key %q: got %d, want %d", key, rec.Code, want)
		}
	}
}
