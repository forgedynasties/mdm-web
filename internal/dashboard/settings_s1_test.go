package dashboard

import (
	"bytes"
	"html/template"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"mdm/internal/db"
)

func TestSettingsS1Builder(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	v := buildSettingsS1(s1Inputs{
		TimeoutSecs: 86400, Now: now,
		SignedIn: []db.SignedIn{{Username: "ali@aioapp.com", LastSeen: now.Add(-30 * time.Second), Sessions: 2}, {Username: "ghost", LastSeen: now.Add(-20 * time.Minute), Sessions: 1}},
		Names:    map[string]string{"ali@aioapp.com": "Ali H."},
		Profiles: []db.AccessProfile{{Name: "Look only"}, {Name: "Look and fix"}, {Name: "Manage apps too"}},
	})
	if v.SessionLen != "24 hours" || v.ProfilesN != 3 || v.Profiles != "Look only · Look and fix · Manage apps too" {
		t.Errorf("view = %+v", v)
	}
	if len(v.SignedIn) != 2 || v.SignedIn[0].Name != "Ali H." || v.SignedIn[0].Seen != "now" || v.SignedIn[1].Name != "ghost" || v.SignedIn[1].Seen != "20m ago" {
		t.Errorf("signed in = %+v", v.SignedIn)
	}
	if v.RemoteRoles != "Super admins, Admins and Fleet managers, plus anyone a rule names" || v.ShellRoles != "Super admins, plus anyone a rule names" {
		t.Errorf("holders = %q / %q", v.RemoteRoles, v.ShellRoles)
	}
	for secs, want := range map[int]string{1800: "30 minutes", 3600: "1 hour", 604800: "7 days", 1000: "1000 seconds"} {
		if got := s1Duration(secs); got != want {
			t.Errorf("s1Duration(%d) = %q, want %q", secs, got, want)
		}
	}
}

func TestSettingsS1Renders(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := template.FuncMap{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(src), -1) {
		funcs[m[1]] = func(...any) any { return "" }
	}
	funcs["dict"] = func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	tmpl := template.Must(template.New("").Funcs(funcs).ParseFiles("../../templates/settings.html"))
	now := time.Now()
	data := map[string]any{
		"S1": buildSettingsS1(s1Inputs{TimeoutSecs: 86400, Now: now, SignedIn: []db.SignedIn{{Username: "ali", LastSeen: now}},
			Profiles: []db.AccessProfile{{Name: "Look only"}}}),
		"SignedInN": 1, "SessionTimeout": 86400, "RemoteEnabled": true, "ShellEnabled": false, "RequireReason": false, "ComplianceReportsCustomers": true,
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "settings-people", data); err != nil {
		t.Fatalf("settings-people failed to render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`data-tab="people"`, `<span>24 hours</span>`, `action="/settings/session-timeout"`, `action="/settings/logout-all"`,
		`1 person signed in now`, `action="/settings/remote/toggle"`, `action="/settings/shell/toggle"`,
		`action="/settings/require-reason"`, `action="/settings/compliance-reports/toggle"`, `href="/users/access/profiles"`,
		`<span class="s1-st">On</span><small>live screen`, `<span class="s1-st">Off</span><small>one-off shell`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("People & sign-in is missing %q", want)
		}
	}
	// Each setting lives in one place now.
	all, _ := os.ReadFile("../../templates/settings.html")
	for _, action := range []string{`"/settings/session-timeout"`, `"/settings/logout-all"`, `"/settings/remote/toggle"`, `"/settings/shell/toggle"`, `"/settings/require-reason"`, `"/settings/compliance-reports/toggle"`} {
		if n := strings.Count(string(all), action); n != 1 {
			t.Errorf("%s appears %d times in settings.html, want 1", action, n)
		}
	}
	if p := os.Getenv("S1_RENDER_OUT"); p != "" {
		os.WriteFile(p, buf.Bytes(), 0o644)
	}
}
