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

// peopleTmpl parses the People pages (access by levels, 2 Nov 2026) with every template
// function stubbed, and the few the pages need for real.
func peopleTmpl(t *testing.T, files ...string) *template.Template {
	t.Helper()
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
	funcs["list"] = func(xs ...string) []string { return xs }
	funcs["lower"] = strings.ToLower
	funcs["title"] = func(s string) string { return strings.ToUpper(s[:1]) + s[1:] }
	funcs["accessScopeText"] = scopeText
	funcs["roleLabel"] = roleLabel
	funcs["add"] = func(a, b int) int { return a + b }
	funcs["sub"] = func(a, b int) int { return a - b }
	funcs["deref"] = func(t *time.Time) time.Time { return *t }
	funcs["isCustomer"] = func(string) bool { return false }
	funcs["hasPrefix"] = strings.HasPrefix
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}{{define "ubub"}}{{end}}{{define "cl-sort"}}<div><div>{{end}}`))
	for i := range files {
		files[i] = "../../templates/" + files[i]
	}
	return template.Must(tmpl.ParseFiles(files...))
}

func renderPeople(t *testing.T, tmpl *template.Template, name string, data map[string]any, wants ...string) {
	t.Helper()
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		t.Fatalf("%s failed to render: %v", name, err)
	}
	out := buf.String()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("%s is missing %q", name, w)
		}
	}
}

func TestPeopleListRenders(t *testing.T) {
	now := time.Now()
	in := p1Sample(now)
	in.Team[0].Access = "Lab devices, the QA group"
	renderPeople(t, peopleTmpl(t, "users.html", "_users_tabs.html"), "users.html", map[string]any{
		"Role": "admin", "Team": in.Team, "Customers": in.Customers, "TeamRoles": []string{"viewer", "operator", "admin"},
		"CustomerRoles": []string{"owner", "staff"}, "RoleCount": map[string]int{"viewer": 2, "owner": 1},
		"Orphans": in.Orphans, "Waiting": []db.User{{ID: uuid.New(), Username: "new@x", Role: "viewer", CreatedAt: now}},
		"Invites": 1, "Quiet": 1, "Assignable": []string{"viewer"}, "UsersTab": "people",
	},
		`data-clf="team"`, `data-clf="cust"`, `data-clf="inv"`, `data-clf="quiet"`, `data-clf="r-viewer"`,
		`>Can operate</th>`, `Lab devices, the QA group`, `id="waiting-signups"`, `id="unlinked"`,
		`id="invite-modal"`, `href="/users/access"`, `href="/activity"`, `aria-current="page"`)
}

func TestAccessPageRenders(t *testing.T) {
	g := uuid.New()
	until := time.Now().Add(72 * time.Hour)
	gs := []db.AccessLevel{
		{ID: uuid.New(), SubjectType: "role", Subject: "engineer", ScopeType: "all", Level: "view", CreatedBy: "seed"},
		{ID: uuid.New(), SubjectType: "role", Subject: "engineer", ScopeType: "group", ScopeID: &g, ScopeName: "QA", Level: "operate", Until: &until, Devices: 31},
	}
	cards := []accessRoleCard{{Role: "engineer", Label: "Engineer", Plural: "Engineers", People: 10, Grants: gs, Sentence: accessSentence("Engineers", "They", gs, time.Now())}}
	tmpl := peopleTmpl(t, "users_access.html", "_users_tabs.html")
	renderPeople(t, tmpl, "users_access.html", map[string]any{
		"Role": "admin", "UsersTab": "access", "Cards": cards, "Sel": "engineer", "Now": time.Now(),
		"Groups": []db.Group{{ID: g, Name: "QA"}}, "Venues": []db.Restaurant{},
	},
		`Engineers can look at every device.`, `the QA group`, `value="manage"`, `starting point`,
		`action="/users/access/levels"`, `Give engineers access`, `>Who can act on…</span>`)
	renderPeople(t, tmpl, "users_access.html", map[string]any{
		"Role": "admin", "UsersTab": "access", "Cards": cards, "Who": "group:" + g.String(), "WhoName": "the QA group",
		"WhoRows": []whoRow{{Who: "Super admins", Level: "Manage", Via: "everything", People: 4}}, "Now": time.Now(),
		"Groups": []db.Group{{ID: g, Name: "QA"}}, "Venues": []db.Restaurant{},
	}, `Who can act on the QA group`, `Super admins`)
}

func TestPersonPageRenders(t *testing.T) {
	email := "hafiz@aioapp.com"
	u := &db.User{ID: uuid.New(), Username: "hafiz", FirstName: "Hafiz", Role: "engineer", Email: &email}
	g := uuid.New()
	grants := []personGrant{
		{AccessLevel: db.AccessLevel{ID: uuid.New(), ScopeType: "state", State: db.StateLab, Level: "operate"}, From: "Engineer"},
		{AccessLevel: db.AccessLevel{ID: uuid.New(), ScopeType: "group", ScopeID: &g, ScopeName: "Test Cycles", Level: "operate"}},
	}
	renderPeople(t, peopleTmpl(t, "person.html", "_users_tabs.html"), "person.html", map[string]any{
		"Role": "admin", "UsersTab": "people", "User": u, "Sentence": "Hafiz can look at every device.", "Grants": grants,
		"CanGive": true, "Groups": []db.Group{}, "Venues": []db.Restaurant{}, "Now": time.Now(), "Back": "/users/x/profile",
		"FirstName": "Hafiz", "Assignable": []string{"engineer", "viewer"}, "CanDelete": true, "IsSuperAdmin": true,
	},
		`Hafiz can look at every device.`, `Engineer role`, `their own`, `the Test Cycles group`, `Give Hafiz access`,
		`action="/users/access/levels"`, `/users/access/levels/`, `name="subject" value="hafiz"`)
}

func TestActivityPageRenders(t *testing.T) {
	renderPeople(t, peopleTmpl(t, "activity.html", "_users_tabs.html"), "activity.html", map[string]any{
		"Role": "admin", "UsersTab": "activity", "Entries": []db.AuditEntry{{Actor: "Hafiz", Action: "device.reboot", CreatedAt: time.Now(), DeviceSerial: "AT070AABU00089"}},
		"Total": 1, "Page": 1, "TotalPages": 2, "Kinds": activityKindOrder, "KindCount": map[string]int{"": 1, "devices": 1},
		"Days": "7", "Kind": "", "BaseQS": template.URL("days=7"),
	}, `>Everything</span>`, `kind=devices`, `AT070AABU00089`, `Last 7 days`, `Next`)
}

func TestActivityKinds(t *testing.T) {
	for a, want := range map[string]string{
		"device.reboot": "devices", "command.send": "devices", "group.delete": "places", "restaurant.assign": "places",
		"access.give": "people", "user.role": "people", "legacy_ota.push": "updates", "release.publish": "updates",
		"auth.login": "signin", "settings.ota_config": "settings",
	} {
		if got := activityKind(a); got != want {
			t.Errorf("activityKind(%q) = %q, want %q", a, got, want)
		}
	}
}
