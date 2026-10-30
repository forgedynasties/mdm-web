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

func TestGroupsG1(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	groups := []db.Group{
		{ID: a, Name: "Test Lab units", DeviceCount: 24, CreatedAt: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)},
		{ID: b, Name: "SJ Ch B1", CreatedAt: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)},
		{ID: c, Name: "SJ 3.0", DeviceCount: 20, CreatedAt: time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)},
	}
	health := map[uuid.UUID]db.GroupHealth{
		a: {GroupID: a, DeviceCount: 24, OfflineCount: 2, Score: 87},
		c: {GroupID: c, DeviceCount: 20, OfflineCount: 15, OpenCritical: 2, OpenWarning: 2, DeployedCount: 2, Score: 71},
	}
	p := buildGroupsG1(groups, health)
	var order []string
	for _, r := range p.Rows {
		order = append(order, r.G.Name)
	}
	if strings.Join(order, "|") != "SJ 3.0|Test Lab units|SJ Ch B1" {
		t.Errorf("worst first, empty last: %v", order)
	}
	if p.Look != 1 || p.Critical != 1 || p.Offline != 2 || p.Empty != 1 || p.AtVenue != 1 || p.Devices != 44 || p.OfflineDevices != 17 {
		t.Errorf("counts = %+v", p)
	}
	if r := p.Rows[0]; r.Flags != "look crit offline venue" || r.Online != 5 || r.Class != "warn" || len(r.Why) == 0 {
		t.Errorf("SJ 3.0 row = %+v", r)
	}

	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := template.FuncMap{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(src), -1) {
		funcs[m[1]] = func(...any) any { return "" }
	}
	funcs["canAdminOrOperator"] = func(string) bool { return true }
	funcs["venueIcon"] = func(id uuid.UUID, name, size string) template.HTML { return `<span class="vicon"></span>` }
	tm := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}`))
	tm = template.Must(tm.ParseFiles("../../templates/groups.html", "../../templates/restaurants.html"))
	var buf bytes.Buffer
	if err := tm.ExecuteTemplate(&buf, "groups.html", map[string]any{"Role": "admin", "Groups": groups, "G1": p}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`id="page-island"`, `4 groups`[1:], `1 needs a look`, `data-cl data-one="group"`, `data-clf="crit"`, `data-clc="venue"`,
		`data-flags="look crit offline venue"`, `class="cc-tag bad">2 critical`, `data-cl-dlg="cl-delete" data-name="SJ 3.0"`,
		`id="cl-delete" data-confirm="1"`, `name="confirm_name"`, `name="redirect" value="/groups"`, `data-cls="created"`,
		`16 Sep 2026`, `/static/js/coll_list.js`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("groups page is missing %q", want)
		}
	}
	if strings.Contains(out, `fl-del`) {
		t.Error("the one-click delete × is gone")
	}
}
