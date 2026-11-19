package dashboard

import (
	"html/template"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"mdm/internal/db"
)

// People Activity PA2 "Actor first" (18 Nov 2026, picked from the users-pages demos): the
// left list is the actors of the chosen track (a person, a job, a key) with counts and a
// failed dot; the right side explains the picked one: facts, a 7-day bar and its events.

type pa2Row struct {
	Key, Name, Sub string
	N, Failed      int
	Last           time.Time
	On             bool
	URL            template.URL
}

type pa2Fact struct {
	Val   string
	Time  *time.Time
	Label string
	Bad   bool
}

type pa2Bar struct {
	Label string
	Pct   int
	Today bool
}

type pa2View struct {
	Rows    []pa2Row
	AllURL  template.URL
	AllOn   bool
	AllN    int
	Title   string
	Sub     string
	Person  bool
	Facts   []pa2Fact
	Bars    []pa2Bar
	TrkURLs map[string]template.URL
}

func pa2N(n int) string { return strconv.Itoa(n) }

// usernameByName maps a display name back to the stable username the actor filter takes.
func usernameByName(users []db.User) map[string]string {
	out := make(map[string]string, len(users))
	for _, u := range users {
		if _, ok := out[u.DisplayName()]; !ok {
			out[u.DisplayName()] = u.Username
		}
	}
	return out
}

// pa2People lists the people who did something in the track's entries, most active first.
func pa2People(entries []db.AuditEntry, userOf map[string]string) []pa2Row {
	idx := map[string]int{}
	var rows []pa2Row
	for _, e := range entries {
		i, ok := idx[e.Actor]
		if !ok {
			key := e.Actor
			if u, ok := userOf[e.Actor]; ok {
				key = u
			}
			rows = append(rows, pa2Row{Key: key, Name: e.Actor})
			i = len(rows) - 1
			idx[e.Actor] = i
		}
		rows[i].N++
		if e.CreatedAt.After(rows[i].Last) {
			rows[i].Last = e.CreatedAt
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].N != rows[j].N {
			return rows[i].N > rows[j].N
		}
		return strings.ToLower(rows[i].Name) < strings.ToLower(rows[j].Name)
	})
	return rows
}

// pa2Sources turns the Automations / API & QA job and key summaries into list rows.
func pa2Sources(src []trackSource) []pa2Row {
	rows := make([]pa2Row, 0, len(src))
	for _, s := range src {
		rows = append(rows, pa2Row{Key: s.Key, Name: s.Name, Sub: s.About, N: s.Sent, Failed: s.Failed, Last: s.Last})
	}
	return rows
}

// pa2Pick narrows the track's entries to the picked person, job or key.
func pa2Pick(entries []db.AuditEntry, track, actor, actorName, src string) []db.AuditEntry {
	switch {
	case track == trackPeople && actor != "":
		kept := make([]db.AuditEntry, 0, len(entries))
		for _, e := range entries {
			if e.Actor == actorName {
				kept = append(kept, e)
			}
		}
		return kept
	case track != trackPeople && src != "":
		want := strings.ToLower(strings.TrimSpace(src))
		kept := make([]db.AuditEntry, 0, len(entries))
		for _, e := range entries {
			if strings.ToLower(strings.TrimSpace(e.Actor)) == want {
				kept = append(kept, e)
			}
		}
		return kept
	}
	return entries
}

func pa2URL(track, days, device, kind string, admin, views bool, actor, src string) template.URL {
	q := url.Values{}
	if track != trackPeople {
		q.Set("track", track)
	}
	if actor != "" {
		q.Set("actor", actor)
	}
	if src != "" {
		q.Set("src", src)
	}
	q.Set("days", days)
	if device != "" {
		q.Set("device", device)
	}
	if kind != "" {
		q.Set("kind", kind)
	}
	if admin {
		q.Set("admin", "1")
	}
	if views {
		q.Set("views", "1")
	}
	return template.URL("/activity?" + q.Encode())
}

