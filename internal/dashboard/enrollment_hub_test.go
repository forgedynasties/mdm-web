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
	cov := buildProdCoverage(covInputs{Base: "/enrollment?tab=backend", Env: "prod", Now: now,
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
		"Profiles": []db.EnrollmentProfile{}, "Enrollments": []db.Enrollment{}, "Unenrollments": []db.Unenrollment{},
		"Inbox": []db.Device{}, "Sightings": []sightingGroup{}, "Groups": []db.Group{},
		"SightingsReady": 2, "ExpiredProfiles": 0, "Enrolling": false, "ScoutOn": true, "FleetKeySet": true,
	}
	var buf bytes.Buffer
	if err := hubTmpl(t, "enrollment.html", "coverage.html", "restaurants.html", "layout.html").ExecuteTemplate(&buf, "enrollment.html", data); err != nil {
		t.Fatalf("enrollment.html: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`class="en-fun"`, `<b>220</b>`, `▲ 6 this week`, `<span class="k">In the AIO backend</span><b>1</b>`,
		`data-tab="backend">From the AIO backend`, `<section class="en-tabpane" data-tab="backend"`,
		`In the AIO backend, not here yet`, `Ole Ole Burrito`, `Redwood Grill`, `href="/enrollment?tab=backend&amp;type=pos"`,
		`Standard MDM for QR provisioning`, `id="fleet-adb-key"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Enrollment hub is missing %q", want)
		}
	}
}
