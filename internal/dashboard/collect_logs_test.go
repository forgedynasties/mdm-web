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
	"mdm/internal/product"
)

// The script is the server's, sent as shell: small sections first and the long logcat
// last, so the client's 1 MB cap only ever cuts old log lines.
func TestCollectLogsScript(t *testing.T) {
	if db.DeviceCommandType("collect_logs") != "shell" {
		t.Fatal("collect_logs must reach the device as a shell command")
	}
	if c, ok := product.CommandFor("collect_logs"); !ok || c.Cap != product.CapShell || c.Access != "query" {
		t.Fatalf("catalogue entry = %+v", c)
	}
	i, j := strings.Index(collectLogsScript, `s "getprop"`), strings.Index(collectLogsScript, `s "logcat (last`)
	if i < 0 || j < i || !strings.HasSuffix(strings.TrimSpace(collectLogsScript), "2>/dev/null") {
		t.Errorf("logcat must be the last section")
	}
	if string(buildPayload("collect_logs", nil)) != string(collectLogsPayload()) {
		t.Error("the payload must come from the server, not the form")
	}
}

func TestCollectLogsCommandPageRenders(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := template.FuncMap{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(src), -1) {
		funcs[m[1]] = func(...any) any { return "" }
	}
	funcs["add"] = func(a, b int) int { return a + b }
	funcs["sub"] = func(a, b int) int { return a - b }
	funcs["pct"] = func(a, b int) int { return 0 }
	funcs["sizeLabel"] = func(n int) string { return "412 KB" }
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("../../templates/command_detail.html"))
	cid := uuid.New()
	data := map[string]any{
		"Command": db.Command{ID: cid, Type: "collect_logs"},
		"Deliveries": []db.CommandDelivery{
			{SerialNumber: "T7-1", Status: "completed", OutputBytes: 421000, UpdatedAt: time.Now()},
			{SerialNumber: "T7-2", Status: "delivered", Online: true, UpdatedAt: time.Now()},
		},
		"Stats": map[string]int{"Done": 1, "Total": 2, "Pending": 1, "Failed": 0, "Offline": 0, "Pct": 50},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "command-deliveries", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`href="/commands/` + cid.String() + `/logs.zip"`,
		`href="/commands/` + cid.String() + `/logs/T7-1?download=1"`,
		"412 KB", "Collecting…",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}
