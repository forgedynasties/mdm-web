package db

import (
	"encoding/json"
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

func TestRamPct(t *testing.T) {
	deref := func(p *int16) any {
		if p == nil {
			return nil
		}
		return *p
	}
	cases := []struct {
		name string
		in   string
		want any
	}{
		{"used over total", `{"used":2007,"total":3630,"available":1623}`, int16(55)},
		{"absent stays null", ``, nil},
		{"zero total cannot divide", `{"used":10,"total":0}`, nil},
		{"missing total stays null", `{"used":10}`, nil},
		{"wrong shape stays null", `1234`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deref(ramPct(json.RawMessage(c.in))); got != c.want {
				t.Errorf("ramPct(%q) = %v, want %v", c.in, got, c.want)
			}
		})
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
