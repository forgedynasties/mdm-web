package dashboard

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// The Host app row lists each product with its own channel menu (H1, 27 Oct 2026).
func TestVenueHostAppRowRenders(t *testing.T) {
	tmpl := template.Must(hubTmpl(t, "_venue_profile.html").Parse(`{{define "venue-wifi-row"}}{{end}}`))
	hv := venueHostAppView{Total: 6, Summary: "TSAI on UAT", Sub: "others prod", Products: []venueHostProduct{
		{Role: "t7", Label: "Tableside AI", Base: "aio.app.nugget", Channel: "uatv2", ChLabel: "UAT", Target: "aio.app.nugget.uatv2", Total: 5, Have: 4,
			Opts: []venueHostOpt{{Key: "", Label: "Prod", Have: 1}, {Key: "uatv2", Label: "UAT", Have: 4, On: true}}},
		{Role: "dongle", Label: "Dongle / TV box", Base: "aio.app.menuboards", ChLabel: "Prod", Target: "aio.app.menuboards", Total: 1, NoBuild: true,
			Opts: []venueHostOpt{{Key: "", Label: "Prod", On: true, NoBuild: true}}},
	}}
	data := map[string]any{"Role": "admin", "Restaurant": &db.Restaurant{ID: uuid.New(), Name: "AIO Lab"}, "HostApp": hv}
	var b bytes.Buffer
	err := tmpl.ExecuteTemplate(&b, "venue-settings", data)
	out := b.String()
	if !strings.Contains(out, `id="vn-hostapp"`) {
		t.Fatalf("no host app row (err %v)", err)
	}
	for _, want := range []string{`TSAI on UAT <small>· others prod</small>`, `name="ch_t7"`, `<option value="uatv2" selected>UAT · .uatv2 (4 of 5)`,
		`name="ch_dongle"`, `no build in the library yet`, `aio.app.nugget.uatv2</code> · 4 of 5 have it`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}
