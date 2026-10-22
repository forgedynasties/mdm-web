package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

func TestModelUsageU1(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 30, 0, 0, time.UTC)
	a, b := uuid.New(), uuid.New()
	ram := json.RawMessage(`{"ram_usage_mb":{"used":2900,"total":4000},"host_app":{"variants":[{"exits_24h":{"low_memory":3}}]}}`)
	byID := map[uuid.UUID]db.Device{a: {SerialNumber: "T7-A", LatestExtra: ram}, b: {SerialNumber: "T7-B", LatestExtra: json.RawMessage(`{"ram_usage_mb":{"used":1000,"total":4000}}`)}}
	f := func(v float64) *float64 { return &v }
	mk := func(id uuid.UUID, pkg string, pss float64, fg int64) db.DeviceAppUsage {
		return db.DeviceAppUsage{DeviceID: id, AppUsageRow: db.AppUsageRow{Package: pkg, Label: pkg, PssAvgMB: f(pss), FgMs: fg}}
	}
	usage := []db.DeviceAppUsage{mk(a, "aio.app.nugget", 460, 3600000), mk(b, "aio.app.nugget", 400, 3600000), mk(a, "com.android.chrome", 365, 0), mk(b, "com.android.chrome", 340, 0)}
	p := modelUsagePage{Window: "24h", Sort: "mem", AnyFull: true}
	p.Rows, _, p.Reporting, _ = buildModelUsageRows(usage, byID, nil, "mem")
	series := []db.UsagePoint{{Bucket: now.Truncate(time.Hour), DeviceID: a, PssMB: 825, HasPss: true}, {Bucket: now.Truncate(time.Hour).Add(-2 * time.Hour), DeviceID: a, PssMB: 400, HasPss: true}}
	v := buildModelUsageU1(&p, usage, series, byID, "aio.app.nugget", now)
	if v.HeroNum != "782" || v.HeroUnit != "MB" {
		t.Errorf("hero = %s %s", v.HeroNum, v.HeroUnit)
	}
	if len(v.Bars) != 24 || !v.Bars[23].Hi || v.Bars[21].Hi || !strings.Contains(v.Peak, "825 MB at 12:00") {
		t.Errorf("bars: %d, peak %q", len(v.Bars), v.Peak)
	}
	if len(v.Tanks) != 2 || v.Tanks[0].Warn != "" || v.Tanks[1].Tone != "r" || v.Tanks[1].Warn != "over 300 MB" {
		t.Errorf("tanks = %+v (the role app must not be red, Chrome must)", v.Tanks)
	}
	if v.Heavy == nil || v.Heavy.Serial != "T7-A" {
		t.Errorf("heavy = %+v", v.Heavy)
	}
	if v.RAMPct != 48 {
		t.Errorf("ram = %d %s", v.RAMPct, v.RAMText)
	}
	titles := ""
	for _, l := range v.Looks {
		titles += l.Title + " | "
	}
	for _, w := range []string{"com.android.chrome open in the background", "Host app stopped (low memory)", "Within the RAM budget"} {
		if !strings.Contains(titles, w) {
			t.Errorf("looks %q missing %q", titles, w)
		}
	}
}
