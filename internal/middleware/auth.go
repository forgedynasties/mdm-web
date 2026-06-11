package middleware

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"time"

	"mdm/internal/ratelimit"
)

// apiAuthFailures throttles repeated bad-key attempts per source IP. The
// constant-time key comparison already makes single guesses ~free to the server,
// but without a cap a single host could grind keys indefinitely (F-11). Failures
// expire after the window; successful requests are never counted.
var (
	apiAuthFailures  = ratelimit.New(time.Minute)
	apiMaxFailPerMin = 30
)

func APIKeyAuth(apiKey, errorMessage string, next http.Handler) http.Handler {
	key := []byte(apiKey)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ratelimit.ClientIP(r)
		if n, retry := apiAuthFailures.Count(ip); n >= apiMaxFailPerMin {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		provided := []byte(r.Header.Get("X-API-Key"))
		if subtle.ConstantTimeCompare(provided, key) != 1 {
			apiAuthFailures.Hit(ip)
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
