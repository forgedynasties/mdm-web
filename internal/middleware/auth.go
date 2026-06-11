package middleware

import (
	"crypto/subtle"
	"net/http"
)

func APIKeyAuth(apiKey, errorMessage string, next http.Handler) http.Handler {
	key := []byte(apiKey)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := []byte(r.Header.Get("X-API-Key"))
		if subtle.ConstantTimeCompare(provided, key) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(errorMessage))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Both key classes return an identical "unauthorized" body so an unauthenticated
// caller cannot distinguish the admin key from the device key (F-07). The class
// is still differentiated at the call site for routing, just not in the response.
func DeviceAPIKeyAuth(apiKey string, next http.Handler) http.Handler {
	return APIKeyAuth(apiKey, `{"error":"unauthorized"}`, next)
}

func AdminAPIKeyAuth(apiKey string, next http.Handler) http.Handler {
	return APIKeyAuth(apiKey, `{"error":"unauthorized"}`, next)
}
