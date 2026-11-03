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

// M1 (3 Nov 2026): the Enrollment page's views are Setup's, each in the pane alone.
func TestSetupEnrollmentViews(t *testing.T) {
	now := time.Now()
	venue := uuid.New()
	in := covInputs{Base: "/enrollment?tab=backend", Now: now,
		Rs: []db.BackendRestaurant{{Env: "prod", Restaurant: aiobackend.Restaurant{ID: 9, Name: "Redwood Grill", RecordStatus: "Active"}},
			{Env: "prod", Restaurant: aiobackend.Restaurant{ID: 1960, Name: "Ole Ole Burrito", City: "Oakland"}}},
		Bds: []db.BackendDevice{{Env: "prod", Device: aiobackend.Device{ID: 1, RestaurantID: 9, Type: "kds", Serial: "DK1"}},
			{Env: "prod", Device: aiobackend.Device{ID: 2, RestaurantID: 1960, Type: "kiosk", Serial: "BCF308127809F0B0"}}},
		Names: map[uuid.UUID]string{venue: "Huanchaco"}}
	pl := placements{Rows: []placementRow{{R: in.Rs[1], Devs: []placementDev{{Serial: "bcf308127809f0b0", Label: "Self-order kiosk"}}, Total: 19}}}
	bk := buildBackendB1(in, pl, map[string]bool{"prod/9": true}, []db.Restaurant{{ID: venue, Name: "Huanchaco"}})
	base := func(role, view string) map[string]any {
		d := map[string]any{
			"Role": role, "View": view, "EnView": enrollmentViews[view], "Stats": db.EnrollmentStats{Inbox: 3, Enrolled7d: 6, Firmware: 200, DPC: 20},
			"Bk": &bk, "SightingsReady": 2, "Sightings": []sightingGroup{},
			"Prov": map[string]any{"AgentAPKHosted": true, "AgentAPKHostedInfo": map[string]any{"URL": "https://mdm/agent.apk", "Name": "dpc.apk", "Size": 1, "At": now, "SHA": "x"},
				"FleetAdbKey": map[string]any{"SecretSet": true}},
			"Restaurants": []db.Restaurant{{ID: venue, Name: "Huanchaco"}}, "Venues": []db.Restaurant{{ID: venue, Name: "Huanchaco"}},
			"Profiles":    []db.EnrollmentProfile{{ID: uuid.New(), Name: "AIO Enroll app · binyamin", Token: "enr_x", EnrollCount: 30}},
			"Enrollments": []db.Enrollment{{Serial: "DP02254J10876", DeviceClass: "pos", EnrolledAt: now}},
			"Groups":      []db.Group{}, "ExpiredProfiles": 0, "Enrolling": false, "ScoutOn": true, "FleetKeySet": true,
			"DeviceKey": "secret-key", "DeviceKeyMask": "sec••••key",
			"Rows": []placeRow{}, "SetRows": []visitDeviceRow{}, "SvcRows": []visitDeviceRow{},
		}
		if role != "admin" { // as enrollmentData: the provisioning cards and the key are the super admin's
			delete(d, "Prov")
			d["DeviceKey"], d["DeviceKeyMask"] = "", ""
		}
		return d
	}
	tmpl := hubTmpl(t, "ops_home.html", "enrollment.html", "restaurants.html", "layout.html", "_settings_cards.html")
	render := func(role, view string) string {
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "ops_home.html", base(role, view)); err != nil {
			t.Fatalf("%s as %s: %v", view, role, err)
		}
		return buf.String()
	}
	for view, wants := range map[string][]string{
		"recent":   {`data-tab="recent"`, `data-f="week" data-v="1">This week`, `DP02254J10876`},
		"profiles": {`id="prof-`, `data-f-state="active"`, `id="en-prof-all"`, `action="/enrollment/profiles/bulk"`},
		"backend":  {`data-cl data-one="restaurant"`, `data-start="act"`, `data-flags="env-prod all act real here"`, `value="unignore"`, `>Huanchaco</option>`},
		"how":      {`id="en-how-tabs"`, `/static/video/enroll-guide.mp4`, `Standard MDM for QR provisioning`, `id="fleet-adb-key"`, `secret-key`},
	} {
		out := render("admin", view)
		for _, w := range append(wants, `hx-get="/visits?view=recent"`, `>From the AIO backend<`, `onclick="enOpenModal()"`, `id="en-modal"`, `/static/js/enrollment.js`) {
			if !strings.Contains(out, w) {
				t.Errorf("%s view is missing %q", view, w)
			}
		}
	}
	// The Ops lead: Enrolled and How to enroll, none of the super admin's (no tokens, no key).
	out := render("fleet_manager", "how")
	for _, gone := range []string{`view=profiles`, `view=found`, `view=backend`, `secret-key`, `id="fleet-adb-key"`, `id="en-modal"`} {
		if strings.Contains(out, gone) {
			t.Errorf("an Ops lead sees %q", gone)
		}
	}
	if !strings.Contains(out, `view=recent`) {
		t.Error("an Ops lead should see Enrolled")
	}
}
