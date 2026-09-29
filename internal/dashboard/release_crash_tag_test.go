package dashboard

import (
	"bytes"
	"html/template"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestReleasesCrashTagRenders executes releases.html with one release carrying crash
// issues: the tag must link to that release's crashes and name the apps.
func TestReleasesCrashTagRenders(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatalf("read handlers.go: %v", err)
	}
	funcs := template.FuncMap{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(src), -1) {
		funcs[m[1]] = func(...any) any { return "" }
	}
	funcs["canOperate"] = func(string) bool { return true }
	funcs["canRelease"] = func(string) bool { return true }
	funcs["pctOf"] = func(a, b int) int { return 0 }
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}{{define "ota-progress"}}{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("../../templates/releases.html"))
	id := 7046
	rows := []versionRow{{Version: "v2.0.96l", ReleaseID: &id, Tracked: true, Status: "published", DeviceCount: 59,
		Crash: &releaseCrashTag{Issues: 2, Devices: 34, Apps: "com.google.android.permissioncontroller crash (34 devices), com.qti.phone crash (4 devices)"}}}
	var buf bytes.Buffer
	err = tmpl.ExecuteTemplate(&buf, "releases.html", map[string]any{"Versions": rows, "FleetTotal": 181, "Role": "admin",
		"LatestPublishedPct": 0, "NotTrackedCount": 0, "TrackedCount": 1, "PublishedCount": 1, "DraftCount": 0, "UnderTestCount": 0, "DevCount": 0})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{`href="/releases/7046/crashes"`, "2 crash issues · 34 devices", "com.qti.phone crash (4 devices)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}