// buildPA2 assembles the list, the picked actor's facts and the 7-day bar. entries are the
// track's entries after every filter (not just one page).
func buildPA2(rows []pa2Row, entries []db.AuditEntry, track, actor, actorName, src, days, device string, admin, views bool, kind string) pa2View {
	v := pa2View{TrkURLs: map[string]template.URL{}}
	for _, t := range []string{trackPeople, trackAuto, trackAPI} {
		v.TrkURLs[t] = pa2URL(t, days, device, kind, admin, views, "", "")
	}
	var cur *pa2Row
	for i := range rows {
		r := &rows[i]
		if track == trackPeople {
			r.On = actor != "" && (r.Key == actor || r.Name == actorName)
			r.URL = pa2URL(track, days, device, kind, admin, views, r.Key, "")
		} else {
			r.On = src != "" && strings.EqualFold(strings.TrimSpace(r.Key), strings.TrimSpace(src))
			r.URL = pa2URL(track, days, device, kind, admin, views, "", r.Key)
		}
		if r.On {
			cur = r
		}
		v.AllN += r.N
	}
	v.Rows = rows
	v.AllOn = cur == nil
	v.AllURL = pa2URL(track, days, device, kind, admin, views, "", "")

	var last time.Time
	serials := map[string]bool{}
	people := map[string]bool{}
	for _, e := range entries {
		if e.CreatedAt.After(last) {
			last = e.CreatedAt
		}
		if e.DeviceSerial != "" {
			serials[e.DeviceSerial] = true
		}
		people[e.Actor] = true
	}
	lastP := func() *time.Time {
		if last.IsZero() {
			return nil
		}
		return &last
	}
	switch {
	case cur != nil && track == trackPeople:
		v.Title, v.Person, v.Sub = cur.Name, true, "Everything they did, newest first"
		v.Facts = []pa2Fact{{Val: pa2N(len(entries)), Label: "Actions"}, {Val: pa2N(len(serials)), Label: "Devices touched"}, {Time: lastP(), Label: "Last action"}}
	case cur != nil:
		v.Title, v.Sub = cur.Name, cur.Sub
		lt := cur.Last
		v.Facts = []pa2Fact{{Val: pa2N(cur.N), Label: "Commands sent"}, {Val: pa2N(cur.Failed), Label: "Failed", Bad: cur.Failed > 0}, {Time: &lt, Label: "Last ran"}, {Val: pa2N(len(entries)), Label: "Logged actions"}}
	case track == trackPeople:
		v.Title, v.Sub = "All people", "Every person's actions"
		v.Facts = []pa2Fact{{Val: pa2N(len(entries)), Label: "Actions"}, {Val: pa2N(len(people)), Label: "People"}, {Val: pa2N(len(serials)), Label: "Devices touched"}, {Time: lastP(), Label: "Last action"}}
	default:
		sub, title := "Every job", "All automations"
		if track == trackAPI {
			sub, title = "Every key", "All API & QA"
		}
		sent, failed := 0, 0
		for _, r := range rows {
			sent += r.N
			failed += r.Failed
		}
		v.Title, v.Sub = title, sub
		v.Facts = []pa2Fact{{Val: pa2N(sent), Label: "Commands sent"}, {Val: pa2N(failed), Label: "Failed", Bad: failed > 0}, {Time: lastP(), Label: "Last action"}, {Val: pa2N(len(entries)), Label: "Logged actions"}}
	}

	// 7-day bar, one column per UTC day, today last.
	now := time.Now().UTC()
	day := func(t time.Time) time.Time {
		t = t.UTC()
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
	today := day(now)
	var counts [7]int
	max := 0
	for _, e := range entries {
		d := int(today.Sub(day(e.CreatedAt)).Hours() / 24)
		if d >= 0 && d < 7 {
			counts[6-d]++
			if counts[6-d] > max {
				max = counts[6-d]
			}
		}
	}
	for i := 0; i < 7; i++ {
		pct := 6
		if max > 0 {
			pct = counts[i] * 100 / max
			if pct < 6 {
				pct = 6
			}
		}
		v.Bars = append(v.Bars, pa2Bar{Label: today.AddDate(0, 0, i-6).Format("Mon")[:2], Pct: pct, Today: i == 6})
	}
	return v
}
