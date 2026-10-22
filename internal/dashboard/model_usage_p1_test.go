package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

func TestBuildModelUsageP1(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	f := func(v float64) *float64 { return &v }
	ram := json.RawMessage(`{"ram_usage_mb":{"used":700,"total":1000}}`)
	full := json.RawMessage(`{"ram_usage_mb":{"used":950,"total":1000}}`)
	a, b, c, d := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	byID := map[uuid.UUID]db.Device{
		a: {ID: a, SerialNumber: "BOX-A", Product: "rbox_d8", RestaurantName: "Mom's", LatestExtra: full},
		b: {ID: b, SerialNumber: "BOX-B", Product: "rbox_d8", LatestExtra: ram},
		c: {ID: c, SerialNumber: "BOX-C", Product: "rbox_d8", LatestExtra: ram},
		d: {ID: d, SerialNumber: "D3-1", Product: "d3_pro"},
	}
	key := byID[a].ProductKey()
	u := func(id uuid.UUID, pkg string, pss, cpu float64, fg int64) db.DeviceAppUsage {
		return db.DeviceAppUsage{DeviceID: id, AppUsageRow: db.AppUsageRow{Package: pkg, PssAvgMB: f(pss), CPUAvg: f(cpu), FgMs: fg, RxBytes: 1 << 20}}
	}
	all := []db.DeviceAppUsage{
		u(a, "aio.app.menuboards", 600, 10, 3600000), u(b, "aio.app.menuboards", 300, 8, 3600000), u(c, "aio.app.menuboards", 300, 8, 3600000),
		u(a, "com.vendor.ota", 350, 2, 0), u(b, "com.vendor.ota", 340, 2, 0),
		{DeviceID: d, AppUsageRow: db.AppUsageRow{Package: "aio.app.pos", FgMs: 7200000}},
	}
	prev := []db.DeviceAppUsage{u(a, "aio.app.menuboards", 300, 8, 3600000), u(b, "aio.app.menuboards", 300, 8, 3600000)}
	in := p1In{Now: now, Win: 24 * time.Hour, Since: now.Add(-24 * time.Hour), WeekSince: now.AddDate(0, 0, -6).Truncate(24 * time.Hour),
		Model: key, ModelLabel: "HK1 RBOX", Window: "24h", ByID: byID, All: all, Prev: prev,
		Labels: map[string]string{"aio.app.menuboards": "Menu boards"}, RoleBase: "aio.app.menuboards"}
	p := buildModelUsageP1(in)

	if p.Devices != 3 || p.Reporting != 3 || !p.Full || p.MeasureLabel != "Memory" || p.Empty {
		t.Fatalf("page = devices %d reporting %d full %v measure %q", p.Devices, p.Reporting, p.Full, p.MeasureLabel)
	}
	if !p.Rows[0].Role || p.Rows[0].Package != "aio.app.menuboards" {
		t.Errorf("first row = %+v", p.Rows[0])
	}
	// Banner: com.vendor.ota is over 300 MB (not the role app), BOX-A is 30 %+ above the model,
	// and com.vendor.ota holds memory without coming on screen.
	var titles []string
	for _, td := range p.Banner.Todos {
		titles = append(titles, td.Title)
	}
	got := strings.Join(titles, " | ")
	for _, want := range []string{"com.vendor.ota averages 345 MB", "BOX-A uses", "holds 345 MB without coming on screen"} {
		if !strings.Contains(got, want) {
			t.Errorf("banner %q lacks %q", got, want)
		}
	}
	if p.Banner.Headline != "3 things to look at" || !strings.Contains(p.Banner.Kicker, "HK1 RBOX") {
		t.Errorf("banner = %+v", p.Banner)
	}
	// Memory card: per device (950+640+300)/3 = 630 MB against 300 the day before.
	if len(p.Cards) != 3 || p.Cards[0].A.Value != "630 MB" || !strings.HasPrefix(p.Cards[0].A.Delta, "▲") || p.Cards[0].A.Tone != "bad" {
		t.Errorf("memory card = %+v", p.Cards[0])
	}
	if p.Cards[0].B.Label != "of RAM in use now" || p.Cards[0].B.Value != "78 %" {
		t.Errorf("memory card B = %+v", p.Cards[0].B)
	}
	// Ring: 2 of 3 under 85 % RAM.
	if p.Ring.Pct != "66" || p.Ring.Label != "within RAM budget" {
		t.Errorf("ring = %+v", p.Ring)
	}
	if len(p.Heavy) == 0 || p.Heavy[0].Serial != "BOX-A" || p.Heavy[0].Tone != "danger" {
		t.Errorf("heavy = %+v", p.Heavy)
	}
	// Models: both, the one in view marked, the D3 with no memory measured.
	if len(p.ModelRows) != 2 || !p.ModelRows[0].On || p.ModelRows[1].Value != "—" || p.ModelRows[1].Reporting != 1 {
		t.Errorf("models = %+v", p.ModelRows)
	}
	if len(p.Top) == 0 || p.Top[0].Tone != "role" {
		t.Errorf("top = %+v", p.Top)
	}

	// A Standard-only model: screen time, no memory card.
	in.Model, in.ModelLabel, in.Measure = byID[d].ProductKey(), "D3 Pro", "mem"
	in.Prev, in.RoleBase = nil, ""
	p = buildModelUsageP1(in)
	if p.Full || p.MeasureLabel != "Screen time" || p.Cards[0].Title != "Screen time" || p.Cards[2].Title != "Reporting" {
		t.Errorf("standard model = full %v measure %q cards %+v", p.Full, p.MeasureLabel, p.Cards)
	}
	if p.Banner.Tone != "calm" {
		t.Errorf("standard banner = %+v", p.Banner)
	}
}

func TestBuildModelUsageP1Empty(t *testing.T) {
	now := time.Now()
	a := uuid.New()
	byID := map[uuid.UUID]db.Device{a: {ID: a, SerialNumber: "K22-1", Product: "kiosk22"}}
	p := buildModelUsageP1(p1In{Now: now, Win: 24 * time.Hour, Since: now.Add(-24 * time.Hour), WeekSince: now.AddDate(0, 0, -6),
		Model: byID[a].ProductKey(), ModelLabel: "Kiosk 22", Window: "24h", ByID: byID})
	if !p.Empty || p.Banner.Headline != "Nothing reported yet" || !strings.Contains(p.Banner.Calm, "1.17.1") {
		t.Errorf("empty = %+v", p.Banner)
	}
}

func TestP1DefaultModel(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	byID := map[uuid.UUID]db.Device{
		a: {ID: a, Product: "kiosk22"}, b: {ID: b, Product: "kiosk22"}, c: {ID: c, Product: "d3_pro"},
	}
	usage := []db.DeviceAppUsage{{DeviceID: c, AppUsageRow: db.AppUsageRow{Package: "x"}}}
	if got := p1DefaultModel("", byID, usage); got != byID[c].ProductKey() {
		t.Errorf("default = %q, want the model that reports", got)
	}
	if got := p1DefaultModel(byID[a].ProductKey(), byID, usage); got != byID[a].ProductKey() {
		t.Errorf("asked-for model not kept: %q", got)
	}
}
