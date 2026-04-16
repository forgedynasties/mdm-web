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

func DeviceAPIKeyAuth(apiKey string, next http.Handler) http.Handler {
	return APIKeyAuth(apiKey, `{"error":"invalid device api key"}`, next)
}

func AdminAPIKeyAuth(apiKey string, next http.Handler) http.Handler {
	return APIKeyAuth(apiKey, `{"error":"invalid admin api key"}`, next)
}
