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

// ac1Sample: three devices (two at one venue), a super admin, an operator a rule gives
// remote control everywhere, a viewer limited to one device, and a staff account with no
// venue.
func ac1Sample(now time.Time) ac1Inputs {
	venue := uuid.New()
	d1, d2, d3 := uuid.New(), uuid.New(), uuid.New()
	scopes := map[uuid.UUID]db.DeviceScope{d1: {RestaurantID: &venue}, d2: {RestaurantID: &venue}, d3: {}}
	person := func(name, role string, pol db.AccessPolicy) ac1Person {
		u := db.User{ID: uuid.New(), Username: strings.ToLower(name), FirstName: name, Role: role}
		a := &access{role: role, pol: pol, scopes: scopes}
		a.once.Do(func() {})
		return ac1Person{User: u, Pol: pol, Acc: a, Simple: deriveSimple(role, pol, nil, scopes), CanEdit: role != "admin"}
	}
	soon := now.Add(3 * 24 * time.Hour)
	remote := db.AccessPolicy{Grants: []db.AccessGrant{{Effect: "allow", ScopeType: "all", Actions: []string{"remote"}, ExpiresAt: &soon}}}
	one := db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{{Effect: "allow", ScopeType: "device", ScopeID: &d3, ScopeName: "AT070AABU00231", Actions: []string{"view"}}}}
	return ac1Inputs{
		People: []ac1Person{
			person("Ali", "admin", db.AccessPolicy{}), person("Shahrukh", "operator", remote),
			person("Muhammad", "viewer", one), person("Sana", "staff", db.AccessPolicy{}),
		},
		Profiles:  []db.AccessProfile{{ID: uuid.New(), Name: "Look only", Actions: []string{"view"}, Users: []string{"muhammad"}}},
		FleetSize: 3, DeviceIDs: []uuid.UUID{d1, d2, d3}, Now: now,
	}
}

func ac1CellOf(r ac1Row, key string) ac1Cell {
	for _, c := range r.Cells {
		if c.Key == key {
			return c
		}
	}
	return ac1Cell{}
}

func TestAccessAC1Builder(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	v := buildAccessAC1(ac1Sample(now))

	names := []string{}
	for _, r := range v.Rows {
		names = append(names, r.User.FirstName)
	}
	// Extras first, then the limited ones, then by role.
	if strings.Join(names, ",") != "Shahrukh,Muhammad,Sana,Ali" {
		t.Errorf("order = %v", names)
	}
	op, vw, st, ad := v.Rows[0], v.Rows[1], v.Rows[2], v.Rows[3]
	if c := ac1CellOf(op, "remote"); c.State != "all" || !c.Extra {
		t.Errorf("operator remote = %+v", c)
	}
	if c := ac1CellOf(op, "shell"); c.State != "above" {
		t.Errorf("operator shell = %+v, want above the role", c)
	}
	if c := ac1CellOf(op, "reboot"); c.State != "all" || c.Extra {
		t.Errorf("operator reboot = %+v", c)
	}
	if c := ac1CellOf(vw, "view"); c.State != "part" || c.N != 1 {
		t.Errorf("viewer see = %+v", c)
	}
	if c := ac1CellOf(vw, "reboot"); c.State != "above" {
		t.Errorf("viewer reboot = %+v", c)
	}
	if vw.Sees != "1 device" || vw.Whole || !op.Ending || !op.Sensitive {
		t.Errorf("rows = %+v / %+v", vw, op)
	}
	if !st.NoVenue || st.Devices != 0 {
		t.Errorf("staff = %+v", st)
	}
	if c := ac1CellOf(ad, "shell"); c.State != "all" || ad.EditJSON != "" {
		t.Errorf("super admin shell = %+v, edit %q", c, ad.EditJSON)
	}
	var e ac1Edit
	if err := json.Unmarshal([]byte(vw.EditJSON), &e); err != nil || !e.OK || e.Every || len(e.Places) != 1 || e.Places[0].Name != "AT070AABU00231" {
		t.Errorf("viewer edit = %+v (%v)", e, err)
	}

	b := v.Banner
	// A lone remote-control rule is a setup the two questions can't show, too.
	if b.Headline != "4 things to look at" || len(b.Todos) != 3 || b.More != 1 ||
		b.Todos[0].Title != "1 person has remote control or firmware beyond their role" ||
		b.Todos[1].Title != "1 person's access ends this week" ||
		b.Todos[2].Title != "1 setup the two questions can't show" {
		t.Errorf("banner = %+v", b)
	}
	if v.Cards[0].A.Value != "2" || v.Cards[1].A.Value != "2" || v.Cards[2].A.Value != "1" || v.Cards[2].A.Tone != "bad" {
		t.Errorf("cards = %+v", v.Cards)
	}
	if v.Rail.Pct != 50 || len(v.Rail.Profiles) != 1 || v.Rail.Profiles[0].Users != 1 || v.Rail.Profiles[0].Desc != "See device" {
		t.Errorf("rail = %+v", v.Rail)
	}
	if strings.Join(v.Roles, ",") != "admin,operator,viewer,staff" {
		t.Errorf("roles = %v", v.Roles)
	}
}

func TestAccessAC1Renders(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := template.FuncMap{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(src), -1) {
		funcs[m[1]] = func(...any) any { return "" }
	}
	funcs["roleLabel"] = roleLabel
	funcs["isCustomer"] = roleIsCustomer
	funcs["hasStr"] = func(xs []string, x string) bool { return containsStr(xs, x) }
	funcs["add"] = func(a, b int) int { return a + b }
	funcs["hasPrefix"] = strings.HasPrefix
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}{{define "ubub"}}{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("../../templates/users_access.html"))
	v := buildAccessAC1(ac1Sample(time.Now()))
	data := map[string]any{"AC1": v, "Show": "extras", "Assignable": []string{"viewer", "support", "operator", "staff"}, "ActivePath": "/users/access"}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "users_access.html", data); err != nil {
		t.Fatalf("users_access.html failed to render: %v", err)
	}
	out := buf.String()
	op := v.Rows[0].User.ID.String()
	for _, want := range []string{
		`<h2>4 things to look at</h2>`, `id="ac1-matrix" data-show="extras"`, `>Remote</th>`, `>Shell</th>`,
		`action="/users/` + op + `/role"`, `name="redirect" value="/users/access"`, `onchange="ac1Role(this)"`,
		`class="ac1-d all extra"`, `class="ac1-dash"`, `data-flags=" extras ending odd"`, `data-flags=" novenue"`,
		`<tr class="ac1-edr" hidden data-for="` + op + `">`, `class="ac1-data"`, `"choices":`,
		`<span><b>50%</b><small>whole fleet</small></span>`, `Look only`, `onsubmit="return ac1Who(this)"`,
		`data-ac1-set="role" data-val="staff"`, `href="/users/access/profiles"`, `/static/js/users/access_ac1.js`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("access page is missing %q", want)
		}
	}
	if strings.Contains(out, "/manage") {
		t.Error("the page still points at the old Manage page")
	}
	if p := os.Getenv("AC1_RENDER_OUT"); p != "" {
		os.WriteFile(p, buf.Bytes(), 0o644)
	}
}
