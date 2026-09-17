package db

import (
	"encoding/json"
	"math"
	"testing"
)

func TestJSONScalar(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`"wlan0"`, "wlan0"}, // a string loses its quotes: readers never see JSON
		{`true`, "true"},     // booleans and numbers render as themselves
		{`1`, "1"},
		{`-57`, "-57"},
		{`31.5`, "31.5"},
		{``, ""}, // absent: the device never reported it
		{`null`, "null"},
	}
	for _, c := range cases {
		if got := jsonScalar(json.RawMessage(c.in)); got != c.want {
			t.Errorf("jsonScalar(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestScaledInt covers the scaling and, more importantly, the cases that must produce
// NULL. A stored NULL means "not measured", which is a different claim from zero — a
// device with no charger is not a device reading 0 °C.
func TestScaledInt(t *testing.T) {
	deref := func(p *int16) any {
		if p == nil {
			return nil
		}
		return *p
	}
	cases := []struct {
		name  string
		in    string
		scale float64
		want  any
	}{
		{"temperature to deci-degrees", `31.5`, 10, int16(315)},
		{"negative temperature", `-5.2`, 10, int16(-52)},
		{"rssi unscaled", `-57`, 1, int16(-57)},
		{"storage to deci-GB", `12.45`, 10, int16(124)},
		{"absent stays null", ``, 10, nil},
		{"non-numeric stays null", `"hot"`, 10, nil},
		{"null stays null", `null`, 10, nil},
		{"over smallint range stays null", `99999`, 10, nil},
		{"under smallint range stays null", `-99999`, 10, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deref(scaledInt(json.RawMessage(c.in), c.scale)); got != c.want {
				t.Errorf("scaledInt(%q, %v) = %v, want %v", c.in, c.scale, got, c.want)
			}
		})
	}
}

// TestRamUsedTotal keeps the two numbers the CSV prints, rather than a percentage
// computed early. Storing a rounded percentage is what made the chart say 55% where
// the export's own columns give 55.3.
func TestRamUsedTotal(t *testing.T) {
	deref := func(p *int32) any {
		if p == nil {
			return nil
		}
		return *p
	}
	cases := []struct {
		name              string
		in                string
		wantUsed, wantTot any
	}{
		{"both numbers", `{"used":2007,"total":3630,"available":1623}`, int32(2007), int32(3630)},
		{"absent stays null", ``, nil, nil},
		{"missing total is not half a reading", `{"used":10}`, nil, nil},
		{"missing used is not half a reading", `{"total":3630}`, nil, nil},
		{"wrong shape stays null", `1234`, nil, nil},
		{"null stays null", `null`, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, tot := ramUsedTotal(json.RawMessage(c.in))
			if deref(u) != c.wantUsed || deref(tot) != c.wantTot {
				t.Errorf("ramUsedTotal(%q) = (%v,%v), want (%v,%v)", c.in, deref(u), deref(tot), c.wantUsed, c.wantTot)
			}
		})
	}
}

// TestJSONFloat covers the value that reaches the chart. It must be the number the
// device sent, unrounded, or the chart and the CSV quote different figures for one
// reading.
func TestJSONFloat(t *testing.T) {
	deref := func(p *float64) any {
		if p == nil {
			return nil
		}
		return *p
	}
	cases := []struct {
		in   string
		want any
	}{
		{`31.4`, 31.4},
		{`34.400001525878906`, 34.400001525878906}, // a Java float32 widened through json
		{`-5.5`, -5.5},
		{``, nil},
		{`null`, nil},
		{`"hot"`, nil},
	}
	for _, c := range cases {
		if got := deref(jsonFloat(json.RawMessage(c.in))); got != c.want {
			t.Errorf("jsonFloat(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestStateKeysAreNotSampled keeps the two shapes from overlapping. A field belongs in
// exactly one of them: stored as a transition because it holds, or as a number because
// it moves. Writing a field to both would double-count it and, worse, leave two
// answers to the same question.
func TestStateKeysAreNotSampled(t *testing.T) {
	sampled := map[string]bool{
		"battery_temp_c":  true,
		"wifi_rssi":       true,
		"ram_usage_mb":    true,
		"storage_free_gb": true,
	}
	for _, k := range stateKeys {
		if sampled[k] {
			t.Errorf("%q is both a state key and a sampled number; it must be one or the other", k)
		}
	}
}

// TestJSONInt32 covers uptime's column. The range guard matters: a device reporting
// something absurd must land as NULL rather than overflow into a plausible-looking
// small number.
func TestJSONInt32(t *testing.T) {
	deref := func(p *int32) any {
		if p == nil {
			return nil
		}
		return *p
	}
	cases := []struct {
		in   string
		want any
	}{
		{`523104`, int32(523104)},
		{`0`, int32(0)},
		{`2147483647`, int32(2147483647)},
		{`2147483648`, nil}, // over int32: NULL, not a wrapped value
		{`-2147483649`, nil},
		{``, nil},
		{`null`, nil},
		{`"up"`, nil},
	}
	for _, c := range cases {
		if got := deref(jsonInt32(json.RawMessage(c.in))); got != c.want {
			t.Errorf("jsonInt32(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestLocationValueRoundTrip: what is written must read back as the same fix, since
// the stored text is what the next deadband comparison is made against.
func TestLocationValueRoundTrip(t *testing.T) {
	for _, c := range []struct{ lat, lon float64 }{
		{31.520370, 74.358749},
		{-33.865143, 151.209900},
		{0, 0},
	} {
		lat, lon, ok := parseLocationValue(formatLocationValue(c.lat, c.lon))
		if !ok || math.Abs(lat-c.lat) > 1e-6 || math.Abs(lon-c.lon) > 1e-6 {
			t.Errorf("round trip of %v,%v gave %v,%v (ok=%v)", c.lat, c.lon, lat, lon, ok)
		}
	}
}

// TestParseLocationValueRejectsGarbage: a value that is not a fix must not be read as
// one. Reporting ok on garbage would make the deadband compare against 0,0 — the Gulf
// of Guinea — and every real position would then look like a 5,000 km move.
func TestParseLocationValueRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "31.5", "31.5,", ",74.3", "a,b", "null"} {
		if _, _, ok := parseLocationValue(s); ok {
			t.Errorf("parseLocationValue(%q) accepted a non-fix", s)
		}
	}
}

// TestMetersBetweenAgainstDeadband pins the two decisions the deadband rests on: the
// jitter actually seen on the fleet stays inside it, and a real move out of the
// building does not. Distances are checked against the measured worst case (5.9 m)
// and a move a person would care about.
func TestMetersBetweenAgainstDeadband(t *testing.T) {
	const lat, lon = 31.520370, 74.358749
	// ~5.9 m north — the largest apparent jump measured across 10,338 located reports.
	if d := metersBetween(lat, lon, lat+5.9/111320, lon); d > locationDeadbandM {
		t.Errorf("measured worst-case jitter (%.1f m) falls outside the deadband", d)
	}
	// ~250 m east: a device that genuinely left the counter.
	east := lon + 250/(111320*math.Cos(lat*math.Pi/180))
	if d := metersBetween(lat, lon, lat, east); d <= locationDeadbandM {
		t.Errorf("a 250 m move measured as %.1f m, inside the deadband", d)
	}
	// Symmetric, and zero for the same point.
	if metersBetween(lat, lon, lat, lon) != 0 {
		t.Error("a point is not zero metres from itself")
	}
}
