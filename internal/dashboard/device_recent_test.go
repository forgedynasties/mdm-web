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

// The Activity tab and the Recent on this device card share their rows (G3).
func TestDeviceRecentRenders(t *testing.T) {
	src, _ := os.ReadFile("handlers.go")
	funcs := template.FuncMap{}
	for _, re := range []*regexp.Regexp{regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`), regexp.MustCompile(`funcMap\["([a-zA-Z][a-zA-Z0-9]*)"\]`)} {
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			funcs[m[1]] = func(...any) any { return "" }
		}
	}
	tmpl := template.Must(template.New("").Funcs(funcs).ParseGlob("../../templates/*.html"))
	now := time.Now()
	cmd := &db.DeviceCommand{ID: uuid.New(), Type: "reboot", Status: "completed", CreatedAt: now}
	data := map[string]any{
		"Device": &db.Device{SerialNumber: "S1"}, "Show": "all",
		"Items": []timelineItem{{At: now, Cmd: cmd}, {At: now.Add(-time.Minute), Change: &deviceActivityItem{Actor: "Ali", Did: "turned wireless charging on", At: now.Add(-time.Minute)}}},
	}
	for _, name := range []string{"device-recent", "device-timeline"} {
		var b bytes.Buffer
		if err := tmpl.ExecuteTemplate(&b, name, data); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out := b.String()
		if !strings.Contains(out, "/commands/"+cmd.ID.String()) || !strings.Contains(out, "turned wireless charging on") {
			t.Errorf("%s is missing a row:\n%s", name, out)
		}
	}
	// Activity tab: the queue is the "Now" section of the same card (27 Oct 2026).
	q := db.DeviceCommand{ID: uuid.New(), Type: "screenshot", Status: "pending", CreatedAt: now}
	data["Queue"] = []db.DeviceCommand{q}
	data["Role"] = "admin"
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, "device-timeline", data); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, `<span class="dq-title">Now</span>`) || !strings.Contains(out, `>Earlier</div>`) || !strings.Contains(out, `data-cmd-id="`+q.ID.String()+`"`) {
		t.Errorf("activity card misses the queue:\n%s", out)
	}
	if strings.Contains(out, "Nothing queued") {
		t.Error("an empty-queue sentence is back")
	}
}
