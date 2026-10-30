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

	"mdm/internal/aiobackend"
	"mdm/internal/db"
)

// hubTmpl parses the given templates with every dashboard func stubbed, and the few that
// shape data (dict, add, sub, role checks) real.
func hubTmpl(t *testing.T, files ...string) *template.Template {
	t.Helper()
	funcs := template.FuncMap{}
	for _, f := range []string{"handlers.go", "access_simple.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(src), -1) {
			funcs[m[1]] = func(...any) any { return "" }
		}
	}
	funcs["dict"] = func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	funcs["add"] = func(a, b int) int { return a + b }
	funcs["sub"] = func(a, b int) int { return a - b }
	for _, f := range []string{"canAdmin", "canAdminOrOperator", "canOperate"} {
		funcs[f] = func(string) bool { return true }
	}
	funcs["isCustomer"] = func(string) bool { return false }
	funcs["timeSince"] = func(any) string { return "just now" }
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}`))
	var paths []string
	for _, f := range files {
		paths = append(paths, "../../templates/"+f)
	}
	tmpl = template.Must(tmpl.ParseFiles(paths...))
	// layout.html brings the real header and footer: replace them with bare ones (an
	// empty body would not replace them, so each holds a hidden tag).
	return template.Must(tmpl.Parse(`{{define "header"}}<i hidden></i>{{end}}{{define "footer"}}<i hidden></i>{{end}}`))
}

func TestEnrollmentHubRenders(t *testing.T) {
	now := time.Now()
	cov := buildProdCoverage(covInputs{Base: "/enrollment?tab=backend", Env: "prod", All: true, Now: now,
		Rs:  []db.BackendRestaurant{{Env: "prod", Restaurant: aiobackend.Restaurant{ID: 9, Name: "Redwood Grill", RecordStatus: "Active"}}},
		Bds: []db.BackendDevice{{Env: "prod", Device: aiobackend.Device{ID: 1, RestaurantID: 9, Type: "kds", Serial: "DK1"}}}})
	pl := &venuesPlacementView{Rows: []placementRow{{R: db.BackendRestaurant{Env: "prod", Restaurant: aiobackend.Restaurant{ID: 1960, Name: "Ole Ole Burrito"}},
		Devs: []placementDev{{Serial: "bcf308127809f0b0", Label: "Self-order kiosk"}}, Total: 19}}, Devices: 1}
	data := map[string]any{
		"Role": "admin", "Stats": db.EnrollmentStats{Inbox: 3, Enrolled7d: 6, Firmware: 200, DPC: 20},
		"Cov": &cov, "CovWork": cov.C.Missing + cov.C.Elsewhere, "Placements": pl,
		"Prov": map[string]any{"AgentAPKHosted": true, "AgentAPKHostedInfo": map[string]any{"URL": "https://mdm/agent.apk", "Name": "dpc.apk", "Size": 1, "At": now, "SHA": "x"},
			"FleetAdbKey": map[string]any{"SecretSet": true}},
		"Restaurants": []db.Restaurant{{ID: uuid.New(), Name: "Huanchaco"}},
		"Profiles": []db.EnrollmentProfile{{ID: uuid.New(), Name: "AIO Enroll app · binyamin", Token: "enr_x", EnrollCount: 30}},
		"Enrollments": []db.Enrollment{{Serial: "DP02254J10876", DeviceClass: "pos", EnrolledAt: now}},
		"Unenrollments": []db.Unenrollment{},
		"Inbox": []db.Device{{ID: uuid.New(), SerialNumber: "AK270AABU00026", AgentKind: "dpc", EnrolledAt: now}},
		"Sightings": []sightingGroup{}, "Groups": []db.Group{},
		"SightingsReady": 2, "ExpiredProfiles": 0, "Enrolling": false, "ScoutOn": true, "FleetKeySet": true,
	}
	var buf bytes.Buffer
	if err := hubTmpl(t, "enrollment.html", "coverage.html", "restaurants.html", "layout.html", "_settings_cards.html").ExecuteTemplate(&buf, "enrollment.html", data); err != nil {
		t.Fatalf("enrollment.html: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		// E1 (30 Oct 2026): Fleet's layout, the tabs as the left list, tables on the right.
		`class="fleet-rail en-rail"`, `data-en-tab="inbox"><span class="dot warn"></span><span class="nm">Needs a venue</span><span class="ct">3</span>`,
		`<b>3</b> need a venue · 220 in the MDM · 6 this week`,
		`data-en-tab="backend"`, `<section class="en-tabpane" data-tab="backend"`, `data-en-tab="found"`,
		`In the AIO backend, not here yet`, `Ole Ole Burrito`, `Redwood Grill`, `href="/enrollment?tab=backend&amp;type=pos"`,
		`Standard MDM for QR provisioning`, `id="fleet-adb-key"`,
		`class="cc-btn" data-en-tab="how">`, `/static/video/enroll-guide.mp4`, `data-t="43.6"`,
		// Needs a venue as a table: tick boxes, the assign bar, a venue picker per row.
		`id="en-assign"`, `id="en-inbox-all"`, `class="ft-row en-row lt-row" id="dev-AK270AABU00026"`, `class="en-dpick" value="AK270AABU00026"`,
		`action="/devices/AK270AABU00026/onboard"`, `>Assign</button>`, `data-f="type" data-v="needed">Model has no class`,
		// Enrolled and Profiles as tables.
		`data-f="week" data-v="1">This week`, `id="prof-`, `data-f-state="active"`, `id="en-prof-all"`, `action="/enrollment/profiles/bulk"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Enrollment hub is missing %q", want)
		}
	}
	if strings.Contains(out, "Unenrollments") || strings.Contains(out, "How devices get here") {
		t.Error("Enrollment hub still shows unenrollments or the old tab name")
	}
	// No number cards any more, and How to enroll is a header button, not a list entry.
	if strings.Contains(out, `class="en-fun"`) || strings.Contains(out, `class="fleet-rail-item" data-en-tab="how"`) {
		t.Error("the funnel cards or a How to enroll rail entry are back")
	}
	// Found on Wi-Fi and From the AIO backend are super admin only (26 Oct 2026).
	data["Role"] = "fleet_admin"
	buf.Reset()
	if err := hubTmpl(t, "enrollment.html", "coverage.html", "restaurants.html", "layout.html", "_settings_cards.html").ExecuteTemplate(&buf, "enrollment.html", data); err != nil {
		t.Fatalf("enrollment.html as fleet_admin: %v", err)
	}
	for _, gone := range []string{`data-tab="found"`, `data-tab="backend"`, `data-en-tab="found"`, `data-en-tab="backend"`} {
		if strings.Contains(buf.String(), gone) {
			t.Errorf("a non-super-admin sees %q", gone)
		}
	}
}
