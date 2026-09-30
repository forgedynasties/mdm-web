package dashboard

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"mdm/internal/alerts"
	"mdm/internal/db"
)

// problemView is one problem on the Alerts page and the Overview: the alerts open on
// one device, or the devices of one restaurant that are all offline, shown once with
// the likely cause in the title and the alerts as its symptoms (30 Sep alert review).
type problemView struct {
	Key        string
	Severity   string // the worst member's
	Cause      alerts.Cause
	Title      string
	Why        string
	Serial     string // set for a one-device problem
	Restaurant string
	RestID     string
	Devices    int
	Symptoms   []problemSymptom
	Members    []humanAlert
	IDs        string // the member alert ids, comma-separated, for the workflow forms
	Since      time.Time
	Assignee   string
	Note       string
	Acked      bool // every member acknowledged
	NewSymptom bool // acknowledged, but something new has opened since
	Snoozed    bool
	SnoozedTil time.Time
	AfterHours bool // held back overnight: it would be critical if the restaurant were open
	Href       string
	HrefLabel  string
	IconKey    string
	AppIcon    string
	Search     string
	CanAct     bool
	KeyReset   bool // an impersonation alert: the device may have lost its key
}

type problemSymptom struct {
	Label    string
	Count    int
	Severity string
}

// restaurantOutageSpread is how close together a restaurant's devices must have gone
// offline to read as one outage (its network) rather than separate faults.
const restaurantOutageSpread = 15 * time.Minute

// buildProblems folds active alerts into problems, worst first. hs[i] is the humanized
// alerts[i]; the handler has already set its trace, icon and CanAct.
func buildProblems(active []db.Alert, hs []humanAlert, users map[string]string) []problemView {
	type devGroup struct {
		alerts []db.Alert
		hs     []humanAlert
	}
	groups := map[string]*devGroup{}
	var order []string
	for i, a := range active {
		key := a.Serial
		if a.DeviceID != nil {
			key = a.DeviceID.String()
		}
		if key == "" {
			key = "alert:" + a.ID.String()
		}
		g, ok := groups[key]
		if !ok {
			g = &devGroup{}
			groups[key] = g
			order = append(order, key)
		}
		g.alerts = append(g.alerts, a)
		g.hs = append(g.hs, hs[i])
	}
	var devs []problemView
	for _, k := range order {
		g := groups[k]
		devs = append(devs, deviceProblem(k, g.alerts, g.hs, users))
	}
	// Devices of one restaurant that went offline together are one problem.
	byRest := map[string][]int{}
	for i, p := range devs {
		if p.Cause.Key == "offline" && p.Restaurant != "" && !p.Snoozed {
			byRest[p.Restaurant] = append(byRest[p.Restaurant], i)
		}
	}
	folded := map[int]bool{}
	var out []problemView
	for rest, idx := range byRest {
		if len(idx) < 2 {
			continue
		}
		first, last := devs[idx[0]].Since, devs[idx[0]].Since
		for _, i := range idx {
			if devs[i].Since.Before(first) {
				first = devs[i].Since
			}
			if devs[i].Since.After(last) {
				last = devs[i].Since
			}
		}
		p := problemView{
			Key: "restaurant:" + rest, Severity: "info", Cause: devs[idx[0]].Cause,
			Restaurant: rest, Devices: len(idx), Since: first, Acked: true,
			IconKey: "offline", CanAct: devs[idx[0]].CanAct,
			Title:     fmt.Sprintf("%s: %d devices offline", rest, len(idx)),
			HrefLabel: "Open restaurant",
		}
		if last.Sub(first) <= restaurantOutageSpread {
			p.Why = fmt.Sprintf("%d devices stopped checking in within %s of each other. Most likely the restaurant's Wi-Fi or router, not the devices.",
				len(idx), spreadText(last.Sub(first)))
		} else {
			p.Why = fmt.Sprintf("%d devices here are not checking in. Check the restaurant's Wi-Fi and that the devices are powered.", len(idx))
		}
		var ids []string
		for _, i := range idx {
			d := devs[i]
			folded[i] = true
			p.Members = append(p.Members, d.Members...)
			ids = append(ids, d.IDs)
			if db.SeverityRank(d.Severity) > db.SeverityRank(p.Severity) {
				p.Severity = d.Severity
			}
			p.Acked = p.Acked && d.Acked
			p.NewSymptom = p.NewSymptom || d.NewSymptom
			p.AfterHours = p.AfterHours || d.AfterHours
			if p.Assignee == "" {
				p.Assignee = d.Assignee
			}
			if p.Note == "" {
				p.Note = d.Note
			}
			if p.Href == "" && d.RestID != "" {
				p.Href = "/restaurants/" + d.RestID
			}
			p.Symptoms = append(p.Symptoms, problemSymptom{Label: d.Serial, Count: 1, Severity: d.Severity})
			p.Search += " " + d.Search
		}
		if p.Severity == "critical" {
			p.AfterHours = false
		}
		if p.Href == "" {
			p.Href = "/restaurants"
		}
		p.IDs = strings.Join(ids, ",")
		out = append(out, p)
	}
	for i, p := range devs {
		if !folded[i] {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := db.SeverityRank(out[i].Severity), db.SeverityRank(out[j].Severity)
		if ri != rj {
			return ri > rj
		}
		return out[i].Since.After(out[j].Since)
	})
	return out
}

