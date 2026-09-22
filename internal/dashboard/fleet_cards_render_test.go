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

// The four fleet layouts are chosen by a cookie and rendered server-side, so a bad call
// inside one of them fails at render time — after the deploy, on a page someone is
// looking at. This renders every layout with a synthetic device and asserts the parts
// that are the whole point of the serial layouts.
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

	for _, layout := range []string{"classic", "new", "serial", "table"} {
		var buf bytes.Buffer
		data := map[string]any{
			"Devices":            []db.Device{dev},
			"Online":             map[uuid.UUID]bool{dev.ID: true},
			"ActiveThresholdSecs": 300,
			"Classic":            layout == "classic",
			"Layout":             layout,
		}
		if err := tmpl.ExecuteTemplate(&buf, "device-cards", data); err != nil {
			t.Fatalf("layout %q failed to render: %v", layout, err)
		}
		out := buf.String()
		if !strings.Contains(out, "AT070AABU00281") {
			t.Errorf("layout %q did not render the serial", layout)
		}
		// The two serial layouts exist to make the number takeable, so the copy control
		// is not decoration — its absence is the feature missing.
		if layout == "serial" || layout == "table" {
			if !strings.Contains(out, `class="dl-copy"`) {
				t.Errorf("layout %q renders no copy control", layout)
			}
		}
		if layout == "serial" {
			// The tail is the last three characters: the split rule is shared with the
			// browser demo, and a hardware serial of this shape keeps its batch letter.
			if !strings.Contains(out, `<span class="tail">281</span>`) {
				t.Errorf("layout %q did not split the serial's tail", layout)
			}
			if !strings.Contains(out, `<span class="pre">AT070AABU00</span>`) {
				t.Errorf("layout %q did not split the serial's prefix", layout)
			}
		}
	}
}
