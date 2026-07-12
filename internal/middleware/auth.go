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
//
// KNOWN LIMITATION — device identity is NOT authenticated, only the fleet is.
// DEVICE_API_KEY is a single shared secret baked into every device image, so this
// middleware proves only "some device in the fleet", not "device X". Every device
// endpoint takes the acting device from a client-supplied serial with no binding to
// the caller, so any holder of the key can post telemetry as another serial or open
// ws?serial=<victim> and receive that device's pushed commands. The command-ack and
// logcat paths ARE defended (they verify the command/request targets the calling
// device), but the read side is not. Closing this needs a per-device credential
// (enrollment token or client cert issued at provisioning) from which the acting
// device is derived, ignoring any client-supplied serial — a change that also spans
// the client/ app. Until then, treat device-reported identity as untrusted.
func DeviceAPIKeyAuth(apiKey string, next http.Handler) http.Handler {
	return APIKeyAuth(apiKey, `{"error":"unauthorized"}`, next)
}

func AdminAPIKeyAuth(apiKey string, next http.Handler) http.Handler {
	return APIKeyAuth(apiKey, `{"error":"unauthorized"}`, next)
}
