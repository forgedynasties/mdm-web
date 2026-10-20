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

	"mdm/internal/db"

	"github.com/google/uuid"
)

// The fleet table is rendered server-side on four pages, so a bad call inside it fails
// at render time — after the deploy, on a page someone is looking at. This renders it
// the way each page does with a synthetic device and asserts the parts that matter.
func TestFleetCardLayoutsRender(t *testing.T) {
	// Every function the dashboard registers, stubbed generically. Hand-listing them is
	// whack-a-mole: the card markup calls a dozen helpers and the list drifts as the page
	// grows, each miss costing another run. The two the test asserts on are overridden
	// below with their real implementations.
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatalf("read handlers.go: %v", err)
	}
	funcs := template.FuncMap{}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`),
		regexp.MustCompile(`funcMap\["([a-zA-Z][a-zA-Z0-9]*)"\]`),
	} {
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			funcs[m[1]] = func(...any) any { return "" }
		}
	}
	if len(funcs) < 40 {
		t.Fatalf("stubbed only %d helpers — extraction no longer matches how funcMap is written", len(funcs))
	}
	// String-valued helpers dominate, so the generic stub returns "" — enough for the
	// `ne x ""` comparisons the markup is full of. The numeric ones are not interchangeable
	// with it (`ge "" 80` is a type error), so they get real signatures. Adding one here is
	// the test telling you a helper changed shape, which is the point.
	for name, fn := range map[string]any{
		"ramPct":       func(string) int { return 0 },
		"batteryFillW": func(int) int { return 0 },
	} {
		funcs[name] = fn
	}
	// The sortable headers are a sub-template fed through dict.
	funcs["dict"] = func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	funcs["serialHead"] = func(s string) string { return s[:len(s)-serialTailLen(s)] }
	funcs["serialTail"] = func(s string) string { return s[len(s)-serialTailLen(s):] }

	tmpl := template.Must(template.New("").Funcs(funcs).ParseGlob("../../templates/devices.html"))

	// ProductLabel/KindLabel/IsDPC/HasBattery are methods on Device, computed from these
	// fields — setting them would not compile, and stubbing them would test nothing.
	dev := db.Device{
		ID:           uuid.New(),
		SerialNumber: "AT070AABU00281",
		Product:      "t7",
		AgentKind:    "firmware",
		BuildID:      "v2.1.017",
		BatteryPct:   80,
		LastSeenAt:   time.Now(),
		LatestExtra:  json.RawMessage(`{}`),
	}

	// The fleet table (R1, 20 Oct) renders the same way on every page; the Fleet roster
	// adds the sortable headers and the select-all box, a venue page drops Venue.
	for _, tc := range []struct {
		name string
		data map[string]any
	}{
		{"fleet", map[string]any{"Selectable": true, "SelectAll": true, "FilterTotal": 1, "Sortable": true, "Sort": "battery", "SortDir": "asc"}},
		{"venue", map[string]any{"RemovePath": "/restaurants/x/devices/", "HideVenue": true}},
		{"group", map[string]any{"RemovePath": "/groups/x/devices/", "Selectable": true}},
		{"production", map[string]any{}},
	} {
		var buf bytes.Buffer
		data := map[string]any{
			"Devices":             []db.Device{dev},
			"Online":              map[uuid.UUID]bool{dev.ID: true},
			"ActiveThresholdSecs": 300,
		}
		for k, v := range tc.data {
			data[k] = v
		}
		if err := tmpl.ExecuteTemplate(&buf, "device-cards", data); err != nil {
			t.Fatalf("%s: failed to render: %v", tc.name, err)
		}
		out := buf.String()
		if !strings.Contains(out, "AT070AABU00281") {
			t.Errorf("%s: did not render the serial", tc.name)
		}
		// The serial is there to be taken, so the copy control is not decoration.
		if !strings.Contains(out, `class="dl-copy"`) {
			t.Errorf("%s: renders no copy control", tc.name)
		}
		// The tail is the last three characters: the split rule is shared with the
		// browser demo, and a hardware serial of this shape keeps its batch letter.
		if !strings.Contains(out, `<span class="tail">281</span>`) || !strings.Contains(out, `<span class="pre">AT070AABU00</span>`) {
			t.Errorf("%s: did not split the serial", tc.name)
		}
		// A firmware device's row carries its build (the live patch writes .dl-buildid).
		if !strings.Contains(out, `<span class="dl-buildid" title="Firmware build">v2.1.017</span>`) {
			t.Errorf("%s: does not show the firmware build", tc.name)
		}
		if got := strings.Count(out, `<tr class="ft-row`); got != 1 {
			t.Errorf("%s: %d device rows, want 1", tc.name, got)
		}
		if hide := tc.data["HideVenue"] == true; strings.Contains(out, `<th class="ft-venue"`) == hide {
			t.Errorf("%s: Venue column shown=%v, want %v", tc.name, !hide, !hide)
		}
		sortable := tc.data["Sortable"] == true
		if strings.Contains(out, `onclick="sortBy('battery')"`) != sortable {
			t.Errorf("%s: sortable headers=%v, want %v", tc.name, !sortable, sortable)
		}
		if sortable && !strings.Contains(out, `aria-sort="ascending"`) {
			t.Errorf("%s: the active sort column is not marked", tc.name)
		}
		if strings.Contains(out, `id="select-all"`) != (tc.data["SelectAll"] == true) {
			t.Errorf("%s: select-all box in the wrong place", tc.name)
		}
		if strings.Contains(out, `class="dl-remove"`) != (tc.data["RemovePath"] != nil) {
			t.Errorf("%s: remove control in the wrong place", tc.name)
		}
	}
}
