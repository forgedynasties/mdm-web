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

func TestRuleCovers(t *testing.T) {
	rest, grp := uuid.New(), uuid.New()
	d := db.Device{ID: uuid.New(), SerialNumber: "T7-1"}
	sc := db.DeviceScope{RestaurantID: &rest, Groups: []uuid.UUID{grp}}
	for _, c := range []struct {
		r    db.KioskPolicy
		want bool
	}{
		{db.KioskPolicy{TargetType: "all"}, true},
		{db.KioskPolicy{TargetType: "restaurant", TargetID: &rest}, true},
		{db.KioskPolicy{TargetType: "restaurant", TargetID: &grp}, false},
		{db.KioskPolicy{TargetType: "group", TargetID: &grp}, true},
		{db.KioskPolicy{TargetType: "device", TargetSerial: "T7-1"}, true},
		{db.KioskPolicy{TargetType: "device", TargetSerial: "T7-2"}, false},
	} {
		if got := ruleCovers(c.r, d, sc); got != c.want {
			t.Errorf("%s: got %v", c.r.TargetType, got)
		}
	}
}

func TestKioskDeviceStatus(t *testing.T) {
	rule := &db.KioskPolicy{ID: uuid.New(), KioskPackage: "aio.app.nugget"}
	other := uuid.New()
	now := time.Now()
	before := now.Add(-time.Hour)
	base := kioskDevice{Rule: rule, CanLock: true, HasApp: true,
		State: db.KioskState{Enabled: true, Rule: &rule.ID, ConfigAt: &before, LastSeen: now}}
	cases := map[string]func(k *kioskDevice){
		"locked":   func(k *kioskDevice) {},
		"waiting":  func(k *kioskDevice) { k.State.LastSeen = before.Add(-time.Minute) },
		"exited":   func(k *kioskDevice) { k.State.Suspended = true },
		"missing":  func(k *kioskDevice) { k.HasApp = false },
		"cant":     func(k *kioskDevice) { k.CanLock = false },
		"override": func(k *kioskDevice) { k.State.Override = true },
	}
	for want, f := range cases {
		k := base
		f(&k)
		if got := k.Status(); got != want {
			t.Errorf("want %s, got %s", want, got)
		}
	}
	k := base
	k.State.Rule = &other
	if k.Status() != "waiting" {
		t.Error("a device still on another rule's config is waiting")
	}
	k = base
	exitAt := time.Now()
	k.State.ExitedAt = &exitAt
	if k.Status() != "exited" {
		t.Error("a device taken out on site is exited, whatever its config says")
	}
	k = base
	k.Rule = nil
	if k.Status() != "" {
		t.Error("no rule, no status")
	}
}

func kioskTmpl(t *testing.T, file string) *template.Template {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := template.FuncMap{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(src), -1) {
		funcs[m[1]] = func(...any) any { return "" }
	}
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}`))
	return template.Must(tmpl.ParseFiles("../../templates/" + file))
}

func TestKioskPagesRender(t *testing.T) {
	id := uuid.New()
	rest := uuid.New()
	rules := []kioskRuleView{
		{KioskPolicy: db.KioskPolicy{ID: id, Name: "Flights Vegas · Nugget", KioskPackage: "aio.app.nugget.uatv2", TargetType: "restaurant", TargetID: &rest, OfflineExit: true},
			Pos: 1, First: true, Last: true, AppName: "Nugget-uatv2", TargetLabel: "Flights Vegas (restaurant)", TargetHref: "/devices?view=restaurant&id=" + rest.String(),
			Devices: 20, Locked: 16, Waiting: 4, Exited: 1},
	}
	exits := []kioskExitView{{KioskExit: db.KioskExit{DeviceID: uuid.New(), Serial: "AT070AABU00875", Restaurant: "Flights Vegas",
		Package: "aio.app.nugget.uatv2", RuleID: &id, RuleName: "Flights Vegas · Nugget"}, AppName: "Nugget-uatv2"}}
	var buf bytes.Buffer
	if err := kioskTmpl(t, "manage.html").ExecuteTemplate(&buf, "manage.html", map[string]any{
		"Rules": rules, "Locked": 16, "NotCovered": 161, "ByHand": 0, "CanEdit": true, "Exits": exits,
	}); err != nil {
		t.Fatalf("manage.html: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"16 locked", "4 waiting", "1 taken out on site", "1 device was taken out of kiosk on site",
		"/devices/AT070AABU00875/kiosk/relock", "/devices/AT070AABU00875/kiosk/leave-out", `by the rule "Flights Vegas · Nugget"`, "/manage/policies/" + id.String() + "/move", "161 devices no rule covers"} {
		if !strings.Contains(out, want) {
			t.Errorf("list missing %q", want)
		}
	}
	if strings.Contains(out, "exit code on") {
		t.Error("the rules list still says \"exit code on\"")
	}
	buf.Reset()
	p := rules[0].KioskPolicy
	if err := kioskTmpl(t, "manage_policy_form.html").ExecuteTemplate(&buf, "manage_policy_form.html", map[string]any{
		"Policy": p, "Restaurants": []db.Restaurant{{ID: rest, Name: "Flights Vegas", DeviceCount: 20}},
		"Groups": []db.Group{{ID: uuid.New(), Name: "SJ 3.0", DeviceCount: 20}}, "Serials": []string{"T7-1"},
		"FleetTotal": 181, "AllowAll": true, "CanEdit": true,
	}); err != nil {
		t.Fatalf("form: %v", err)
	}
	out = buf.String()
	for _, want := range []string{`value="aio.app.nugget.uatv2"`, `data-id="` + rest.String() + `"`, "Power five times", "stays out of kiosk until someone locks it again", "Every device"} {
		if !strings.Contains(out, want) {
			t.Errorf("form missing %q", want)
		}
	}
}
