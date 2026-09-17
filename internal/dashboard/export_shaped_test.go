package dashboard

import (
	"encoding/json"
	"testing"

	"mdm/internal/db"
)

func p16(v int16) *int16    { return &v }
func p32(v int32) *int32    { return &v }
func pf(v float64) *float64 { return &v }

// TestShapedExtraProducesIdenticalCSVCells is the test this migration rests on. The
// export can now read from either the check-in snapshot or the shaped tables, and the
// only thing that makes that safe is the two producing the same characters in the file.
//
// So it takes a real snapshot, decomposes it the way the writer does — numbers into a
// sample, states into events — rebuilds an extra from those pieces, and runs BOTH
// through the same column formatters the handler uses, asserting cell by cell.
func TestShapedExtraProducesIdenticalCSVCells(t *testing.T) {
	// A snapshot as a device actually sends it: a Java float32 widened through JSON,
	// an SSID carrying Android's own quotes, a coordinate at full precision.
	snapshot := json.RawMessage(`{
		"battery_temp_c": 34.400001525878906,
		"storage_free_gb": 12.452000617980957,
		"wifi_rssi": -57,
		"ram_usage_mb": {"used": 2007, "total": 3630, "available": 1623},
		"charging": true,
		"wlc_status": 2,
		"wifi": "\"AIO-Guest\"",
		"ip_address": "10.32.1.170",
		"timezone": "Asia/Karachi",
		"mic_gain": 84
	}`)

	// The same reading as the shaped tables hold it.
	sample := db.DeviceSample{
		BatteryPct:    p16(77),
		TempC:         pf(34.400001525878906),
		WifiRSSI:      p16(-57),
		RAMUsedMB:     p32(2007),
		RAMTotalMB:    p32(3630),
		StorageFreeGB: pf(12.452000617980957),
	}
	state := map[string]string{
		"charging":   "true",
		"wlc_status": "2",
		"wifi":       `"AIO-Guest"`, // jsonScalar strips JSON's quotes, Android's remain
		"ip_address": "10.32.1.170",
		"timezone":   "Asia/Karachi",
	}
	rebuilt := db.ShapedExtra(sample, state)

	cells := func(extra json.RawMessage) map[string]string {
		return map[string]string{
			"battery_temp_c":  extraFloat(extra, "battery_temp_c"),
			"charging":        extraBoolAsInt(extra, "charging"),
			"wifi":            extraString(extra, "wifi"),
			"ip_address":      extraString(extra, "ip_address"),
			"ram_used_mb":     extraRamField(extra, "used"),
			"ram_total_mb":    extraRamField(extra, "total"),
			"storage_free_gb": extraFloat(extra, "storage_free_gb"),
			"wlc_status":      extraInt(extra, "wlc_status"),
			"timezone":        extraString(extra, "timezone"),
		}
	}
	from, to := cells(snapshot), cells(rebuilt)
	for col, want := range from {
		if got := to[col]; got != want {
			t.Errorf("column %s: shaped gives %q, snapshot gives %q", col, got, want)
		}
	}
	// And the cells must not be vacuously equal because both are empty.
	for col, v := range from {
		if v == "" {
			t.Errorf("column %s came out empty from the snapshot; the test proves nothing about it", col)
		}
	}
}

// TestShapedExtraOmitsUnreportedKeys: a key the device never sent must be absent, not
// present-and-null. The export's formatters treat a missing key and a null differently
// only by luck, and a row of zeroes for a device with no thermistor would read as a
// very cold device rather than as one that does not measure temperature.
func TestShapedExtraOmitsUnreportedKeys(t *testing.T) {
	rebuilt := db.ShapedExtra(db.DeviceSample{BatteryPct: p16(50)}, map[string]string{})
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rebuilt, &m); err != nil {
		t.Fatalf("rebuilt extra is not valid JSON: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("a sample with no readings produced keys: %s", rebuilt)
	}
	for _, col := range []string{"battery_temp_c", "storage_free_gb"} {
		if got := extraFloat(rebuilt, col); got != "" {
			t.Errorf("column %s exported %q for an unreported reading, want empty", col, got)
		}
	}
}

// TestShapedExtraRejectsMalformedState: a state value that is not what its key should
// hold must be dropped rather than written into the rebuilt object, where it would
// produce invalid JSON and blank every column in the row, not just its own.
func TestShapedExtraRejectsMalformedState(t *testing.T) {
	rebuilt := db.ShapedExtra(db.DeviceSample{TempC: pf(30)}, map[string]string{
		"charging":   "sometimes",
		"wlc_status": "on",
	})
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rebuilt, &m); err != nil {
		t.Fatalf("malformed state produced invalid JSON: %v", err)
	}
	for _, k := range []string{"charging", "wlc_status"} {
		if _, ok := m[k]; ok {
			t.Errorf("malformed state was written through as %s", k)
		}
	}
	// The rest of the row still exports.
	if got := extraFloat(rebuilt, "battery_temp_c"); got != "30.0" {
		t.Errorf("temperature = %q, want 30.0 — one bad state field spoiled the row", got)
	}
}
