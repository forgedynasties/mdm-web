package dashboard

import (
	"bytes"
	"html/template"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// d1Sample is deployment #422 at 11:15 UTC on 21 Oct 2026, after the wave fix: one
// tablet silent since its reboot, three downloading, seven in line at Flights Vegas
// (one offline), twelve installed across two venues.
func d1Sample(now time.Time) dp1Inputs {
	at := func(m int) *time.Time { t := now.Add(time.Duration(m) * time.Minute); return &t }
	fv, hu := uuid.New(), uuid.New()
	start := now.Add(-42 * time.Minute)
	var ts []db.UpdateTarget
	prog := map[uuid.UUID]otaRowView{}
	add := func(sn string, venue uuid.UUID, name, status string, f func(*db.UpdateTarget)) {
		t := db.UpdateTarget{DeviceID: uuid.New(), SerialNumber: sn, Status: status, BuildID: "v2.0.96l", SourceBuildID: "v2.0.96l",
			UpdatedAt: start, LastSeenAt: now.Add(-time.Minute), VenueID: &venue, Venue: name}
		if f != nil {
			f(&t)
		}
		ts = append(ts, t)
	}
	add("AT070AABU00644", fv, "Flights Vegas", "reboot_sent", func(t *db.UpdateTarget) {
		t.StartedAt, t.RebootSentAt, t.LastSeenAt = at(-42), at(-30), now.Add(-29*time.Minute)
	})
	for i, sn := range []string{"AT070AABU00116", "AT070AABU00190", "AT070AABU00242"} {
		pc := []int{41, 33, 12}[i]
		add(sn, fv, "Flights Vegas", "downloading", func(t *db.UpdateTarget) {
			t.StartedAt, t.UpdatedAt = at(-4), now.Add(-4*time.Minute)
			prog[t.DeviceID] = otaRowView{Phase: "downloading", Percent: pc, BytesTotal: 760 << 20, SeenAt: now, AdvancedAt: now}
		})
	}
	for _, sn := range []string{"AT070AABU00450", "AT070AABU00527", "AT070AABU00601", "AT070AABU00607", "AT070AABU00673"} {
		add(sn, fv, "Flights Vegas", "pending", nil)
	}
	add("AT070AABU00802", fv, "Flights Vegas", "pending", func(t *db.UpdateTarget) { t.LastSeenAt = now.Add(-2 * time.Hour) })
	add("AT070AABU00034", fv, "Flights Vegas", "pending", func(t *db.UpdateTarget) { t.BuildID, t.SourceBuildID = "v2.0.6l", "v2.0.6l" })
	done := func(sn string, venue uuid.UUID, name string, s, c int) {
		add(sn, venue, name, "installed", func(t *db.UpdateTarget) {
			t.StartedAt, t.CompletedAt, t.BuildID = at(s), at(c), "v2.0.997"
		})
	}
	for _, sn := range []string{"AT070AABU00703", "AT070AABU00746"} {
		done(sn, fv, "Flights Vegas", -42, -28)
	}
	for _, sn := range []string{"AT070AABU00509", "AT070AABU00600", "AT070AABU00808"} {
		done(sn, hu, "Huanchaco Peruvian", -42, -28)
	}
	for _, sn := range []string{"AT070AABU00489", "AT070AABU00655", "AT070AABU00772"} {
		done(sn, fv, "Flights Vegas", -30, -16)
	}
	done("AT070AABU00690", hu, "Huanchaco Peruvian", -30, -16)
	for _, sn := range []string{"AT070AABU00249", "AT070AABU00583", "AT070AABU00864"} {
		done(sn, fv, "Flights Vegas", -18, -4)
	}
	return dp1Inputs{
		Dep:     &db.Update{ID: 422, ReleaseID: 10862, Status: "active", RebootBehavior: "immediate", CreatedAt: start},
		Version: "v2.0.997", Targets: ts, Progress: prog,
		Incremental: map[string]bool{"v2.0.96l": true, "v2.0.994": true, "v2.0.995": true}, HasFull: true,
		CanOperate: true, Base: "/releases/10862/deployments/422", SeenSecs: 900, Now: now,
	}
}

func TestDeployD1Builder(t *testing.T) {
	now := time.Date(2026, 10, 21, 11, 15, 0, 0, time.UTC)
	v := buildDeployD1(d1Sample(now))

	if v.Banner.Headline != "1 thing to look at" || len(v.Banner.Todos) != 1 {
		t.Fatalf("banner = %+v", v.Banner)
	}
	if td := v.Banner.Todos[0]; td.Title != "AT070AABU00644 hasn't come back from its reboot" || td.Href != "/devices/AT070AABU00644" {
		t.Errorf("todo = %+v", td)
	}
	// Six online in line at one venue, one offline.
	if l := v.Banner.Line; !strings.HasPrefix(l, "The other 6 devices at Flights Vegas are online and get it at their next check-in.") ||
		!strings.Contains(l, "1 device is offline and will get it when back.") {
		t.Errorf("line = %q", l)
	}
	if v.Counts["look"] != 1 || v.Counts["dl"] != 3 || v.Counts["wait"] != 7 || v.Counts["ok"] != 12 || v.Counts["failed"] != 0 {
		t.Errorf("counts = %v", v.Counts)
	}
	// Problems first, then downloading, waiting, installed.
	if v.Rows[0].Serial != "AT070AABU00644" || v.Rows[0].Word != "Not back from its reboot" || v.Rows[1].Key != "dl" || v.Rows[len(v.Rows)-1].Key != "ok" {
		t.Errorf("order: first %+v, second %+v", v.Rows[0], v.Rows[1])
	}
	for _, r := range v.Rows {
		switch r.Serial {
		case "AT070AABU00034":
			if !r.Full || r.Detail != "gets it at its next check-in" {
				t.Errorf("v2.0.6l row = %+v", r)
			}
		case "AT070AABU00802":
			if r.Detail != "offline · gets it when it is back" {
				t.Errorf("offline row = %+v", r)
			}
		case "AT070AABU00116":
			if !r.Bar || r.Pct != 41 || !strings.Contains(r.Detail, "41%") || !r.Cancel {
				t.Errorf("downloading row = %+v", r)
			}
		case "AT070AABU00864":
			if r.Detail != "4m ago · took 14 min" {
				t.Errorf("installed row = %+v", r)
			}
		}
	}
	if c := v.Cards[0]; c.A.Value != "12" || c.A.Delta != "▲ 3 last 15 min" || c.B.Value != "14 min" || c.LineA == "" {
		t.Errorf("installed card = %+v", c)
	}
	if c := v.Cards[1]; c.A.Value != "3" || c.B.Value != "1" || c.B.Delta != "1 not back" || c.B.Tone != "bad" {
		t.Errorf("in progress card = %+v", c)
	}
	// No caps: everything left downloads at once, about one 14 min download.
	if c := v.Cards[2]; c.A.Value != "7" || c.A.Delta != "about 14 min to go" || c.B.Value != "0" {
		t.Errorf("left card = %+v", c)
	}
	if v.Rail.Pct != 52 || v.Rail.Tone != "warn" || len(v.Rail.Venues) != 2 || v.Rail.Venues[0].Name != "Flights Vegas" || v.Rail.Venues[0].Done != 8 {
		t.Errorf("rail = %+v", v.Rail)
	}
	if v.Rail.Package != "22 incremental, 1 full" || v.Rail.PackageSub != "from v2.0.96l · v2.0.6l takes the full image" {
		t.Errorf("package = %q / %q", v.Rail.Package, v.Rail.PackageSub)
	}
	if v.Rail.BarsCap != "installs per 10 minutes" || len(v.Rail.Bars) < 4 {
		t.Errorf("bars = %q %d", v.Rail.BarsCap, len(v.Rail.Bars))
	}
}

func TestDeployD1Calm(t *testing.T) {
	now := time.Date(2026, 10, 21, 13, 0, 0, 0, time.UTC)
	in := d1Sample(now)
	keep := in.Targets[:0]
	for _, tg := range in.Targets {
		if tg.Status == "installed" {
			keep = append(keep, tg)
		}
	}
	in.Targets, in.Progress = keep, nil
	in.Dep.Status = "complete"
	v := buildDeployD1(in)
	if v.Banner.Tone != "calm" || v.Banner.Headline != "On all 12 devices" || len(v.Banner.Todos) != 0 || v.Rail.Tone != "" {
		t.Errorf("calm banner = %+v, rail tone %q", v.Banner, v.Rail.Tone)
	}
}

func TestDeployD1Renders(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := template.FuncMap{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(src), -1) {
		funcs[m[1]] = func(...any) any { return "" }
	}
	funcs["canOperate"] = func(string) bool { return true }
	funcs["canOTA"] = func(string) bool { return true }
	funcs["otaErrorText"] = func(s string) string {
		return "Download failed — the device couldn't fetch the OTA package (check the URL is reachable from the device)."
	}
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("../../templates/deployment_detail.html"))

	now := time.Date(2026, 10, 21, 11, 15, 0, 0, time.UTC)
	in := d1Sample(now)
	in.Dep.Release = &db.Release{ID: 10862, Version: "v2.0.997", Product: "t7"}
	for i := range in.Targets {
		if in.Targets[i].SerialNumber == "AT070AABU00450" {
			in.Targets[i].Status, in.Targets[i].ErrorCode = "failed", "DOWNLOAD_ERROR"
		}
	}
	data := map[string]any{
		"Role": "admin", "Deployment": in.Dep, "Release": in.Dep.Release, "SummaryTotal": len(in.Targets),
		"D1": buildDeployD1(in),
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "deployment_detail.html", data); err != nil {
		t.Fatalf("deployment_detail.html failed to render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`<h2>2 things to look at</h2>`, `href="/devices/AT070AABU00644">Open device</a>`,
		`class="dp1-st warn"><i></i>Not back from its reboot</span>`, `class="dp1-st q"><i></i>Waiting</span>`,
		`from v2.0.6l · full image`, `<i style="width:41%"></i>`,
		`action="/releases/10862/deployments/422/devices/AT070AABU00116/cancel-ota"`,
		`action="/releases/10862/deployments/422/devices/AT070AABU00527/remove"`,
		`aria-label="More for AT070AABU00450"`, `>Retry with the full image</button>`, `>Remove from this rollout</button>`,
		`data-dp1-set="state" data-val="wait">Waiting · 6</button>`, `data-dp1-set="venue" data-val="Huanchaco Peruvian"`,
		`<span><b>52%</b><small>installed</small></span>`, `onclick="dplOpen('dpl-reboot-modal')">Change</button>`,
		`data-copy-serials="#deployment-targets"`, `id="dpl-reboot-modal"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("deployment page is missing %q", want)
		}
	}
	for _, gone := range []string{`dpl-hero`, `class="cc-card dpl-reboot"`} {
		if strings.Contains(out, gone) {
			t.Errorf("old page piece %q is still there", gone)
		}
	}
	if p := os.Getenv("DP1_RENDER_OUT"); p != "" {
		os.WriteFile(p, buf.Bytes(), 0o644)
	}
}
