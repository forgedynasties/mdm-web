package dashboard

import (
	"bytes"
	"html/template"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestAppUsageCardRenders(t *testing.T) {
	src, _ := os.ReadFile("handlers.go")
	funcs := template.FuncMap{}
	for _, re := range []*regexp.Regexp{regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`), regexp.MustCompile(`funcMap\["([a-zA-Z][a-zA-Z0-9]*)"\]`)} {
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			funcs[m[1]] = func(...any) any { return "" }
		}
	}
	tmpl := template.Must(template.New("").Funcs(funcs).ParseGlob("../../templates/*.html"))
	render := func(v appUsageView) string {
		var b bytes.Buffer
		if err := tmpl.ExecuteTemplate(&b, "dd-app-usage", v); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	full := render(appUsageView{Serial: "S1", Window: "24h", Windows: []string{"24h", "7d", "30d"}, Sort: "mem", HasBattery: true, Reported: true, All: true,
		Rows: []appUsageRowView{{Name: "Nugget", Package: "aio.app.nugget", Screen: "11 h 40", Mem: "412 MB", MemPeak: "450 MB peak", CPU: "18.0 %", Net: "1.9 GB", Battery: "120 mAh", BarPct: 100}}, More: 3})
	for _, w := range []string{`id="dd-app-usage"`, `>Memory</th>`, `>Battery</th>`, `450 MB peak`, `3 more apps`, `hx-get="/devices/S1/app-usage?window=7d&sort=mem&all=1"`, `← Top apps`} {
		if !strings.Contains(full, w) {
			t.Errorf("full card missing %q", w)
		}
	}
	// T1, the card as it opens: two columns of bars, then "All N apps".
	top := render(appUsageView{Serial: "S1", Window: "24h", Windows: []string{"24h", "7d", "30d"}, Sort: "mem", Reported: true, Count: 41,
		Cols: []auCol{{Title: "Memory", Total: "2.2 GB in apps", Bars: []auBar{{Name: "Chrome", Value: "343 MB", Pct: 100, Tone: "bad"}, {Name: "Nugget", Value: "15 MB", Pct: 4, Tone: "role"}}},
			{Title: "CPU", Total: "87 % summed", Bars: []auBar{{Name: "AIO MDM", Value: "9.4 %", Pct: 100}}}}})
	for _, w := range []string{`>Top apps</h2>`, `2.2 GB in apps`, `class="bad" style="width:100%"`, `class="role"`, `All 41 apps →`, `app-usage?window=24h&all=1`} {
		if !strings.Contains(top, w) {
			t.Errorf("top card missing %q", w)
		}
	}
	if strings.Contains(top, `<th`) || strings.Contains(top, `class="au-sort"`) {
		t.Error("the top view shows the table or its sort")
	}
	dpc := render(appUsageView{Serial: "S2", Window: "24h", Windows: []string{"24h"}, Sort: "screen", Limited: true, Reported: true, All: true,
		Rows: []appUsageRowView{{Name: "Menu boards", Package: "aio.app.menuboards", Screen: "9 h"}}})
	if strings.Contains(dpc, ">Memory</th>") || !strings.Contains(dpc, "screen time and network only") {
		t.Error("Standard client card shows columns it cannot measure, or no note")
	}
	if e := render(appUsageView{Serial: "S3", Window: "24h"}); !strings.Contains(e, "No app usage reported") {
		t.Error("empty card has no note")
	}
}

func TestAppUsageFormats(t *testing.T) {
	for in, want := range map[int64]string{0: "—", 30000: "<1 min", 12 * 60000: "12 min", 185 * 60000: "3 h 05"} {
		if got := fmtMinutes(in); got != want {
			t.Errorf("fmtMinutes(%d) = %q, want %q", in, got, want)
		}
	}
	if fmtBytes(3<<30) != "3.0 GB" || fmtBytes(5<<20) != "5 MB" {
		t.Error("fmtBytes")
	}
}
