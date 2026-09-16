package db

import "testing"

// The derived figures are where a venue metric can quietly lie, so they are pinned here:
// a thin window must not read as a confident zero, and a device left on overnight must not
// report more than 100% of a site's opening hours.
func TestSiteMetricsDerived(t *testing.T) {
	// A week for 2 devices: open 10h/day, powered nearly all of it.
	m := SiteMetrics{
		Days: 7, DeviceCount: 2,
		PoweredMinutes:     2 * 7 * 20 * 60, // 20h/day each — they run outside opening hours too
		PoweredOpenMinutes: 2 * 7 * 9 * 60,  // 9 of the 10 open hours
		HasOpenHours:       true,
		OpenWindowMinutes:  2 * 7 * 10 * 60,
		FullWindowMinutes:  2 * 7 * 24 * 60,
	}
	if got := m.UptimeOpenPct(); got != 90 {
		t.Errorf("UptimeOpenPct = %d, want 90", got)
	}
	if got := m.UptimeFullPct(); got != 83 {
		t.Errorf("UptimeFullPct = %d, want 83 (20 of 24 hours)", got)
	}

	// Devices left on around the clock at a site that opens for 10 hours: the open-hours
	// share is capped rather than reported as 240%.
	always := SiteMetrics{
		Days: 7, DeviceCount: 1, HasOpenHours: true,
		PoweredOpenMinutes: 7 * 24 * 60,
		OpenWindowMinutes:  7 * 10 * 60,
		PoweredMinutes:     7 * 24 * 60,
		FullWindowMinutes:  7 * 24 * 60,
	}
	if got := always.UptimeOpenPct(); got != 100 {
		t.Errorf("UptimeOpenPct with round-the-clock devices = %d, want it capped at 100", got)
	}

	// No service window configured: the open-hours figure is not invented.
	noWindow := SiteMetrics{Days: 7, DeviceCount: 1, PoweredMinutes: 600, FullWindowMinutes: 7 * 24 * 60}
	if got := noWindow.UptimeOpenPct(); got != 0 {
		t.Errorf("UptimeOpenPct without opening hours = %d, want 0 (the UI says 'not set')", got)
	}
}

func TestSiteMetricsPadDrain(t *testing.T) {
	// 40 battery points over 80 minutes on the pad = 0.5%/min.
	m := SiteMetrics{PadDrainPct: 40, PadDrainMinutes: 80}
	if !m.HasPadDrain() {
		t.Fatal("80 minutes of measured pad time should be enough to report a rate")
	}
	if got := m.PadDrainPctPerMin(); got != 0.5 {
		t.Errorf("PadDrainPctPerMin = %v, want 0.5", got)
	}

	// Barely any pad time: report nothing rather than extrapolating a rate from a minute.
	thin := SiteMetrics{PadDrainPct: 3, PadDrainMinutes: 4}
	if thin.HasPadDrain() {
		t.Error("4 minutes of pad time should not be enough to claim a drain rate")
	}
	if got := thin.PadDrainPctPerMin(); got != 0 {
		t.Errorf("PadDrainPctPerMin on a thin window = %v, want 0", got)
	}

	// No pad use at all (a kiosk, or a T7 nobody used): not a divide by zero.
	if got := (SiteMetrics{}).PadDrainPctPerMin(); got != 0 {
		t.Errorf("PadDrainPctPerMin with no data = %v, want 0", got)
	}
}

// Standby is powered time nobody was using. It is only meaningful over the device-days
// that actually reported screen state, because the firmware carrying that field reaches
// the fleet gradually — a device that cannot answer must not read as permanently idle.
func TestSiteMetricsStandby(t *testing.T) {
	// 3 device-days reporting: 30h powered, 12h with the screen on.
	m := SiteMetrics{
		ScreenDeviceDays:     3,
		ScreenPoweredMinutes: 30 * 60,
		ScreenOnMinutes:      12 * 60,
		PoweredMinutes:       90 * 60, // the wider window includes devices that cannot report
	}
	if !m.HasStandby() {
		t.Fatal("device-days with screen data should report standby")
	}
	if got := m.StandbyMinutes(); got != 18*60 {
		t.Errorf("StandbyMinutes = %v, want %v (30h powered - 12h used)", got, 18*60)
	}
	if got := m.StandbyPct(); got != 60 {
		t.Errorf("StandbyPct = %d, want 60", got)
	}

	// Nothing reporting yet: the tile says so rather than claiming the fleet is idle.
	none := SiteMetrics{PoweredMinutes: 90 * 60}
	if none.HasStandby() {
		t.Error("no screen data should not report standby")
	}
	if none.StandbyMinutes() != 0 || none.StandbyPct() != 0 {
		t.Error("standby without data should be zero, not derived from powered time")
	}

	// Screen-on exceeding powered (clock skew, a partial day) must not go negative.
	odd := SiteMetrics{ScreenDeviceDays: 1, ScreenPoweredMinutes: 60, ScreenOnMinutes: 90}
	if got := odd.StandbyMinutes(); got != 0 {
		t.Errorf("StandbyMinutes = %v, want 0 rather than negative", got)
	}
}
