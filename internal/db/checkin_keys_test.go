package db

import (
	"strings"
	"testing"
)

// TestCheckinStripKeysKeepReadKeys guards the keys that other features read back out
// of checkins.extra. Stripping one saves bytes and silently breaks a chart, an alert
// rule or a daily rollup — the failure is a flat line, not an error, so it can sit
// unnoticed for weeks. If a key here genuinely has to go, move its readers first and
// then delete its line, on purpose.
func TestCheckinStripKeysKeepReadKeys(t *testing.T) {
	readFromHistory := map[string]string{
		"battery_temp_c": "device chart temp series, live SSE payload, CSV column, temp_elevated rule, daily temp_max rollup",
		"ram_usage_mb":   "device chart RAM series, live SSE payload, CSV columns, memory_low rule, daily ram_pct_peak rollup",
		"charging":       "charge sessions and charging_frac in the daily rollup",
		"wlc_status":     "pad minutes, guest-pad fraction and wlc drain in the daily rollup",
		"screen_on":      "standby minutes in the daily rollup",
	}
	for key, why := range readFromHistory {
		if strings.Contains(checkinStripKeys, "'"+key+"'") {
			t.Errorf("checkinStripKeys strips %q, which is read from history: %s", key, why)
		}
	}
}

// TestCheckinKeyListsDoNotOverlap keeps the two lists meaning distinct things. A
// stripped key is never stored, so naming it volatile as well is dead weight that
// suggests it is still being compared.
func TestCheckinKeyListsDoNotOverlap(t *testing.T) {
	for _, key := range []string{"crash_events", "wifi_scan", "charger_voltage_mv", "mic_gain", "location_address"} {
		if !strings.Contains(checkinStripKeys, "'"+key+"'") {
			t.Errorf("expected %q in checkinStripKeys", key)
		}
		if strings.Contains(checkinVolatileKeys, "'"+key+"'") {
			t.Errorf("%q is stripped, so listing it in checkinVolatileKeys is dead weight", key)
		}
	}
}
