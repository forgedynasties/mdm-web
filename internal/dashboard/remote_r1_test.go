package dashboard

import (
	"bytes"
	"encoding/json"
	"html/template"
	"os"
	"regexp"
	"strings"
	"testing"

	"mdm/internal/db"
)

func TestRemoteKbdLevel(t *testing.T) {
	for _, c := range []struct {
		kind, extra, want string
	}{
		{"firmware", `{"agent_version":"1.15.3"}`, "keys"},
		{"firmware", `{"remote_input":2}`, "full"},
		{"dpc", `{"remote_input":2}`, "dpc"},
	} {
		d := &db.Device{AgentKind: c.kind, LatestExtra: json.RawMessage(c.extra)}
		if got := remoteKbdLevel(d); got != c.want {
			t.Errorf("%s %s = %q, want %q", c.kind, c.extra, got, c.want)
		}
	}
}

// TestRemoteR1Renders: the R1 page renders for each keyboard level, saying what the
// device's client cannot take.
func TestRemoteR1Renders(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
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
	tmpl := template.Must(template.New("").Funcs(funcs).ParseGlob("../../templates/*.html"))
	template.Must(tmpl.Parse(`{{define "header"}}<i hidden></i>{{end}}{{define "footer"}}<i hidden></i>{{end}}`))
	for kbd, wants := range map[string][]string{
		"full": {`id="rc-log"`, `Copy from device`, `kbd: "full"`, `id="rc-text"`},
		"keys": {`plain keys only`, `id="rc-text"`, `title="Needs firmware client 1.16.0"`},
		"dpc":  {`Standard client can only take arrows`},
	} {
		var buf bytes.Buffer
		data := map[string]any{"Serial": "AT070AABU00848", "Online": true, "Token": "t", "Kbd": kbd,
			"Device": &db.Device{SerialNumber: "AT070AABU00848", LatestExtra: json.RawMessage(`{}`)}}
		if err := tmpl.ExecuteTemplate(&buf, "remote.html", data); err != nil {
			t.Fatalf("%s: %v", kbd, err)
		}
		out := buf.String()
		for _, w := range append(wants, `id="rc-kchip"`, `class="rc-tools"`, `Shift</kbd>+<kbd>Esc`) {
			if !strings.Contains(out, w) {
				t.Errorf("%s: missing %q", kbd, w)
			}
		}
		if kbd == "dpc" && strings.Contains(out, `id="rc-text"`) {
			t.Error("dpc: the text box is offered to a client that cannot type")
		}
		if kbd != "full" && strings.Contains(out, "Copy from device") {
			t.Errorf("%s: Copy from device offered without client support", kbd)
		}
	}
}
