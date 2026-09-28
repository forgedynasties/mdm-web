package dashboard

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"testing"
	"time"

	"mdm/internal/db"

	"github.com/google/uuid"
)

func TestOpenHours(t *testing.T) {
	for _, c := range []struct {
		open, close int
		want        []int
	}{
		{660, 1380, []int{11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22}}, // 11am–11pm
		{1080, 120, []int{18, 19, 20, 21, 22, 23, 0, 1}},                   // 6pm–2am, past midnight
		{690, 1350, []int{11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21}},     // 11:30am–10:30pm
	} {
		if got := openHours(c.open, c.close); fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("openHours(%d, %d) = %v, want %v", c.open, c.close, got, c.want)
		}
	}
	if n := len(openHours(0, 0)); n != 24 {
		t.Errorf("no window should be the whole day, got %d hours", n)
	}
}

func TestGradeFor(t *testing.T) {
	for _, c := range []struct {
		ready, red int
		want       string
	}{
		{99, 0, "A"}, {99, 1, "A−"}, {95, 0, "A−"}, {93, 0, "B+"}, {90, 0, "B"}, {85, 0, "C"}, {60, 0, "D"}, {60, 2, "D"},
	} {
		if g, _, _ := gradeFor(c.ready, c.red); g != c.want {
			t.Errorf("gradeFor(%d, %d) = %s, want %s", c.ready, c.red, g, c.want)
		}
	}
}

// storyFixture is a week at a four-tablet venue open 11am–11pm, in Los Angeles:
//   - Table 1 is fine all week and charges three guest phones;
//   - Table 2 runs flat on Saturday 8pm–10pm;
//   - the unnamed tablet goes offline for Friday dinner (6pm–9pm, alert at 6:20pm);
//   - Bar <b>1</b> has a failing charging cable, and a name that must be escaped.
func storyFixture(t *testing.T) (storyInput, map[string]uuid.UUID) {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("no tz database: ", err)
	}
	from := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC) // a Monday
	win := reportWindow{From: from, To: from.AddDate(0, 0, 6), Days: 7}
	ids := map[string]uuid.UUID{"t1": uuid.New(), "t2": uuid.New(), "t3": uuid.New(), "bar": uuid.New()}
	devices := []db.DeviceWeek{
		{DeviceID: ids["t1"], Serial: "SER0001", Nickname: "Table 1", DeviceDays: 7, PoweredMinutes: 10000, FullWindowMinutes: 10080, PluggedMinutes: 6000},
		{DeviceID: ids["t2"], Serial: "SER0002", Nickname: "Table 2", DeviceDays: 7, PoweredMinutes: 9800, FullWindowMinutes: 10080, PluggedMinutes: 5000},
		{DeviceID: ids["t3"], Serial: "SER0003", DeviceDays: 7, PoweredMinutes: 9600, FullWindowMinutes: 10080, PluggedMinutes: 5500},
		{DeviceID: ids["bar"], Serial: "SER0004", Nickname: "Bar <b>1</b>", DeviceDays: 7, PoweredMinutes: 9900, FullWindowMinutes: 10080, PluggedMinutes: 5800},
	}
	dayStart := time.Date(2026, 9, 21, 0, 0, 0, 0, loc)
	var ins, prev db.VenueInsights
	ins.Restarts, prev.Restarts = map[uuid.UUID]int{}, map[uuid.UUID]int{}
	for d := 0; d < 7; d++ {
		for h := 0; h < 24; h++ {
			at := dayStart.AddDate(0, 0, d).Add(time.Duration(h) * time.Hour)
			for key, id := range ids {
				prev.Hours = append(prev.Hours, db.VenueHour{DeviceID: id, Hour: at.AddDate(0, 0, -7), MinBattery: 80})
				bat := 80
				if key == "t2" && d == 5 && h >= 20 && h < 22 { // Saturday 8pm–10pm
					bat = 3
				}
				if key == "t3" && d == 4 && h >= 18 && h < 21 { // Friday 6pm–9pm: silent
					continue
				}
				ins.Hours = append(ins.Hours, db.VenueHour{DeviceID: id, Hour: at, MinBattery: bat})
			}
		}
	}
	for i := 0; i < 3; i++ {
		ins.Sessions = append(ins.Sessions, db.PadSession{DeviceID: ids["t1"], Start: dayStart.AddDate(0, 0, 4).Add(time.Duration(19+i) * time.Hour), Minutes: 20})
	}
	prev.Sessions = []db.PadSession{{DeviceID: ids["t1"], Start: dayStart.AddDate(0, 0, -3).Add(19 * time.Hour), Minutes: 30}}
	ins.Alerts = []db.VenueAlert{
		{DeviceID: ids["t3"], Type: "offline_peak", At: dayStart.AddDate(0, 0, 4).Add(18*time.Hour + 20*time.Minute)},
		{DeviceID: ids["bar"], Type: "charger_flapping", At: dayStart.AddDate(0, 0, 2).Add(15 * time.Hour)},
	}
	return storyInput{
		Win: win, Loc: loc, OpenMin: 660, CloseMin: 1380, Now: dayStart.AddDate(0, 0, 14),
		Devices: devices, Metrics: &db.SiteMetrics{ConnectAvgPct: 34, ConnectCount: 100},
		Ins: ins, Prev: prev, PrevFrom: dayStart.AddDate(0, 0, -7),
	}, ids
}