func deviceProblem(key string, as []db.Alert, hs []humanAlert, users map[string]string) problemView {
	p := problemView{Key: key, Severity: "info", Acked: true, Snoozed: true, Devices: 1, CanAct: hs[0].CanAct}
	var types, ids []string
	someAcked, someOpen := false, false
	seen := map[string]int{}
	for i, a := range as {
		h := hs[i]
		types = append(types, a.Type)
		ids = append(ids, a.ID.String())
		if db.SeverityRank(a.Severity) > db.SeverityRank(p.Severity) {
			p.Severity = a.Severity
		}
		if p.Since.IsZero() || a.FiredAt.Before(p.Since) {
			p.Since = a.FiredAt
		}
		if a.Status == "acknowledged" {
			someAcked = true
		} else {
			someOpen = true
		}
		if a.MutedUntil == nil || a.MutedUntil.Before(time.Now()) {
			p.Snoozed = false
		} else if a.MutedUntil.After(p.SnoozedTil) {
			p.SnoozedTil = *a.MutedUntil
		}
		if p.Assignee == "" && a.Assignee != "" {
			p.Assignee = a.Assignee
		}
		if p.Note == "" && a.Note != "" {
			p.Note = a.Note
		}
		if afterHours(a.Detail) {
			p.AfterHours = true
		}
		label := symptomLabel(h)
		if j, ok := seen[label]; ok {
			p.Symptoms[j].Count += h.Occurrences
		} else {
			seen[label] = len(p.Symptoms)
			p.Symptoms = append(p.Symptoms, problemSymptom{Label: label, Count: h.Occurrences, Severity: a.Severity})
		}
		p.Members = append(p.Members, h)
		p.Search += " " + h.Headline + " " + plainSentence(h.Sentence)
		if p.AppIcon == "" {
			p.AppIcon = h.AppIcon
		}
	}
	p.Acked = someAcked && !someOpen
	p.NewSymptom = someAcked && someOpen
	if p.Severity == "critical" {
		p.AfterHours = false
	}
	p.Cause = alerts.CauseOf(types)
	p.Why = p.Cause.Why
	p.Serial = as[0].Serial
	p.Restaurant = as[0].RestaurantName
	p.RestID = as[0].RestaurantID
	p.IconKey = hs[0].IconKey
	for _, h := range hs {
		if h.Severity == p.Severity {
			p.IconKey = h.IconKey
			break
		}
	}
	subject := p.Serial
	if subject == "" {
		subject = hs[0].Headline
	}
	p.Title = subject + ": " + p.Cause.Title
	if p.Cause.Key == "other" {
		p.Title = hs[0].Headline
		if p.Serial != "" {
			p.Title = p.Serial + ": " + hs[0].Headline
		}
		p.Why = ""
	}
	if len(as) == 1 && p.Why == "" {
		p.Why = plainSentence(hs[0].Sentence)
	}
	if p.Serial != "" {
		p.Href, p.HrefLabel = "/devices/"+p.Serial, "Open device"
		for _, a := range as {
			if a.Type == "identity_conflict" {
				p.KeyReset = true
			}
		}
	}
	if hs[0].Primary != nil && len(as) == 1 && p.Cause.Key == "crash" {
		p.Href, p.HrefLabel = hs[0].Primary.Href, hs[0].Primary.Label
	}
	if n, ok := users[p.Assignee]; ok && n != "" {
		p.Assignee = n
	}
	p.IDs = strings.Join(ids, ",")
	p.Search = p.Title + " " + p.Serial + " " + p.Restaurant + " " + p.Assignee + p.Search
	return p
}

// symptomLabel names one alert inside a problem: its rule's catalog label, or for a
// crash the app that crashed.
func symptomLabel(h humanAlert) string {
	if h.Type == "device_crash" {
		if h.PackageName != "" {
			return "crash · " + h.PackageName
		}
		return "app crash"
	}
	if l := alertTypeLabel(h.Type); l != h.Type {
		return l
	}
	return h.Headline
}

func afterHours(detail json.RawMessage) bool {
	if len(detail) == 0 {
		return false
	}
	var d struct {
		AfterHours bool `json:"after_hours"`
	}
	_ = json.Unmarshal(detail, &d)
	return d.AfterHours
}

func spreadText(d time.Duration) string {
	if d < 2*time.Minute {
		return "a minute"
	}
	return fmt.Sprintf("%d minutes", int(d.Minutes()))
}

// problemReasons is the resolve-reason menu.
func problemReasons() []struct{ Key, Label string } { return db.ProblemReasons }

// problemsFromHumans builds problems from already-humanized alerts (the sneak-peek
// preview has no alert rows behind its cards).
func problemsFromHumans(hs []humanAlert) []problemView {
	as := make([]db.Alert, len(hs))
	for i, h := range hs {
		as[i] = db.Alert{ID: h.ID, Type: h.Type, Severity: h.Severity, Status: h.Status,
			Serial: h.Serial, RestaurantName: h.Restaurant, FiredAt: h.FiredAt, Occurrences: h.Occurrences}
	}
	return buildProblems(as, hs, nil)
}
