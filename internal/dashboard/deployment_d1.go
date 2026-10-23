package dashboard

import (
	"fmt"
	"html/template"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Deployment page D1 (picked 21 Oct 2026, with the state column S1): the Overview's
// language on one rollout. A to-do banner (devices that did not come back from their
// reboot, stopped downloads, failures, installs waiting for a manual reboot), Installed /
// In progress / Left cards, one device table with problems first and the state as a dot
// and a word with its progress under it, and a rail: installed ring, installs over time,
// venues, and how the rollout goes out (reboot, packages).
//
// There is no download cap since 23 Oct 2026 (the 3-per-venue / 8-in-all waves were
// removed on the user's ask): a waiting device simply has not checked in since the
// deployment started, and gets it when it does.

type dp1Row struct {
	Serial, Venue, VenueKey string
	InFleet                 bool
	From                    string // the build it is coming from
	Full                    bool   // takes the full image
	Legacy                  bool   // updated over otautil
	Key                     string // look | failed | dl | reboot | wait | ok | canceled
	LookOf                  string // for look: the step it is stuck at (dl | reboot)
	Word                    string
	Tone                    string // warn | bad | run | q | ok | muted
	Detail                  string
	ErrorCode               string
	Bar                     bool
	Pct                     int
	// actions (the template still checks the role)
	Retry, RetryFull, Reboot, Remove, Cancel, RetryStalled, Open bool
	order                                                       int
}

type dp1Todo struct {
	Title, Sub   string
	Action, Href string
	Post         string // when set, the action is a POST form to this URL
	Confirm      string
}

type dp1Banner struct {
	Kicker, Headline string
	Tone             string // "" (coral) | calm
	Todos            []dp1Todo
	More             int
	Line             string
}

type dp1Venue struct {
	Name, Href                      string
	Total, Done, Look, Active, Wait int
	offline                         int // waiting, but offline: not in the estimate
}

type dp1Bar struct {
	Pct   int
	Cur   bool
	Label template.HTML
	Title string
}

type dp1Rail struct {
	Pct            int
	Offset         string
	Tone           string
	Done, Total    int
	Sub            string
	Bars           []dp1Bar
	BarsCap        string
	Venues         []dp1Venue
	MoreVenues     int
	Reboot, RebootSub string
	Package, PackageSub string
}

type dp1View struct {
	Banner  dp1Banner
	Cards   []ovStat
	Rows    []dp1Row
	Rail    dp1Rail
	Venues  []string // filter pill
	Counts  map[string]int
	HasLook bool
}

type dp1Inputs struct {
	Dep         *db.Update
	Version     string
	Targets     []db.UpdateTarget
	Progress    map[uuid.UUID]otaRowView
	Legacy      []db.LegacyDeploymentDevice
	Incremental map[string]bool // source builds an active incremental covers
	HasFull     bool
	Rebootable  int
	CanOperate  bool
	Base        string // /releases/{id}/deployments/{did}
	SeenSecs    int    // a device not seen this long is offline
	Now         time.Time
}

const (
	dp1RebootGrace = 15 * time.Minute // a reboot that has not checked back in by then
	dp1DefaultDur  = 14 * time.Minute // per device, until one has finished
)

func dp1Mins(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	switch {
	case m < 1:
		return "under a minute"
	case m < 90:
		return strconv.Itoa(m) + " min"
	}
	h := float64(m) / 60
	if h < 10 {
		return strings.TrimSuffix(fmt.Sprintf("%.1f", h), ".0") + " h"
	}
	return strconv.Itoa(int(h+.5)) + " h"
}

func dp1Plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func buildDeployD1(in dp1Inputs) dp1View {
	now := in.Now
	active := in.Dep.Status == "active"
	offline := func(seen time.Time) bool {
		return in.SeenSecs > 0 && now.Sub(seen) > time.Duration(in.SeenSecs)*time.Second
	}
	v := dp1View{Counts: map[string]int{}}

	// Average time per device, from the ones that finished.
	var durSum time.Duration
	durN := 0
	for _, t := range in.Targets {
		if t.StartedAt != nil && t.CompletedAt != nil && t.CompletedAt.After(*t.StartedAt) {
			durSum += t.CompletedAt.Sub(*t.StartedAt)
			durN++
		}
	}
	avg := dp1DefaultDur
	if durN > 0 {
		avg = durSum / time.Duration(durN)
	}

	venueOf := func(t db.UpdateTarget) (string, string) {
		if t.VenueID == nil {
			return "No venue", ""
		}
		name := t.Venue
		if name == "" {
			name = "Venue"
		}
		return name, t.VenueID.String()
	}

	for _, t := range in.Targets {
		name, key := venueOf(t)
		r := dp1Row{Serial: t.SerialNumber, Venue: name, VenueKey: key, InFleet: true, ErrorCode: t.ErrorCode}
		r.From = t.SourceBuildID
		if r.From == "" && t.Status != "installed" {
			r.From = t.BuildID
		}
		r.Full = t.ForceFull || (r.From != "" && !in.Incremental[r.From] && in.HasFull)
		p, hasP := in.Progress[t.DeviceID]
		switch t.Status {
		case "installed":
			r.Key, r.Word, r.Tone = "ok", "Installed", "ok"
			if t.CompletedAt != nil {
				r.Detail = dp1Ago(now, *t.CompletedAt)
				if d := t.DurationSeconds(); d > 0 {
					r.Detail += " · took " + dp1Mins(time.Duration(d)*time.Second)
				}
			}
		case "failed":
			r.Key, r.Word, r.Tone = "failed", "Failed", "bad"
			r.Retry, r.RetryFull, r.Remove = true, true, true
		case "canceled":
			r.Key, r.Word, r.Tone, r.Detail = "canceled", "Canceled", "muted", "taken out of this rollout"
			r.Remove = true
		case "awaiting_reboot":
			r.Key, r.Word, r.Tone = "reboot", "Installed, needs a reboot", "run"
			switch in.Dep.RebootBehavior {
			case "scheduled":
				r.Detail = "reboots at the scheduled time"
			default:
				r.Detail = "reboot it to apply the update"
			}
			r.Reboot = true
		case "reboot_sent":
			sent := t.UpdatedAt
			if t.RebootSentAt != nil {
				sent = *t.RebootSentAt
			}
			if now.Sub(sent) > dp1RebootGrace {
				r.Key, r.Tone, r.LookOf = "look", "warn", "reboot"
				// The device often acks the reboot a minute or two after it is sent, so
				// "seen since" proves nothing; being online now with the old build does.
				if now.Sub(t.LastSeenAt) > 5*time.Minute {
					r.Word = "Not back from its reboot"
					r.Detail = "rebooted " + dp1Ago(now, sent) + ", nothing heard since " + dp1Ago(now, t.LastSeenAt)
				} else {
					r.Word = "Rebooted, not confirmed"
					r.Detail = "rebooted " + dp1Ago(now, sent) + ", still reports its old build"
				}
				r.Open = true
			} else {
				r.Key, r.Word, r.Tone = "reboot", "Rebooting", "run"
				r.Detail = "rebooted " + dp1Ago(now, sent) + " to apply it"
			}
		case "pending":
			r.Key, r.Word, r.Tone = "wait", "Waiting", "q"
			r.Remove = true
			switch {
			case !active:
				r.Detail = "not offered: the rollout is " + in.Dep.Status
			case offline(t.LastSeenAt):
				r.Detail = "offline · gets it when it is back"
			default:
				r.Detail = "gets it at its next check-in"
			}
		default: // downloading, installing and the client's live phases
			r.Key, r.Word, r.Tone = "dl", "Downloading", "run"
			r.Cancel = true
			last := t.UpdatedAt
			if hasP && p.SeenAt.After(last) {
				last = p.SeenAt
			}
			if hasP {
				r.Bar, r.Pct = true, p.Percent
				switch p.Phase {
				case "installing", "verifying", "finalizing":
					r.Word = "Installing"
				}
				bits := []string{}
				if s := p.Size(); s != "" {
					bits = append(bits, s)
				}
				bits = append(bits, strconv.Itoa(p.Percent)+"%")
				if t.StartedAt != nil {
					bits = append(bits, dp1Mins(now.Sub(*t.StartedAt)))
				}
				r.Detail = strings.Join(bits, " · ")
			} else {
				r.Detail = "offered, not started yet"
			}
			switch {
			case now.Sub(last) > 30*time.Minute:
				r.Key, r.Tone, r.RetryStalled, r.Open, r.LookOf = "look", "warn", true, true, "dl"
				if offline(t.LastSeenAt) {
					r.Word, r.Detail = "Download stopped", "the device went offline "+dp1Ago(now, t.LastSeenAt)
				} else {
					r.Word, r.Detail = "Download stopped", "nothing for "+dp1Mins(now.Sub(last))+" while the device stayed online"
				}
			case hasP && !p.AdvancedAt.IsZero() && now.Sub(p.AdvancedAt) > 15*time.Minute:
				r.Key, r.Tone, r.Open, r.LookOf = "look", "warn", true, "dl"
				r.Word = "Stuck at " + strconv.Itoa(p.Percent) + "%"
				r.Detail = "still reporting, not moving for " + dp1Mins(now.Sub(p.AdvancedAt))
			}
		}
		v.Rows = append(v.Rows, r)
	}
	for _, dv := range in.Legacy {
		r := dp1Row{Serial: dv.Serial, Venue: "No venue", InFleet: dv.InFleet, From: dv.BuildID, Legacy: true}
		switch dv.Status {
		case "installed", "updated":
			r.Key, r.Word, r.Tone, r.Detail = "ok", "Installed", "ok", dp1Ago(now, dv.UpdatedAt)
		case "awaiting_reboot":
			r.Key, r.Word, r.Tone, r.Detail, r.Reboot = "reboot", "Installed, needs a reboot", "run", "reboot it to apply the update", dv.InFleet
		case "failed":
			r.Key, r.Word, r.Tone, r.Detail, r.Retry = "failed", "Failed", "bad", dv.Error, true
		case "offered", "downloading", "installing", "verifying", "finalizing":
			r.Key, r.Word, r.Tone, r.Bar, r.Pct = "dl", "Downloading", "run", true, dv.Percent
			if dv.Status != "offered" && dv.Status != "downloading" {
				r.Word = "Installing"
			}
			if dv.Status == "installing" && dv.Percent >= 100 {
				r.Key, r.Word, r.Bar, r.Detail, r.Reboot = "reboot", "Installed, needs a reboot", false, "reboot it to apply the update", dv.InFleet
			} else {
				r.Detail = strconv.Itoa(dv.Percent) + "%"
			}
		default:
			r.Key, r.Word, r.Tone, r.Detail = "wait", "Waiting", "q", "offered over the legacy path"
		}
		v.Rows = append(v.Rows, r)
	}
	order := map[string]int{"look": 0, "failed": 1, "dl": 2, "reboot": 3, "wait": 4, "ok": 5, "canceled": 6}
	for i := range v.Rows {
		v.Rows[i].order = order[v.Rows[i].Key]
		v.Counts[v.Rows[i].Key]++
	}
	sort.SliceStable(v.Rows, func(i, j int) bool {
		if v.Rows[i].order != v.Rows[j].order {
			return v.Rows[i].order < v.Rows[j].order
		}
		return v.Rows[i].Serial < v.Rows[j].Serial
	})
	v.HasLook = v.Counts["look"] > 0

	total := len(v.Rows)
	installed := v.Counts["ok"]
	inLine, offlineWait := 0, 0
	venues := map[string]*dp1Venue{}
	var venueOrder []string
	for _, r := range v.Rows {
		k := r.VenueKey
		if k == "" {
			k = "-"
		}
		vv := venues[k]
		if vv == nil {
			vv = &dp1Venue{Name: r.Venue}
			if r.VenueKey != "" {
				vv.Href = "/restaurants/" + r.VenueKey
			}
			venues[k] = vv
			venueOrder = append(venueOrder, k)
		}
		vv.Total++
		switch r.Key {
		case "ok":
			vv.Done++
		case "look", "failed":
			vv.Look++
		case "dl", "reboot":
			vv.Active++
		case "wait":
			vv.Wait++
			if strings.HasPrefix(r.Detail, "offline") {
				offlineWait++
				vv.offline++
			} else {
				inLine++
			}
		}
	}

	// When it should be done: everything downloads at once, so about one download.
	var eta time.Duration
	busiest, busiestN, waitVenues := "", 0, 0
	totalLeft := 0
	for _, k := range venueOrder {
		vv := venues[k]
		left := vv.Active + vv.Wait - vv.offline
		totalLeft += left
		if vv.Wait > vv.offline {
			waitVenues++
		}
		if left == 0 {
			continue
		}
		eta = avg
		if n := vv.Wait - vv.offline; n > busiestN {
			busiest, busiestN = vv.Name, n
		}
	}
	toGo := ""
	if active && totalLeft > 0 {
		toGo = "about " + dp1Mins(eta) + " to go"
	}

	// Banner.
	b := dp1Banner{Kicker: "Rollout · " + in.Version + " · " + strconv.Itoa(dp1Pct(installed, total)) + "% installed"}
	var todos []dp1Todo
	for _, r := range v.Rows {
		if r.Key != "look" {
			continue
		}
		title := r.Serial + " hasn't come back from its reboot"
		switch {
		case strings.HasPrefix(r.Word, "Rebooted"):
			title = r.Serial + " rebooted but still reports its old build"
		case strings.HasPrefix(r.Word, "Download stopped"):
			title = r.Serial + ": its download stopped"
		case strings.HasPrefix(r.Word, "Stuck"):
			title = r.Serial + " is stuck at " + strconv.Itoa(r.Pct) + "%"
		}
		todos = append(todos, dp1Todo{Title: title, Sub: r.Venue + " · " + r.Detail, Action: "Open device", Href: "/devices/" + r.Serial})
	}
	if n := v.Counts["failed"]; n == 1 {
		for _, r := range v.Rows {
			if r.Key == "failed" {
				t := dp1Todo{Title: r.Serial + " failed", Sub: r.Venue + " · " + otaErrorShort(r.ErrorCode), Action: "Open device", Href: "/devices/" + r.Serial}
				if in.CanOperate && !r.Legacy {
					t.Action, t.Href, t.Post = "Retry", "", in.Base+"/devices/"+r.Serial+"/retry"
				}
				todos = append(todos, t)
			}
		}
	} else if n > 1 {
		todos = append(todos, dp1Todo{Title: strconv.Itoa(n) + " devices failed", Sub: "Retry each from the table, or with the full image", Action: "See them", Href: "#dp1-devices"})
	}
	if n := v.Counts["reboot"]; n > 0 && in.Dep.RebootBehavior == "manual" && in.Rebootable > 0 {
		t := dp1Todo{Title: dp1Plural(n, "device has", "devices have") + " the update, waiting for a reboot", Sub: "This rollout reboots by hand", Action: "See them", Href: "#dp1-devices"}
		if in.CanOperate {
			t.Action, t.Href, t.Post = "Reboot all", "", in.Base+"/reboot-all"
			t.Confirm = "Reboot all " + strconv.Itoa(in.Rebootable) + " installed device(s) now to apply the update?"
		}
		todos = append(todos, t)
	}
	if len(todos) > ovTodoRows {
		b.More = len(todos) - ovTodoRows
		todos = todos[:ovTodoRows]
	}
	b.Todos = todos
	headN := len(todos) + b.More
	switch {
	case headN > 0:
		b.Headline = dp1Plural(headN, "thing", "things") + " to look at"
	case in.Dep.Status == "canceled":
		b.Tone, b.Headline = "calm", "Canceled"
	case installed == total && total > 0:
		b.Tone, b.Headline = "calm", "On all "+strconv.Itoa(total)+" devices"
	case active && totalLeft > 0:
		b.Tone, b.Headline = "calm", "Rolling out · "+toGo
	default:
		b.Tone, b.Headline = "calm", "Nothing more to do here"
	}
	if active && inLine > 0 {
		lead := ""
		if headN > 0 {
			lead = "The other "
		}
		if waitVenues == 1 && busiest != "No venue" {
			b.Line = fmt.Sprintf("%s%s at %s %s online and get%s it at %s next check-in.",
				lead, dp1Plural(inLine, "device", "devices"), busiest, dp1IsAre(inLine), dp1S(inLine), dp1Their(inLine))
		} else {
			b.Line = fmt.Sprintf("%s%s %s online and get%s it at %s next check-in.",
				lead, dp1Plural(inLine, "device", "devices"), dp1IsAre(inLine), dp1S(inLine), dp1Their(inLine))
		}
		if lead == "" {
			b.Line = strings.ToUpper(b.Line[:1]) + b.Line[1:]
		}
	}
	if active && offlineWait > 0 {
		off := dp1Plural(offlineWait, "device is", "devices are") + " offline and will get it when back."
		if b.Line != "" {
			off = " " + off
		}
		b.Line += off
	}
	if b.Line == "" && b.Tone == "calm" && in.Dep.Status == "active" && totalLeft == 0 && installed < total {
		b.Line = "Nothing is downloading or waiting: what is left needs a look in the table."
	}
	v.Banner = b

	// Cards.
	recent := 0
	for _, t := range in.Targets {
		if t.Status == "installed" && t.CompletedAt != nil && now.Sub(*t.CompletedAt) <= 15*time.Minute {
			recent++
		}
	}
	instCard := ovStat{Title: "Installed", Href: "#dp1-devices",
		A: ovMetric{Value: strconv.Itoa(installed), Label: "of " + strconv.Itoa(total) + " devices", Tone: "good"},
		B: ovMetric{Value: "—", Label: "average per device", Tone: "flat"}}
	if recent > 0 {
		instCard.A.Delta = "▲ " + strconv.Itoa(recent) + " last 15 min"
	}
	if durN > 0 {
		instCard.B.Value = dp1Mins(avg)
	}
	instCard.LineA, _, instCard.Fill = ovLines(dp1Cumulative(in.Targets, in.Dep.CreatedAt, now, 24), nil)
	// A device stuck at a step still counts at that step; the delta says it is stuck.
	stuckDl, stuckReb := 0, 0
	for _, r := range v.Rows {
		switch r.LookOf {
		case "dl":
			stuckDl++
		case "reboot":
			stuckReb++
		}
	}
	dl := v.Counts["dl"] + stuckDl
	reb := v.Counts["reboot"] + stuckReb
	prog := ovStat{Title: "In progress", Href: "#dp1-devices",
		A: ovMetric{Value: strconv.Itoa(dl), Label: "downloading now", Tone: "flat"},
		B: ovMetric{Value: strconv.Itoa(reb), Label: "rebooting", Tone: "flat"}}
	if in.Dep.RebootBehavior != "immediate" {
		prog.B.Label = "installed, to reboot"
	}
	if active && dl > 0 {
		busy := 0
		for _, k := range venueOrder {
			if venues[k].Active > 0 {
				busy++
			}
		}
		prog.A.Delta = "at " + dp1Plural(busy, "venue", "venues")
	}
	if stuckDl > 0 {
		prog.A.Delta, prog.A.Tone = strconv.Itoa(stuckDl)+" stuck", "bad"
	}
	if stuckReb > 0 {
		prog.B.Delta, prog.B.Tone = strconv.Itoa(stuckReb)+" not back", "bad"
	}
	left := ovStat{Title: "Left", Href: "#dp1-devices",
		A: ovMetric{Value: strconv.Itoa(v.Counts["wait"]), Label: "waiting", Tone: "flat"},
		B: ovMetric{Value: strconv.Itoa(v.Counts["failed"]), Label: "failed", Tone: "good"}}
	if toGo != "" {
		left.A.Delta = toGo
	}
	if v.Counts["failed"] > 0 {
		left.B.Tone, left.B.Delta = "bad", "retry from the table"
	}
	v.Cards = []ovStat{instCard, prog, left}

	// Rail.
	rl := dp1Rail{Pct: dp1Pct(installed, total), Done: installed, Total: total}
	rl.Offset = fmt.Sprintf("%.1f", 326.7*float64(100-rl.Pct)/100)
	switch {
	case v.Counts["failed"] > 0 || v.Counts["look"] > 0:
		rl.Tone = "warn"
	}
	subs := []string{}
	if toGo != "" {
		subs = append(subs, toGo)
	}
	if n := v.Counts["failed"]; n > 0 || active {
		subs = append(subs, strconv.Itoa(n)+" failed")
	}
	rl.Sub = strings.Join(subs, " · ")
	rl.Bars, rl.BarsCap = dp1InstallBars(in.Targets, in.Dep.CreatedAt, now)
	sort.SliceStable(venueOrder, func(i, j int) bool {
		a, c := venues[venueOrder[i]], venues[venueOrder[j]]
		la, lc := a.Total-a.Done, c.Total-c.Done
		if (la == 0) != (lc == 0) {
			return la > 0
		}
		if la != lc {
			return la > lc
		}
		return a.Name < c.Name
	})
	for i, k := range venueOrder {
		if i == 6 {
			rl.MoreVenues = len(venueOrder) - 6
			break
		}
		rl.Venues = append(rl.Venues, *venues[k])
	}
	switch in.Dep.RebootBehavior {
	case "immediate":
		rl.Reboot, rl.RebootSub = "Immediate", "restarts as soon as it is installed"
	case "scheduled":
		rl.Reboot, rl.RebootSub = "Scheduled", "restarts at the scheduled time"
		if in.Dep.ScheduledTime != nil {
			rl.RebootSub = "restarts " + in.Dep.ScheduledTime.UTC().Format("Jan 2 15:04") + " UTC"
		}
	default:
		rl.Reboot, rl.RebootSub = "Manual", "installs, then waits for someone to reboot it"
	}
	incN, fullN := 0, 0
	fromInc, fromFull := map[string]int{}, map[string]int{}
	for _, r := range v.Rows {
		if r.Legacy || r.From == "" {
			continue
		}
		if r.Full {
			fullN++
			fromFull[r.From]++
		} else {
			incN++
			fromInc[r.From]++
		}
	}
	parts := []string{}
	if incN > 0 {
		parts = append(parts, strconv.Itoa(incN)+" incremental")
	}
	if fullN > 0 {
		parts = append(parts, strconv.Itoa(fullN)+" full")
	}
	rl.Package = strings.Join(parts, ", ")
	sub := []string{}
	if len(fromInc) > 0 {
		sub = append(sub, "from "+strings.Join(dp1TopKeys(fromInc, 2), ", "))
	}
	if len(fromFull) > 0 {
		ks := dp1TopKeys(fromFull, 2)
		verb := "takes"
		if len(ks) > 1 || fromFull[ks[0]] > 1 {
			verb = "take"
		}
		sub = append(sub, strings.Join(ks, ", ")+" "+verb+" the full image")
	}
	rl.PackageSub = strings.Join(sub, " · ")
	v.Rail = rl

	seen := map[string]bool{}
	for _, r := range v.Rows {
		if !seen[r.Venue] {
			seen[r.Venue] = true
			v.Venues = append(v.Venues, r.Venue)
		}
	}
	sort.Strings(v.Venues)
	return v
}

func dp1Pct(a, b int) int {
	if b == 0 {
		return 0
	}
	return a * 100 / b
}

func dp1IsAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func dp1Their(n int) string {
	if n == 1 {
		return "its"
	}
	return "their"
}

func dp1They(n int) string {
	if n == 1 {
		return "it"
	}
	return "they"
}

// topKeys returns up to n keys, biggest count first.
func dp1TopKeys(m map[string]int, n int) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		if m[ks[i]] != m[ks[j]] {
			return m[ks[i]] > m[ks[j]]
		}
		return ks[i] < ks[j]
	})
	if len(ks) > n {
		ks = ks[:n]
	}
	return ks
}

