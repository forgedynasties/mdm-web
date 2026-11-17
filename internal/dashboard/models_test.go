package dashboard

import (
	"bytes"
	"html/template"
	"os"
	"regexp"
	"strings"
	"testing"

	"mdm/internal/db"
	"mdm/internal/product"
)

// The Models page renders a Standard model with several roles (editable), and a Firmware
// model with its firmware section; the rail lists every model with its kind and roles.
func TestModelsRenders(t *testing.T) {
	src, _ := os.ReadFile("handlers.go")
	funcs := template.FuncMap{}
	for _, re := range []*regexp.Regexp{regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`), regexp.MustCompile(`funcMap\["([a-zA-Z][a-zA-Z0-9]*)"\]`)} {
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			funcs[m[1]] = func(...any) any { return "" }
		}
	}
	tmpl := template.Must(template.New("").Funcs(funcs).ParseGlob("../../templates/*.html"))
	d2s := modelRow{Key: "d2s_kds_stgl", Label: "Sunmi D2s", Roles: []string{product.ClassKDS, product.ClassKiosk}, Devices: 57, Online: 40}
	t7 := modelRow{Key: "t7", Label: "T7", Firmware: true, Roles: []string{product.ClassT7}, Devices: 76}
	a16 := modelRow{Key: "a16nsxx", Label: "Galaxy A16", Devices: 5}
	rows := []modelRow{t7, d2s, a16}
	for _, tc := range []struct {
		pane modelPane
		want []string
	}{
		{modelPane{M: d2s, CanEdit: true, AllRoles: []string{product.ClassPOS, product.ClassKDS, product.ClassKiosk},
			Moving: map[string]int{product.ClassKDS: 1}, ByRole: []roleCount{{product.ClassKDS, 56}, {product.ClassKiosk, 1}},
			Apps: []modelApp{{Role: product.ClassKDS, Label: "Kitchen display", Base: "aio.app.kds", Have: 50, Of: 56}}},
			[]string{`action="/models/d2s_kds_stgl/roles"`, `value="several" checked`, "aio.app.kds", "50 of 56", "Show its devices"}},
		{modelPane{M: t7, Fw: &pipeModel{Key: "t7", Label: "T7", Mode: "prod", Channel: &db.FirmwareChannel{Version: "v2.1.031"}, Newer: []db.Release{{ID: 9, Version: "v2.1.032"}}},
			FwRoleKey: product.ClassT7, CanFw: true},
			[]string{"set by its firmware", "v2.1.031", "v2.1.032", `name="back" value="/models?model=t7"`}},
	} {
		var b bytes.Buffer
		data := map[string]any{"AlertsOpenCount": 0, "Title": "Models", "Models": rows, "Total": 3, "NeedsN": 1, "SeveralN": 1, "Pane": tc.pane}
		if err := tmpl.ExecuteTemplate(&b, "models.html", data); err != nil {
			t.Fatalf("%s: %v", tc.pane.M.Key, err)
		}
		out := b.String()
		for _, w := range append(tc.want, "Galaxy A16", "Needs a role", "Firmware", "Standard", `data-rail-search="Search models"`, `hx-select="#md-pane"`) {
			if !strings.Contains(out, w) {
				t.Errorf("%s: missing %q", tc.pane.M.Key, w)
			}
		}
	}
	if (modelRow{}).RoleText() != "Needs a role" || d2s.Kind() != "Standard" || t7.Kind() != "Firmware" {
		t.Error("RoleText / Kind")
	}
}
