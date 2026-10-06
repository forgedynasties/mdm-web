package dashboard

import (
	"bytes"
	"encoding/json"
	"html/template"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

func problemFixture(now time.Time) []db.Alert {
	a := func(dev uuid.UUID, typ, sev, serial, rest, status string, fired time.Time) db.Alert {
		d := dev
		return db.Alert{ID: uuid.New(), DeviceID: &d, Type: typ, Severity: sev, Status: status,
			Serial: serial, RestaurantName: rest, RestaurantID: "r-" + rest, FiredAt: fired, Occurrences: 1}
	}
	pad, hot, v1, v2 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	later := now.Add(time.Hour)
	snoozed := a(uuid.New(), "storage_low", "warning", "T7-S", "Pho 88", "open", now)
	snoozed.MutedUntil = &later
	night := a(hot, "overheating", "warning", "T7-H", "Golden Wok", "acknowledged", now.Add(-time.Hour))
	night.Detail = json.RawMessage(`{"after_hours":true}`)
	night.Assignee = "sam"
	return []db.Alert{
		a(pad, "charger_flapping", "warning", "T7-P", "Pho 88", "open", now),
		a(pad, "battery_low", "critical", "T7-P", "Pho 88", "open", now.Add(-10*time.Minute)),
		a(pad, "slow_charge_night", "warning", "T7-P", "Pho 88", "open", now.Add(-8*time.Hour)),
		night,
		a(hot, "memory_low", "warning", "T7-H", "Golden Wok", "open", now),
		a(v1, "offline_peak", "critical", "T7-V1", "Flights Vegas", "open", now.Add(-3*time.Minute)),
		a(v2, "offline", "warning", "T7-V2", "Flights Vegas", "open", now.Add(-2*time.Minute)),
		snoozed,
	}
}

func TestBuildProblems(t *testing.T) {
	now := time.Now()
	as := problemFixture(now)
	hs := make([]humanAlert, len(as))
	for i, a := range as {
		hs[i] = humanizeAlert(a)
	}
	ps := buildProblems(as, hs, map[string]string{"sam": "Sam Lee"})
	if len(ps) != 4 {
		for _, p := range ps {
			t.Logf("%s (%s, %d alerts)", p.Title, p.Severity, len(p.Members))
		}
		t.Fatalf("got %d problems, want 4 (pad, hot, Vegas outage, snoozed)", len(ps))
	}
	byKey := map[string]problemView{}
	for _, p := range ps {
		byKey[p.Title] = p
	}
	pad, ok := byKey["T7-P: Charging hardware"]
	if !ok || pad.Severity != "critical" || len(pad.Symptoms) != 3 || !pad.Since.Equal(now.Add(-8*time.Hour)) {
		t.Errorf("pad problem = %+v", pad)
	}
	vegas, ok := byKey["Flights Vegas: 2 devices offline"]
	if !ok || vegas.Devices != 2 || vegas.Severity != "critical" || !strings.Contains(vegas.Why, "router") ||
		vegas.Href != "/restaurants/r-Flights Vegas" || len(strings.Split(vegas.IDs, ",")) != 2 {
		t.Errorf("restaurant outage = %+v", vegas)
	}
	hot, ok := byKey["T7-H: Overheating"]
	if !ok || !hot.AfterHours || !hot.NewSymptom || hot.Acked || hot.Assignee != "Sam Lee" {
		t.Errorf("hot problem = %+v", hot)
	}
	if ps[0].Severity != "critical" || ps[len(ps)-1].Severity != "warning" {
		t.Errorf("not worst first: %s … %s", ps[0].Severity, ps[len(ps)-1].Severity)
	}
	snz := 0
	for _, p := range ps {
		if p.Snoozed {
			snz++
		}
	}
	if snz != 1 {
		t.Errorf("snoozed problems = %d, want 1", snz)
	}
}

// TestAlertsProblemsRender executes alerts.html with problems, a release issue and the
// workflow menus, so a template error fails here rather than as a 500 on /alerts.
func TestAlertsProblemsRender(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatalf("read handlers.go: %v", err)
	}
	funcs := template.FuncMap{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(src), -1) {
		funcs[m[1]] = func(...any) any { return "" }
	}
	funcs["add"] = func(a, b int) int { return a + b }
	funcs["sub"] = func(a, b int) int { return a - b }
	funcs["canAdmin"] = func(string) bool { return true }
	funcs["canOperate"] = func(string) bool { return true }
	funcs["dict"] = func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}{{define "crash-trace"}}{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("../../templates/alerts.html"))

	now := time.Now()
	as := problemFixture(now)
	hs := make([]humanAlert, len(as))
	for i, a := range as {
		hs[i] = humanizeAlert(a)
		hs[i].CanAct = true
	}
	var needs, watching, snoozed []problemView
	for _, p := range buildProblems(as, hs, nil) {
		switch {
		case p.Snoozed:
			snoozed = append(snoozed, p)
		case p.Severity == "critical":
			needs = append(needs, p)
		default:
			watching = append(watching, p)
		}
	}
	issue := releaseIssueView{db.CrashIssue{Kind: "system_app_crash", BuildID: "v2.1.024",
		Summary: "com.google.android.permissioncontroller — java.lang.IllegalArgumentException", Events: 75, Devices: 40,
		FirstAt: now.Add(-72 * time.Hour), LastAt: now, ReleaseID: 7}, "com.google.android.permissioncontroller", "Crash", "crash"}
	data := map[string]any{
		"Title": "Alerts", "Summary": db.AlertSummary{}, "View": "",
		"Needs": needs, "WatchingP": watching, "Snoozed": snoozed,
		"NeedsCount": len(needs), "WatchCount": len(watching), "ActiveCount": len(needs) + len(watching) + len(snoozed),
		"AlertCount": len(as), "Issues": []releaseIssueView{issue},
		"Assignees": []assigneeOption{{"sam", "Sam Lee"}, {"me", "Me Myself"}}, "Me": "me",
		"Reasons": problemReasons(), "CanAct": true, "Role": "admin", "CrashCount": 0,
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "alerts.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if dump := os.Getenv("ALERTS_DUMP"); dump != "" {
		_ = os.WriteFile(dump, buf.Bytes(), 0o644)
	}
	for _, want := range []string{
		"T7-P: Charging hardware", "Flights Vegas: 2 devices offline", "Release issues",
		"v2.1.024: com.google.android.permissioncontroller", `href="/releases/7/crashes"`,
		`value="false_alarm"`, "after hours", "new since acknowledged", "Snoozed", "Sam Lee",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered page is missing %q", want)
		}
	}
}
