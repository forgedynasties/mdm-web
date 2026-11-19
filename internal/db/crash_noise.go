package db

import "strings"

// knownCrashNoise are crash signatures that are not faults of ours or of the app a device
// runs, and that only bury the real ones. Matched on the summary's start.
//
// Google Play services' fused-location code asks the system for a location provider's
// properties; on the HK1 box's Android 13 image (build TQ3C.230805.001.B2) the system's
// location service answers with a NullPointerException ("LocationProviderManager
// .getProperties() on a null object reference"). Only Google's own process restarts: seen
// 19 Nov 2026 on five prod boxes, 40 times in 6 minutes on one. User: hide it. The real
// fix is a newer Play services in the vendor image.
var knownCrashNoise = []string{
	"com.google.android.gms.persistent — java.lang.NullPointerException: Attempt to invoke virtual method 'android.location.provider.ProviderProperties com.android.server.location.provider.LocationProviderManager.getProperties()'",
}

// isKnownCrashNoise reports whether a crash event is one of knownCrashNoise.
func isKnownCrashNoise(summary string) bool {
	for _, p := range knownCrashNoise {
		if strings.HasPrefix(summary, p) {
			return true
		}
	}
	return false
}

// crashNoiseSchema removes the rows of that signature already stored, once, so the crash
// lists and release pages stop showing them. The marker rides on access_seed.
const crashNoiseSchema = `
WITH s AS (INSERT INTO access_seed (k) VALUES ('crash-noise-gms-location-v1') ON CONFLICT DO NOTHING RETURNING k)
DELETE FROM device_events e USING s
WHERE e.kind = 'system_app_crash'
  AND e.summary LIKE 'com.google.android.gms.persistent — java.lang.NullPointerException: Attempt to invoke virtual method ''android.location.provider.ProviderProperties com.android.server.location.provider.LocationProviderManager.getProperties()''%';
`
