package middleware

import "net/http"

// contentSecurityPolicy constrains where the dashboard may load resources from.
// The app relies heavily on inline <script>/<style> and inline event handlers
// (hx-on, onclick, style="..."), so 'unsafe-inline' is required for script/style
// until those are refactored to nonces; everything else is locked to same-origin.
// frame-ancestors 'none' blocks click-jacking (e.g. framing /login), object-src
// 'none' blocks plugin vectors, and base-uri/form-action 'self' prevent base-tag
// and form-action hijacking. All third-party assets are self-hosted (GB-07/F-10),
// so no external origins are allowed.
const contentSecurityPolicy = "default-src 'self'; " +
	// The overview fleet map uses the Google Maps JavaScript API, which loads its
	// loader/worker scripts from maps.googleapis.com + maps.gstatic.com, tiles and
	// sprites from *.googleapis.com/*.gstatic.com (images), fonts from
	// fonts.gstatic.com, and telemetry via connect to maps.googleapis.com.
	"script-src 'self' 'unsafe-inline' https://maps.googleapis.com https://maps.gstatic.com; " +
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
	"img-src 'self' data: blob: https://*.googleapis.com https://*.gstatic.com; " +
	"font-src 'self' https://fonts.gstatic.com; " +
	"connect-src 'self' https://maps.googleapis.com; " +
	// Device-page location map is a Google Maps Embed iframe (browser-facing,
	// referrer-restricted key). Only the Google Maps host is allowed to be framed;
	// everything else stays same-origin.
	"frame-src https://www.google.com; " +
	"frame-ancestors 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"object-src 'none'"

// SecurityHeaders wraps the whole mux and attaches the standard security header
// set to every response (F-03 HSTS, F-04 CSP/X-Frame-Options/Referrer-Policy/
// Permissions-Policy, F-12 Cache-Control). Handlers that need a different
// Cache-Control (the static and splash file servers set a long immutable cache)
// override it after this runs, so caching of immutable assets is preserved while
// dynamic/authenticated responses default to no-store.
//
// WebSocket upgrade requests are passed through untouched so the upgrade
// handshake response is not perturbed.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "geolocation=(), camera=(), microphone=(), usb=(), payment=()")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		// Default dynamic responses to no-store; the static/splash handlers reset
		// this to a long immutable cache for their fingerprinted assets.
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
