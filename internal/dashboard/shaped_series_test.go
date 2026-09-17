package dashboard

import (
	"testing"
	"time"

	"mdm/internal/db"
)

func i16(v int16) *int16 { return &v }

// TestMergeShapedSeriesCarriesStateForward is the core of reading transitions back: a
// state is written once and must then apply to every sample until the next event. The
// old shape repeated the value on every row, so this is the behaviour that has to be
// reproduced exactly.
func TestMergeShapedSeriesCarriesStateForward(t *testing.T) {
	t0 := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }

	samples := []db.DeviceSample{
		{At: at(0), BatteryPct: i16(80), TempDeciC: i16(302)},
		{At: at(5), BatteryPct: i16(79), TempDeciC: i16(305)},
		{At: at(10), BatteryPct: i16(81), TempDeciC: i16(311)},
		{At: at(15), BatteryPct: i16(83), TempDeciC: i16(318)},
	}
	events := []db.StateAt{
		{At: at(7), Key: "charging", Value: "true"},
		{At: at(7), Key: "wlc_status", Value: "1"},
		{At: at(12), Key: "wlc_status", Value: "0"},
	}

	got := mergeShapedSeries(samples, events)
	if len(got) != 4 {
		t.Fatalf("got %d points, want 4", len(got))
	}

	// Before the first event: unknown, not a default. Drawing "false" here would claim
	// the device was definitely not charging, which we do not know.
	if got[0].HasCharge || got[1].HasCharge {
		t.Error("charging reported as known before its first event")
	}
	// After the event at +7, both samples that follow carry it.
	if !got[2].HasCharge || !got[2].Charging || !got[3].HasCharge || !got[3].Charging {
		t.Error("charging did not carry forward to later samples")
	}
	// wlc flips to 1 at +7 and back to 0 at +12, so sample +10 is 1 and +15 is 0.
	if !got[2].HasWlc || got[2].WlcStatus != 1 {
		t.Errorf("sample at +10 has wlc %v/%d, want 1", got[2].HasWlc, got[2].WlcStatus)
	}
	if !got[3].HasWlc || got[3].WlcStatus != 0 {
		t.Errorf("sample at +15 has wlc %v/%d, want 0", got[3].HasWlc, got[3].WlcStatus)
	}
	// Numbers come back unscaled.
	if !got[0].HasTemp || got[0].TempC != 30.2 {
		t.Errorf("temp %v/%v, want 30.2", got[0].HasTemp, got[0].TempC)
	}
}

// TestMergeShapedSeriesUsesLeadingEvent covers the left edge. A device that went on
// charge yesterday and has not changed since has no event inside today's window; the
// query supplies the last event before it, and every sample must already carry it.
func TestMergeShapedSeriesUsesLeadingEvent(t *testing.T) {
	t0 := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	samples := []db.DeviceSample{
		{At: t0, BatteryPct: i16(50)},
		{At: t0.Add(time.Minute), BatteryPct: i16(51)},
	}
	events := []db.StateAt{{At: t0.Add(-48 * time.Hour), Key: "charging", Value: "true"}}

	for i, p := range mergeShapedSeries(samples, events) {
		if !p.HasCharge || !p.Charging {
			t.Errorf("point %d did not inherit the state from before the window", i)
		}
	}
}

// TestMergeShapedSeriesNilNumbersStayUnknown: a device with no battery reports none,
// and that must stay distinguishable from a reading of zero.
func TestMergeShapedSeriesNilNumbersStayUnknown(t *testing.T) {
	p := mergeShapedSeries([]db.DeviceSample{{At: time.Now()}}, nil)[0]
	if p.HasBattery || p.HasTemp || p.HasRAM {
		t.Errorf("absent readings reported as known: %+v", p)
	}
}

func TestAtoiState(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"1", 1, true},
		{"0", 0, true},
		{"-57", -57, true},
		{"", 0, false},
		{"true", 0, false}, // a boolean is not a number
		{"1.5", 0, false},  // wlc_status is an integer; anything else is a bug upstream
		{"null", 0, false}, // explicitly reported as null
	}
	for _, c := range cases {
		got, ok := atoiState(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("atoiState(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