func TestBuildVenueStory(t *testing.T) {
	in, ids := storyFixture(t)
	st := buildVenueStory(in)

	// 4 tablets × 7 days × 12 open hours = 336; Table 2 lost 2 to running flat and the
	// unnamed tablet 3 to being silent.
	if !st.HasReady || st.ReadyPct != (336-5)*100/336 {
		t.Errorf("ready = %d%% (has %v), want %d%%", st.ReadyPct, st.HasReady, (336-5)*100/336)
	}
	if st.ReadyDelta == nil || st.ReadyDelta.Class != "dn" || st.ReadyDelta.Prev != "from 100%" {
		t.Errorf("ready delta = %+v, want down from 100%%", st.ReadyDelta)
	}
	if st.Flat != 1 || fmt.Sprint(st.FlatNames) != "[Table 2]" {
		t.Errorf("flat = %d %v, want one episode on Table 2", st.Flat, st.FlatNames)
	}
	if st.Sessions != 3 || st.SessionsDelta == nil || st.SessionsDelta.Text != "▲ 2" {
		t.Errorf("sessions = %d, delta %+v; want 3, ▲ 2", st.Sessions, st.SessionsDelta)
	}
	if len(st.TopSpots) == 0 || st.TopSpots[0].Name != "Table 1" || st.TopSpots[0].Count != 3 {
		t.Errorf("top spots = %+v", st.TopSpots)
	}
	if st.PlugInPct != 34 {
		t.Errorf("plug-in = %d%%, want 34%%", st.PlugInPct)
	}

	status := map[uuid.UUID]string{}
	for _, tb := range st.Tablets {
		status[tb.DeviceID] = tb.Status
	}
	if status[ids["t1"]] != "g" || status[ids["t2"]] != "a" || status[ids["t3"]] != "r" || status[ids["bar"]] != "a" {
		t.Errorf("statuses t1 %s t2 %s t3 %s bar %s, want g a r a",
			status[ids["t1"]], status[ids["t2"]], status[ids["t3"]], status[ids["bar"]])
	}
	if st.Red != 1 || st.Amber != 2 || st.Fine != 1 || len(st.Attention) != 3 || st.Attention[0].DeviceID != ids["t3"] {
		t.Errorf("red %d amber %d fine %d attention %d (first %s), want 1 2 1 3 with the red tablet first",
			st.Red, st.Amber, st.Fine, len(st.Attention), st.Attention[0].Name)
	}
	if st.Attention[0].Name != "SER0003" {
		t.Errorf("an unnamed tablet should read by its serial, got %q", st.Attention[0].Name)
	}
	if !strings.Contains(st.Attention[0].Reason(), "Fri 6:20pm") {
		t.Errorf("offline reason %q should say when", st.Attention[0].Reason())
	}
	// Ready 98% would be an A; a tablet that let guests down costs a step.
	if st.Grade != "A−" {
		t.Errorf("grade = %s, want A−", st.Grade)
	}

	if len(st.Timeline) != 7 || len(st.Timeline[0].Cells) != 12 || len(st.HourLabel) != 12 || st.HourLabel[0] != "11am" {
		t.Fatalf("timeline %d×%d, labels %v", len(st.Timeline), len(st.Timeline[0].Cells), st.HourLabel)
	}
	fri, sat := st.Timeline[4], st.Timeline[5]
	if fri.Cells[7].Class != "a" || !fri.Cells[7].Flag { // 6pm, the offline alert's hour
		t.Errorf("Fri 6pm = %+v, want one tablet down, flagged", fri.Cells[7])
	}
	if sat.Cells[9].Class != "a" || sat.Cells[11].Class != "g" { // 8pm flat, 10pm back
		t.Errorf("Sat 8pm %+v, 10pm %+v", sat.Cells[9], sat.Cells[11])
	}
	if st.AllReadyHours != 84-5 || st.OpenHourSlots != 84 {
		t.Errorf("all-ready hours %d of %d, want 79 of 84", st.AllReadyHours, st.OpenHourSlots)
	}
	if len(st.Moments) != 3 || st.Moments[0].Name != "Bar <b>1</b>" {
		t.Errorf("moments %+v", st.Moments)
	}

	letter := ""
	for _, p := range st.Letter {
		letter += string(p) + "\n"
	}
	if strings.Contains(letter, "<b>1</b>") || !strings.Contains(letter, "Bar &lt;b&gt;1&lt;/b&gt;") {
		t.Errorf("a nickname reached the letter unescaped:\n%s", letter)
	}
	for _, want := range []string{"4 tablets", "3 times", "Table 1 was the most popular", "34% battery", "Table 2 ran flat"} {
		if !strings.Contains(letter, want) {
			t.Errorf("letter missing %q:\n%s", want, letter)
		}
	}
}

