package middleware

import (
	"context"
	"errors"
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

func TestDeviceAuthMarksTheEnrollKey(t *testing.T) {
	var got bool
	h := DeviceAuth([]string{"public-default", "enroll-secret-0123456789abcdef0123456789"},
		func(context.Context, string) (string, bool) { return "", false },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = EnrollKeyUsed(r) }))
	for key, want := range map[string]bool{"public-default": false, "enroll-secret-0123456789abcdef0123456789": true} {
		got = !want
		req := httptest.NewRequest("POST", "/api/v1/device-key", nil)
		req.RemoteAddr = "192.0.2.9:1234"
		req.Header.Set("X-API-Key", key)
		h.ServeHTTP(httptest.NewRecorder(), req)
		if got != want {
			t.Errorf("key %q: EnrollKeyUsed = %v, want %v", key, got, want)
		}
	}
}

func TestDeviceAuthLookupFailureIsNot401(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for name, c := range map[string]struct {
		err  error
		want int
	}{
		"unknown key": {ErrUnknownKey, http.StatusUnauthorized},
		"db down":     {errors.New("conn closed"), http.StatusServiceUnavailable},
		"found":       {nil, http.StatusNoContent},
	} {
		h := DeviceAuthE([]string{"public-default"}, func(context.Context, string) (string, error) { return "SER1", c.err }, ok)
		req := httptest.NewRequest("POST", "/api/v1/checkin", nil)
		req.RemoteAddr = "192.0.2.9:1234"
		req.Header.Set("X-API-Key", "dvk_whatever")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", name, rec.Code, c.want)
		}
	}
}
