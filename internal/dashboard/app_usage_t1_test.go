package dashboard

import (
	"testing"

	"mdm/internal/db"
)

// T1: the top five per measure, the role app always among them, tones by who and how much.
func TestAppUsageTop(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	row := func(pkg, label string, sys bool, pss, cpu float64) db.AppUsageRow {
		return db.AppUsageRow{Package: pkg, Label: label, System: sys, PssAvgMB: f(pss), CPUAvg: f(cpu)}
	}
	rows := []db.AppUsageRow{
		row("com.android.chrome", "Chrome", true, 343, 0.4),
		row("com.google.android.gms", "Google Play services", true, 316, 1.3),
		row("com.vendor.big", "Big", false, 420, 25),
		row("android", "Android System", true, 164, 0),
		row("com.android.systemui", "System UI", true, 116, 2.6),
		row("com.aioapp.mdm", "AIO MDM", true, 17, 9.4),
		row("aio.app.nugget.pos", "Nugget", false, 15, 0.1),
	}
	mem := appUsageTop(rows, "mem", "aio.app.nugget")
	if len(mem.Bars) != 5 || mem.Bars[0].Name != "Big" || mem.Bars[0].Tone != "bad" || mem.Bars[0].Pct != 100 {
		t.Fatalf("mem = %+v", mem)
	}
	if last := mem.Bars[4]; last.Name != "Nugget" || last.Tone != "role" {
		t.Errorf("role app not kept in the top five: %+v", mem.Bars)
	}
	if mem.Bars[1].Tone != "sys" || mem.Total != "1.36 GB in apps" {
		t.Errorf("mem tones/total = %+v %q", mem.Bars[1], mem.Total)
	}
	cpu := appUsageTop(rows, "cpu", "aio.app.nugget")
	if cpu.Bars[0].Name != "Big" || cpu.Bars[1].Name != "AIO MDM" || cpu.Total != "39 % summed" {
		t.Errorf("cpu = %+v", cpu)
	}
	// Standard client rows: no memory/CPU, so the screen column carries it.
	scr := appUsageTop([]db.AppUsageRow{{Package: "a", FgMs: 3600000}, {Package: "b"}}, "screen", "")
	if len(scr.Bars) != 1 || scr.Bars[0].Name != "a" || scr.Bars[0].Value != "1.0 h" {
		t.Errorf("screen = %+v", scr)
	}
}