// TestCurrentWeekHidesCountDeltas: counts from a week in progress cannot be set against
// a whole week, so only the percentage is compared.
func TestCurrentWeekHidesCountDeltas(t *testing.T) {
	in, _ := storyFixture(t)
	in.Win.Current = true
	st := buildVenueStory(in)
	if st.SessionsDelta != nil || st.FlatDelta != nil {
		t.Errorf("count deltas shown for a part week: %+v %+v", st.SessionsDelta, st.FlatDelta)
	}
	if st.ReadyDelta == nil {
		t.Error("the readiness comparison should stay")
	}
}

// TestRestaurantReportViewsRender executes every view against a real story, so a field
// or method a view names that does not exist fails here rather than as a blank page.
func TestRestaurantReportViewsRender(t *testing.T) {
	in, _ := storyFixture(t)
	st := buildVenueStory(in)
	funcs := template.FuncMap{
		"hrs1":       func(minutes float64) string { return fmt.Sprintf("%.1f", minutes/60) },
		"band":       func(int) string { return "" },
		"lowerFirst": lowerFirst,
		"index0": func(s []storySpot) *storySpot {
			if len(s) == 0 {
				return nil
			}
			return &s[0]
		},
	}
	tmpl, err := template.New("").Funcs(funcs).Parse(`{{define "header"}}<html><body>{{end}}{{define "footer"}}</body></html>{{end}}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmpl.ParseFiles("../../templates/restaurant_report.html"); err != nil {
		t.Fatalf("parse: %v", err)
	}
	wants := map[string][]string{
		"scorecard": {"A−", "This week, please", "SER0003", "Tablets that need a look"},
		"letter":    {"Hi team,", "The AIO team", "Bar &lt;b&gt;1&lt;/b&gt;"},
		"tablets":   {"Your tablets this week", "1 fine · 2 need a look · 1 let guests down", "/devices/SER0001"},
		"service":   {"Every tablet ready in 79 of 84 opening hours", "What happened", "went offline during opening hours"},
		"guests":    {"phones charged at the table", "Table 1 (3)", "When guests charged their phones"},
		"summary":   {"Page 1 · for the venue", "Needs attention", "Page 2", "Individual device reports", "The other 1 tablet was fine all week."},
	}
	for view, want := range wants {
		key, views := pickReportView(view)
		if key != view {
			t.Fatalf("pickReportView(%q) = %q", view, key)
		}
		data := map[string]any{
			"Restaurant":  &db.Restaurant{ID: uuid.New(), Name: "Harbor & Vine"},
			"WindowFrom":  in.Win.From,
			"WindowTo":    in.Win.To,
			"WindowHours": 168,
			"DeviceWeeks": in.Devices,
			"Week":        in.Win,
			"Weeks":       reportWeeks(time.Now()),
			"View":        key,
			"Views":       views,
			"Story":       st,
		}
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "restaurant_report.html", data); err != nil {
			t.Fatalf("%s: execute: %v", view, err)
		}
		out := buf.String()
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("%s view is missing %q", view, w)
			}
		}
		if strings.Contains(out, "Bar <b>1</b>") {
			t.Errorf("%s view rendered a nickname as HTML", view)
		}
		if !strings.Contains(out, "Print or save as PDF") || strings.Contains(out, "report.pdf?week=") {
			t.Errorf("%s view should print from the browser, not link the standard PDF", view)
		}
	}
	// No view: the standard report, untouched.
	key, views := pickReportView("nonsense")
	if key != "" || !views[0].Selected {
		t.Errorf("an unknown view should fall back to the standard report, got %q", key)
	}
}
