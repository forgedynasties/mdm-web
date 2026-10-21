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

// p1Sample is the live account mix of 21 Oct 2026 in small: two super admins active
// today, an operator with a shell rule, three viewers nobody has used for a month, one
// invited and one brand new, and a customer owner.
func p1Sample(now time.Time) p1Inputs {
	ago := func(d int) *time.Time { t := now.AddDate(0, 0, -d); return &t }
	row := func(name, role string, last *time.Time, created int) userRow {
		u := db.User{ID: uuid.New(), Username: strings.ToLower(name) + "@aioapp.com", FirstName: name, Role: role, CreatedAt: now.AddDate(0, 0, -created)}
		r := userRow{User: u, LastAt: last, Rank: rank(role)}
		r.Activity = p1Activity(r, now)
		return r
	}
	team := []userRow{
		row("Ali", "admin", ago(0), 130), row("Talmiz", "admin", ago(1), 110),
		row("Shahrukh", "operator", ago(5), 50), row("Humayun", "viewer", ago(31), 30),
		row("Adil", "viewer", ago(40), 60), row("Zoya", "viewer", nil, 45), row("Neo", "viewer", nil, 3),
	}
	team[2].Sensitive, team[2].Rules = []string{"shell"}, 1
	inv := row("Iqra", "viewer", nil, 2)
	inv.Invited = ago(2)
	inv.Activity = p1Activity(inv, now)
	team = append(team, inv)
	owner := row("Aaron", "owner", ago(3), 20)
	owner.Rules = 1
	var days []db.PeopleDay
	for i := 7; i >= 0; i-- {
		days = append(days, db.PeopleDay{Day: now.AddDate(0, 0, -i), Actions: 10 * (8 - i), People: 2})
	}
	return p1Inputs{Team: team, Customers: []userRow{owner}, Waiting: 1,
		Orphans: []db.OrphanActor{{Username: "user1@aioapp.com", Rows: 133}}, Days: days, Now: now}
}

func TestUsersP1Builder(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	v := buildUsersP1(p1Sample(now))

	b := v.Banner
	if b.Kicker != "People · 9 accounts" || b.Headline != "5 things to look at" || len(b.Todos) != 3 || b.More != 2 {
		t.Fatalf("banner = %+v", b)
	}
	if b.Todos[0].Href != "#waiting-signups" || b.Todos[1].Title != "1 invite not used yet" ||
		b.Todos[2].Title != "3 accounts haven't done anything in 30 days" {
		t.Errorf("todos = %+v", b.Todos)
	}
	if !strings.HasPrefix(b.Todos[2].Sub, "All viewers, last active 11 Sep–20 Sep or never") {
		t.Errorf("quiet sub = %q", b.Todos[2].Sub)
	}
	if b.Line != "" {
		t.Errorf("line = %q, want none while sign-ups and invites wait", b.Line)
	}

	if c := v.Cards[0]; c.A.Value != "8" || c.A.Delta != "▲ 3 joined in Oct" || c.B.Value != "1" || c.B.Label != "customer" {
		t.Errorf("people card = %+v", c)
	}
	// Active this week: Ali, Talmiz, Shahrukh, Aaron. Actions: the last 7 days of 10..80.
	if c := v.Cards[1]; c.A.Value != "4" || c.A.Label != "of 9 did something" || c.B.Value != "350" || c.LineA == "" {
		t.Errorf("active card = %+v", c)
	}
	if c := v.Cards[2]; c.A.Value != "2" || c.B.Value != "1" || c.B.Tone != "bad" {
		t.Errorf("access card = %+v", c)
	}
	r := v.Rail
	if r.Pct != 44 || r.Active != 4 || len(r.Bars) != 8 || !r.Bars[7].Cur || r.Bars[7].Pct != 100 {
		t.Errorf("rail = %+v", r)
	}
	want := []string{"Super admin", "Operator", "Viewer", "Owner · customer"}
	if len(r.Roles) != len(want) {
		t.Fatalf("roles = %+v", r.Roles)
	}
	for i, w := range want {
		if r.Roles[i].Label != w {
			t.Errorf("role %d = %q, want %q", i, r.Roles[i].Label, w)
		}
	}
	if r.Roles[2].N != 5 || r.Roles[2].Pct != 100 {
		t.Errorf("viewers = %+v", r.Roles[2])
	}
}

func TestUsersP1Calm(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	in := p1Sample(now)
	in.Team = in.Team[:2]
	in.Customers, in.Orphans, in.Waiting = nil, nil, 0
	v := buildUsersP1(in)
	if v.Banner.Tone != "calm" || v.Banner.Headline != "Every account is in order" || v.Banner.Line == "" {
		t.Errorf("calm banner = %+v", v.Banner)
	}
}

func TestUsersP1Renders(t *testing.T) {
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
	funcs["roleLabel"] = roleLabel
	funcs["deref"] = func(t *time.Time) time.Time { return *t }
	funcs["canCreateUsers"] = func(string) bool { return true }
	funcs["isCustomer"] = func(string) bool { return false }
	funcs["hasPrefix"] = strings.HasPrefix
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}{{define "ubub"}}{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("../../templates/users.html"))

	now := time.Now()
	in := p1Sample(now)
	data := map[string]any{
		"Role": "admin", "ActivePath": "/users", "Team": in.Team, "Customers": in.Customers, "Tab": "team", "Activity": "quiet",
		"TeamRoles": []string{"viewer", "operator", "admin"}, "CustomerRoles": []string{"owner", "staff"},
		"Orphans": in.Orphans, "Waiting": []db.User{{ID: uuid.New(), Username: "new@x", Role: "viewer", CreatedAt: now}},
		"P1": buildUsersP1(in), "Invites": 1, "Assignable": []string{"viewer"},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "users.html", data); err != nil {
		t.Fatalf("users.html failed to render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`<h2>5 things to look at</h2>`, `href="#waiting-signups">Verify</a>`, `href="/users?activity=quiet">Review</a>`,
		`id="waiting-signups"`, `id="unlinked"`, `data-activity="quiet"`, `data-act="quiet"`, `data-up-act="invited"`,
		`data-up-role="operator"`, `>Active this week</a>`, `<span><b>44%</b><small>active</small></span>`,
		`Owner · customer`, `action="/users/merge"`, `id="invite-modal"`, `/static/js/oc-menus.js`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("users page is missing %q", want)
		}
	}
	if p := os.Getenv("P1_RENDER_OUT"); p != "" {
		os.WriteFile(p, buf.Bytes(), 0o644)
	}
}
