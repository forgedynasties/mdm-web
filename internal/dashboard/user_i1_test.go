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

// i1Sample is Muhammad B. on 21 Oct 2026: a Viewer limited to one device by the "Look
// only" profile, email verified, last active two days ago.
func i1Sample(now time.Time) i1Inputs {
	email := "muhammad@aioapp.com"
	verified := now.AddDate(0, -1, 0)
	last := now.AddDate(0, 0, -2)
	pid := uuid.New()
	u := &db.User{ID: uuid.New(), Username: email, Email: &email, EmailVerifiedAt: &verified, FirstName: "Muhammad", LastName: "B.",
		Role: "viewer", CreatedAt: time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC)}
	return i1Inputs{
		User: u, Now: now, LastAt: &last, Visible: 1, Venues: 0, FleetSize: 216,
		Simple:  simpleAccess{OK: true, Places: []scopeOption{{Type: "device", Name: "AT070AABU00231"}}, Do: "profile:" + pid.String()},
		Choices: []simpleProfileView{{Value: "profile:" + pid.String(), Name: "Look only", Fits: true}, {Value: "role", Name: "Everything a viewer can", Fits: true}},
		Grants:  []grantView{{AccessGrant: db.AccessGrant{Effect: "allow", Actions: []string{"view"}}}},
		Week:    db.ActorWeek{Actions: 3, PageViews: 145, PrevActions: 1},
	}
}

func TestUserI1Builder(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	v := buildUserI1(i1Sample(now))

	b := v.Banner
	if b.Tone != "calm" || b.Headline != "Sees 1 device" || len(b.Todos) != 0 {
		t.Fatalf("banner = %+v", b)
	}
	if b.Line != "Access comes from the “Look only” profile. Email verified, signs in normally." {
		t.Errorf("line = %q", b.Line)
	}
	if len(v.Answers) != 3 || v.Answers[0].A != "1 device" || v.Answers[0].Chips[0] != "AT070AABU00231" ||
		v.Answers[1].A != "Look only · profile" || v.Answers[2].A != "No end date · no note" {
		t.Errorf("answers = %+v", v.Answers)
	}
	if c := v.Cards[0]; c.A.Value != "3" || c.A.Delta != "▲ 2 vs last week" || c.B.Value != "145" {
		t.Errorf("actions card = %+v", c)
	}
	if c := v.Cards[1]; c.A.Value != "1" || c.A.Label != "of 216" || c.B.Label != "venues" {
		t.Errorf("devices card = %+v", c)
	}
	if c := v.Cards[2]; c.A.Value != "1" || c.A.Label != "rule · profile" || c.B.Label != "never ends" {
		t.Errorf("access card = %+v", c)
	}
	if v.Status != "Joined 22 Sep 2026 · last active 2d ago · 3 actions this week" {
		t.Errorf("status = %q", v.Status)
	}
}

func TestUserI1Todos(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	in := i1Sample(now)
	in.User.EmailVerifiedAt = nil
	old := now.AddDate(0, 0, -45)
	in.LastAt = &old
	soon := now.Add(48 * time.Hour)
	in.Grants = append(in.Grants, grantView{AccessGrant: db.AccessGrant{Effect: "allow", Actions: []string{"shell"}, ExpiresAt: &soon}})
	in.ActorAdmin = true
	v := buildUserI1(in)
	b := v.Banner
	if b.Headline != "4 things to look at" || len(b.Todos) != 3 || b.More != 1 {
		t.Fatalf("banner = %+v", b)
	}
	if b.Todos[0].Post != "/users/"+in.User.ID.String()+"/verify-email" || b.Todos[0].Action != "Verify" {
		t.Errorf("verify todo = %+v", b.Todos[0])
	}
	if !strings.HasPrefix(b.Todos[1].Title, "Hasn't done anything in 30 days") || b.Todos[2].Title != "A rule gives more than a sales person gets" {
		t.Errorf("todos = %+v", b.Todos)
	}
	if c := v.Cards[2]; c.B.Value != "23 Oct" || c.B.Label != "first one ends" {
		t.Errorf("access card = %+v", c)
	}

	// A super admin: no rules, no editor, two answers.
	in = i1Sample(now)
	in.User.Role = "admin"
	v = buildUserI1(in)
	if len(v.Answers) != 2 || v.Answers[0].A != "Every device" || v.Answers[1].A != "Everything" || v.Banner.Headline != "Sees and can do everything" {
		t.Errorf("admin = %+v / %+v", v.Answers, v.Banner)
	}
}

func TestUserI1Renders(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := template.FuncMap{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(src), -1) {
		funcs[m[1]] = func(...any) any { return "" }
	}
	funcs["roleLabel"] = roleLabel
	funcs["isCustomer"] = func(string) bool { return false }
	funcs["hasStr"] = func(xs []string, x string) bool { return containsStr(xs, x) }
	funcs["canCreateUsers"] = func(string) bool { return true }
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}{{define "ubub"}}{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("../../templates/user_access.html"))

	now := time.Now()
	in := i1Sample(now)
	u := in.User
	data := map[string]any{
		"Role": "admin", "User": u, "I1": buildUserI1(in), "Open": "access", "Simple": in.Simple, "SimpleHas": map[string]bool{},
		"Choices": in.Choices, "Assignable": []string{"viewer", "operator"}, "Bubble": struct{ URL, Initial string }{"", "M"},
		"CanEditAvatar": true, "CanDelete": true, "ActorIsAdmin": true, "HasBase": true, "Policy": db.AccessPolicy{},
		"Summary": []string{"Sees 1 device."}, "Stats": &db.UserStats{Actions: 3}, "CanSeeActivity": true,
		"Recent": []db.AuditEntry{{Action: "device.reboot", Target: "AT070AABU00231", CreatedAt: now}},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "user_access.html", data); err != nil {
		t.Fatalf("user_access.html failed to render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`<h2>Sees 1 device</h2>`, `id="ua-access"`, `Which devices can they see?`, `<span class="i1-chip">AT070AABU00231</span>`,
		`id="i1-edit"`, `id="ua-simple"`, `id="ua-pv"`, `id="ua-adv"`, `class="ua-words"`, `id="ua-cq"`, `id="ua-cres-fleet"`,
		`id="i1-name"`, `id="i1-role"`, `id="i1-password"`, `id="i1-picture"`, `id="i1-delete"`,
		`action="/users/` + u.ID.String() + `/role"`, `value="/users/` + u.ID.String() + `/profile?open=role"`,
		`Recent activity`, `/static/js/user_access.js`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("person page is missing %q", want)
		}
	}
	if strings.Contains(out, `<div class="i1-edit" id="i1-edit" hidden`) {
		t.Error("?open=access left the editor closed")
	}
	if strings.Contains(out, "/manage") {
		t.Error("the page still points at the old Manage page")
	}
	if p := os.Getenv("I1_RENDER_OUT"); p != "" {
		os.WriteFile(p, buf.Bytes(), 0o644)
	}
}
