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

// jobsSample: two running jobs (one still waiting on 34 devices), one failing install
// from today, and finished jobs today (one with a failed device) and yesterday.
func jobsSample(now time.Time) (attn, prog, done []db.Command, sum map[uuid.UUID]db.CommandDeliverySummary) {
	sum = map[uuid.UUID]db.CommandDeliverySummary{}
	mk := func(typ, by string, at time.Time, s db.CommandDeliverySummary) db.Command {
		c := db.Command{ID: uuid.New(), Type: typ, CreatedBy: by, CreatedAt: at, TargetType: "devices"}
		sum[c.ID] = s
		return c
	}
	prog = []db.Command{
		mk("app_update", "", now.Add(-2*time.Hour), db.CommandDeliverySummary{Completed: 126, Pending: 30, Delivered: 4}),
		mk("reboot", "tariq.jamil@aioapp.com", now.Add(-50*time.Minute), db.CommandDeliverySummary{Completed: 3, Delivered: 2}),
	}
	attn = []db.Command{
		mk("install_apk", "shahrukh.bashir@aioapp.com", now.Add(-3*time.Hour), db.CommandDeliverySummary{Failed: 2}),
	}
	done = []db.Command{
		mk("shell", "ali@aioapp.com", now.Add(-1*time.Hour), db.CommandDeliverySummary{Completed: 3, Failed: 1}),
		mk("screenshot", "ali@aioapp.com", now.Add(-90*time.Minute), db.CommandDeliverySummary{Completed: 1}),
		mk("screenshot", "ali@aioapp.com", now.Add(-26*time.Hour), db.CommandDeliverySummary{Completed: 1}),
	}
	return
}

func TestJobsCards(t *testing.T) {
	now := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	attn, prog, done, sum := jobsSample(now)
	cards := buildJobsCards(jobsCardInputs{Attn: attn, Prog: prog, Done: done, Sum: sum, Now: now})
	if len(cards) != 3 {
		t.Fatalf("cards = %d", len(cards))
	}
	if c := cards[0]; c.A.Value != "2" || c.A.Label != "jobs running" || c.B.Value != "36" || c.B.Label != "devices still to report" || c.LineA == "" {
		t.Errorf("in flight = %+v", c)
	}
	// Failing today: the install and the shell; none yesterday.
	if c := cards[1]; c.A.Value != "1" || c.B.Value != "2" || c.A.Delta != "▲ 2 today" || c.A.Tone != "bad" {
		t.Errorf("needs a look = %+v", c)
	}
	// Done today: the shell and a screenshot, one yesterday; 4 of 5 devices succeeded.
	if c := cards[2]; c.A.Value != "2" || c.A.Delta != "▲ 1 vs yesterday" || c.A.Tone != "good" || c.B.Value != "80%" {
		t.Errorf("done today = %+v", c)
	}
	// A quiet day: no delta, no percentage.
	empty := buildJobsCards(jobsCardInputs{Sum: map[uuid.UUID]db.CommandDeliverySummary{}, Now: now})
	if empty[2].B.Value != "—" || empty[1].A.Delta != "" || empty[0].A.Label != "jobs running" {
		t.Errorf("empty = %+v", empty)
	}
}

func TestJobFilters(t *testing.T) {
	now := time.Now()
	attn, prog, done, _ := jobsSample(now)
	f := buildJobFilters(attn, prog, done)
	var types, by []string
	for _, o := range f.Types {
		types = append(types, o.Href)
	}
	for _, o := range f.Senders {
		by = append(by, o.Href+"="+o.Label)
	}
	if strings.Join(types, ",") != "app_update,install_apk,reboot,screenshot,shell" && len(types) != 5 {
		t.Errorf("types = %v", types)
	}
	if len(by) != 4 || !strings.Contains(strings.Join(by, ","), "auto=Sent automatically") {
		t.Errorf("senders = %v", by)
	}
}

// TestJobsPageRenders executes jobs.html with the sample: cards, rollouts, pills, the
// table header and rows carrying the data the pills filter on.
func TestJobsPageRenders(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := template.FuncMap{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(src), -1) {
		funcs[m[1]] = func(...any) any { return "" }
	}
	funcs["dict"] = func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	funcs["add"] = func(a, b int) int { return a + b }
	funcs["sub"] = func(a, b int) int { return a - b }
	funcs["pct"] = func(a, b int) int { return 0 }
	funcs["canOperate"] = func(string) bool { return true }
	funcs["canAdmin"] = func(string) bool { return true }
	funcs["cmdTitle"] = func(c any) string { return "Job title" }
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}{{define "ubub"}}{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("../../templates/jobs.html", "../../templates/commands.html", "../../templates/_settings_cards.html"))

	now := time.Now()
	attn, prog, done, sum := jobsSample(now)
	rollouts := buildOvRollouts([]db.Rollout{{Kind: "install_apk", Name: "Nugget", Who: "shahrukh.bashir@aioapp.com",
		Start: now.Add(-30 * time.Hour), End: now.Add(-29 * time.Hour), Total: 2, Done: 2, Commands: 2, AppPackage: "aio.app.nugget"}}, now, true)
	data := map[string]any{
		"Role": "admin", "Attention": attn, "InProgress": prog, "Completed": done,
		"AttnCount": len(attn), "ProgCount": len(prog), "DoneCount": len(done), "Summaries": sum,
		"TargetSerials": map[uuid.UUID][]string{}, "AppNames": map[string]string{}, "AppIcons": map[string]string{},
		"PkgIcons": map[string]string{}, "Batches": map[uuid.UUID][]db.Command{}, "Clusters": map[uuid.UUID][]db.Command{},
		"JobCards":    buildJobsCards(jobsCardInputs{Attn: attn, Prog: prog, Done: done, Sum: sum, Now: now}),
		"JobFilters":  buildJobFilters(attn, prog, done),
		"JobRollouts": rollouts,
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "jobs.html", data); err != nil {
		t.Fatalf("jobs.html failed to render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`id="jz-cards"`, `hx-get="/jobs?partial=jobscards"`, `>In flight</a>`, `>Needs a look</a>`, `>Done today</a>`,
		`<b>36</b><small>devices still to report</small>`,
		`Rollouts <span>last 7 days`, `href="/apps/aio.app.nugget"`, `2 / 2`,
		`data-jz-set="state" data-val="attn">Needs a look</button>`, `data-jz-set="by" data-val="auto">Sent automatically</button>`,
		`Job and progress`, `id="cmd-timeline"`, `hx-get="/jobs?partial=histlive"`,
		`data-bucket="attn" data-by="shahrukh.bashir@aioapp.com"`, `data-bucket="prog" data-by="auto"`,
		`<span class="ht-chip fail">Needs a look</span>`, `<span class="ht-chip run">Running</span>`, `<span class="ht-chip ok">Done</span>`,
		`/static/js/jobs_j1.js`, `/static/js/oc-menus.js`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Jobs page is missing %q", want)
		}
	}
	// The cards are their own fragment for the live refresh.
	buf.Reset()
	if err := tmpl.ExecuteTemplate(&buf, "jobs-cards", data); err != nil || !strings.Contains(buf.String(), `class="oc-stats"`) {
		t.Errorf("jobs-cards fragment: %v", err)
	}
	// A scoped viewer gets no rollouts card (it is fleet-wide).
	delete(data, "JobRollouts")
	buf.Reset()
	if err := tmpl.ExecuteTemplate(&buf, "jobs.html", data); err != nil || strings.Contains(buf.String(), `Rollouts <span>`) {
		t.Errorf("rollouts shown without data: %v", err)
	}
}
