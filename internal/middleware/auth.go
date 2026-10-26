package middleware

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
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

type boundSerialKey struct{}

type enrollKeyUsedKey struct{}

// EnrollKeyUsed reports whether the request authenticated with the secret enrollment
// key (any shared key after the first, which is the public one whose value is in the
// client source). Taking over a serial's identity — recovering a lost own key,
// registering a first one — needs it (security audit, 26 Oct 2026): with only the
// public key, anyone who knew a serial could do it.
func EnrollKeyUsed(r *http.Request) bool {
	used, _ := r.Context().Value(enrollKeyUsedKey{}).(bool)
	return used
}

// BoundSerial returns the device serial derived from a per-device credential, when the
// request authenticated with one. Handlers use it to reject client-supplied serials that
// don't match the caller's own identity. Empty for legacy shared-key requests.
func BoundSerial(r *http.Request) string {
	s, _ := r.Context().Value(boundSerialKey{}).(string)
	return s
}

// DeviceAuth authenticates device-API requests with either the legacy shared fleet key
// (unbound — device identity stays client-supplied) or a per-device "dvk_..." key issued
// at enrollment. Per-device keys are resolved via lookup (SHA-256 hex of the presented
// key → serial); on success the bound serial is attached to the request context so
// handlers can enforce identity. Same rate limiting and uniform error body as APIKeyAuth.
//
// sharedKeys[0] is the public key; any later one is a secret enrollment key, and a
// request that used one is marked for EnrollKeyUsed.
func DeviceAuth(sharedKeys []string, lookup func(ctx context.Context, keyHashHex string) (string, bool), next http.Handler) http.Handler {
	type sharedKey struct {
		key    []byte
		enroll bool
	}
	var shared []sharedKey
	for i, k := range sharedKeys {
		if k != "" {
			shared = append(shared, sharedKey{[]byte(k), i > 0})
		}
	}
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
		for _, k := range shared {
			if subtle.ConstantTimeCompare(provided, k.key) == 1 {
				if k.enroll {
					r = r.WithContext(context.WithValue(r.Context(), enrollKeyUsedKey{}, true))
				}
				next.ServeHTTP(w, r)
				return
			}
		}
		if len(provided) > 0 {
			sum := sha256.Sum256(provided)
			if serial, ok := lookup(r.Context(), hex.EncodeToString(sum[:])); ok {
				next.ServeHTTP(w, r.WithContext(
					context.WithValue(r.Context(), boundSerialKey{}, serial)))
				return
			}
		}
		apiAuthFailures.Hit(ip)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	})
}

func AdminAPIKeyAuth(apiKey string, next http.Handler) http.Handler {
	return APIKeyAuth(apiKey, `{"error":"unauthorized"}`, next)
}