// otaErrorShort is a one-line reason for the banner; the table carries the long text.
func otaErrorShort(code string) string {
	switch code {
	case "":
		return "no reason reported"
	case "stalled":
		return "went quiet mid-update"
	case "DOWNLOAD_ERROR":
		return "the download failed"
	case "REBOOT_NO_CHECKIN":
		return "rebooted and never checked in"
	case "SLOT_SWITCH_FAILED":
		return "came back on the old build"
	case "CANCELLED":
		return "canceled"
	}
	return code
}

// dp1Cumulative is devices installed so far at n points from start to now.
func dp1Cumulative(ts []db.UpdateTarget, start, now time.Time, n int) []float64 {
	if !now.After(start) || n < 2 {
		return nil
	}
	out := make([]float64, n)
	span := now.Sub(start)
	for i := range out {
		at := start.Add(span * time.Duration(i) / time.Duration(n-1))
		c := 0
		for _, t := range ts {
			if t.Status == "installed" && t.CompletedAt != nil && !t.CompletedAt.After(at) {
				c++
			}
		}
		out[i] = float64(c)
	}
	return out
}

// dp1InstallBars buckets installs since the rollout began into at most eight bars,
// in a step that fits (10 min up to a day).
func dp1InstallBars(ts []db.UpdateTarget, start, now time.Time) ([]dp1Bar, string) {
	var last time.Time
	for _, t := range ts {
		if t.CompletedAt != nil && t.CompletedAt.After(last) {
			last = *t.CompletedAt
		}
	}
	end := now
	if !last.IsZero() && now.Sub(last) > 24*time.Hour {
		end = last // a finished rollout: draw it, not the days since
	}
	steps := []struct {
		d    time.Duration
		name string
	}{{10 * time.Minute, "10 minutes"}, {30 * time.Minute, "half hour"}, {time.Hour, "hour"}, {6 * time.Hour, "6 hours"}, {24 * time.Hour, "day"}}
	step := steps[len(steps)-1]
	for _, s := range steps {
		if end.Sub(start) <= 8*s.d {
			step = s
			break
		}
	}
	from := start.Truncate(step.d)
	n := int(end.Sub(from)/step.d) + 1
	if n > 8 {
		from = end.Truncate(step.d).Add(-7 * step.d)
		n = 8
	}
	if n < 2 {
		n = 2
	}
	counts := make([]int, n)
	maxC := 0
	for _, t := range ts {
		if t.Status != "installed" || t.CompletedAt == nil || t.CompletedAt.Before(from) {
			continue
		}
		i := int(t.CompletedAt.Sub(from) / step.d)
		if i >= 0 && i < n {
			counts[i]++
		}
	}
	for _, c := range counts {
		if c > maxC {
			maxC = c
		}
	}
	bars := make([]dp1Bar, n)
	for i, c := range counts {
		at := from.Add(time.Duration(i) * step.d)
		h := 4
		if maxC > 0 && c > 0 {
			h = c * 100 / maxC
			if h < 8 {
				h = 8
			}
		}
		lbl := `<span class="js-local-time" data-utc="` + at.UTC().Format(time.RFC3339) + `" data-format="short">` + at.UTC().Format("15:04") + `</span>`
		if step.d >= 24*time.Hour {
			lbl = at.UTC().Format("Jan 2")
		}
		bars[i] = dp1Bar{Pct: h, Cur: i == n-1, Label: template.HTML(lbl), Title: dp1Plural(c, "install", "installs")}
	}
	return bars, "installs per " + step.name
}

// dp1Ago is agoShort against the page's clock rather than the wall clock.
func dp1Ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h ago"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d ago"
}

// dp1S is the verb's s: "1 device gets", "3 devices get".
func dp1S(n int) string {
	if n == 1 {
		return "s"
	}
	return ""
}
