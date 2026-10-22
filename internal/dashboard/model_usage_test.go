package dashboard

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"mdm/internal/db"
)

func TestBuildModelUsageRows(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	byID := map[uuid.UUID]db.Device{a: {SerialNumber: "HK1-A", RestaurantName: "Mom's"}, b: {SerialNumber: "HK1-B"}, c: {SerialNumber: "HK1-C"}}
	f := func(v float64) *float64 { return &v }
	u := func(id uuid.UUID, pkg string, pss float64, max int, fg int64) db.DeviceAppUsage {
		return db.DeviceAppUsage{DeviceID: id, AppUsageRow: db.AppUsageRow{Package: pkg, PssAvgMB: f(pss), PssMaxMB: max, FgMs: fg, RxBytes: 1 << 20}}
	}
	usage := []db.DeviceAppUsage{
		u(a, "aio.app.menuboards", 400, 512, 3600000), u(b, "aio.app.menuboards", 380, 420, 3600000), u(c, "aio.app.menuboards", 384, 400, 0),
		u(a, "com.google.android.gms", 220, 300, 0), u(b, "com.google.android.gms", 232, 250, 0),
	}
	rows, more, reporting, top := buildModelUsageRows(usage, byID, map[string]string{"aio.app.menuboards": "Menu boards"}, "mem")
	if reporting != 3 || more != 0 || len(rows) != 2 {
		t.Fatalf("reporting %d more %d rows %d", reporting, more, len(rows))
	}
	r := rows[0]
	if r.Name != "Menu boards" || r.On != 3 || r.Mem != "388 MB" || !strings.Contains(r.Worst, "512 MB · HK1-A") || r.BarPct != 100 {
		t.Errorf("menu boards row = %+v", r)
	}
	if len(r.Heaviest) != 3 || r.Heaviest[0].Serial != "HK1-A" || r.Heaviest[0].Venue != "Mom's" {
		t.Errorf("heaviest = %+v", r.Heaviest)
	}
	if rows[1].Mem != "226 MB" || rows[1].BarPct != 58 {
		t.Errorf("gms row = %+v", rows[1])
	}
	if !strings.HasPrefix(top, "Menu boards · ") {
		t.Errorf("top share = %q", top)
	}
	// By screen time: the board that was not on screen is not among the heaviest.
	rows, _, _, _ = buildModelUsageRows(usage, byID, nil, "screen")
	if rows[0].Package != "aio.app.menuboards" || len(rows[0].Heaviest) != 2 || rows[0].Screen != "40 min" {
		t.Errorf("screen sort = %+v", rows[0])
	}
}
