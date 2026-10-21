package dashboard

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Lineup L1 (picked 21 Oct 2026, from the Lineup demos): the role pane in the Overview's
// language. Roles are a chip row; the banner is this role's to-do list (a newer build to
// promote, a model with no prod firmware, devices on another line, a client left behind
// with auto-update off) with the model's picture; three cards show how far firmware, the
// MDM client and the app have got; models are picture cards; a 30-day firmware timeline
// per model; the rail has the "fully on lineup" ring, devices moved per day, what needs
// a look and the move window. What the pane can do is unchanged.

// luTodo is one banner row. A promotion posts the same form the firmware lane did.
type luTodo struct {
	Title, Sub   string
	Action, Href string
	// Promote posts /pipeline/firmware with these, after Confirm.
	Promote   bool
	Model     string
	ReleaseID int
	Confirm   string
}

type luBanner struct {
	Tone     string // warn | calm
	Kicker   string
	Headline string
	Todos    []luTodo
	Calm     string
	Pic      *modelPicture
}

// luSeg is one slice of a card's progress bar.
type luSeg struct {
	Pct   string
	Tone  string // ok | wait | other | none
	Title string
}

type luCard struct {
	Title, Href string
	A, B        ovMetric
	Bar         []luSeg
	Empty       string
}

type luMoveBar struct {
	Label  string
	Pct    int
	Val    int
	Cur    bool
	Detail string
}

type luLook struct {
	Tone, Title, Sub, Href string
}

type luRail struct {
	Pct        int
	Offset     string // ring stroke-dashoffset (r52, 326.7)
	Full       int
	Deployed   int
	Tone       string // ok | warn | danger
	Bars       []luMoveBar
	MovedWeek  int
	Looks      []luLook
	HasPctData bool
}

// luTimeline is one model's firmware over the last 30 days.
type luTimeline struct {
	Key, Label string
	Rows       []ovBar
	Days       []string
	NowPct     string
}

type luPane struct {
	Banner    luBanner
	Cards     []luCard
	Rail      luRail
	Timelines map[string]luTimeline
}

// luInputs is everything the pane is built from, so tests and the handler share it.
type luInputs struct {
	RoleKey, RoleLabel string
	Models             []pipeModel
	HW                 []hwModel
	App                lineupApp
	Clients            []*ClientSlotView
	Full, Deployed     int // deployed devices fully on the lineup, of all deployed
	Moves              []db.RoleMoveDay
	History            map[string][]db.FirmwareProdChange
	Pic                *modelPicture
	CanEdit            bool
	Now                time.Time
}

func luPct(n, of int) string {
	if of <= 0 {
		return "0"
	}
	return fmt.Sprintf("%.2f", float64(n)*100/float64(of))
}

func buildLineupPane(in luInputs) luPane {
	var p luPane
	p.Banner = buildLuBanner(in)
	p.Cards = buildLuCards(in)
	p.Rail = buildLuRail(in)
	p.Timelines = map[string]luTimeline{}
	for _, m := range in.Models {
		p.Timelines[m.Key] = buildLuTimeline(m, in.History[m.Key], in.Now)
	}
	return p
}

func buildLuBanner(in luInputs) luBanner {
	b := luBanner{Tone: "calm", Pic: in.Pic}
	nModels := len(in.HW)
	devices := 0
	for _, m := range in.HW {
		devices += m.Devices
	}
	kick := []string{in.RoleLabel}
	if nModels == 1 {
		kick = append(kick, in.HW[0].Display)
	} else if nModels > 1 {
		kick = append(kick, fmt.Sprintf("%d models", nModels))
	}
	kick = append(kick, fmt.Sprintf("%d device%s", devices, plural(devices)))
	b.Kicker = strings.Join(kick, " · ")

	var todos []luTodo
	for _, m := range in.Models {
		switch {
		case m.Mode == "prod" && m.Channel != nil && len(m.Newer) > 0:
			nw := m.Newer[0]
			when := "published"
			if nw.PublishedAt != nil {
				when = "published " + agoShort(*nw.PublishedAt)
			}
			t := luTodo{
				Title: fmt.Sprintf("%s is newer than prod %s", nw.Version, m.Channel.Version),
				Sub:   fmt.Sprintf("%s · %s · deployed devices move in their window and reboot", m.Label, when),
			}
			if len(in.Models) == 1 {
				t.Sub = fmt.Sprintf("%s · deployed devices move in their window and reboot", when)
			}
			if in.CanEdit {
				t.Action, t.Promote, t.Model, t.ReleaseID = "Promote to prod", true, m.Key, nw.ID
				t.Confirm = fmt.Sprintf("Make %s prod firmware for %s?\n\nDeployed devices on the same line move to it in their quiet window, and reboot.", nw.Version, m.Label)
			} else {
				t.Action, t.Href = "Open release", fmt.Sprintf("/releases/%d", nw.ID)
			}
			todos = append(todos, t)
		case m.Mode == "prod" && m.Channel == nil && len(m.Releases) > 0:
			todos = append(todos, luTodo{Title: "No prod firmware for " + m.Label,
				Sub:    "deployed devices are left alone until one is set",
				Action: "Pick one", Href: "#fw-" + m.Key})
		}
		if m.Mode == "prod" && m.OtherLine > 0 {
			todos = append(todos, luTodo{
				Title: fmt.Sprintf("%d deployed device%s on another firmware line", m.OtherLine, plural(m.OtherLine)),
				Sub:   fmt.Sprintf("%s · prod only moves devices on the v%s line, migrate these with a rollout you watch", m.Label, m.Line),
				Action: "Releases", Href: "/releases"})
		}
	}
	for _, c := range in.Clients {
		if c.Hosted && c.Behind > 0 && (!c.Auto.On || c.Auto.Paused) {
			state := "off"
			if c.Auto.On {
				state = "paused"
			}
			todos = append(todos, luTodo{
				Title:  fmt.Sprintf("%d behind %s %s", c.Behind, c.Label, c.Build.Version),
				Sub:    "auto-update is " + state + " for this client, so they stay where they are",
				Action: "Open client", Href: "/lineup?client=" + c.Slot})
		}
	}
	b.Todos = todos
	if len(todos) > 0 {
		b.Tone = "warn"
		thing := "things"
		if len(todos) == 1 {
			thing = "thing"
		}
		b.Headline = fmt.Sprintf("%d %s to decide", len(todos), thing)
		return b
	}
	b.Headline = in.RoleLabel + " is on its lineup"
	var bits []string
	for _, m := range in.Models {
		if m.Mode == "prod" && m.Channel != nil {
			bits = append(bits, "prod firmware "+m.Channel.Version)
			break
		}
	}
	for _, c := range in.Clients {
		if c.Hosted {
			bits = append(bits, c.Label+" "+c.Build.Version)
			break
		}
	}
	if in.App.Name != "" {
		bits = append(bits, in.App.Name)
	}
	if len(bits) == 0 {
		b.Calm = "Nothing to decide here."
	} else {
		b.Calm = "Nothing to decide: " + strings.Join(bits, " · ") + "."
	}
	if nModels == 0 {
		b.Headline = "No models in " + in.RoleLabel + " yet"
		b.Calm = "Attach a model below; its devices take this role now and at every later enrollment."
	}
	return b
}

func buildLuCards(in luInputs) []luCard {
	var fwMoved, clMoved int
	for _, m := range in.Moves {
		fwMoved += m.Firmware
		clMoved += m.Client
	}
	// Firmware: every model in prod mode with a prod build.
	fw := luCard{Title: "Firmware", Href: "#firmware"}
	on, behind, other, total := 0, 0, 0, 0
	var vers []string
	for _, m := range in.Models {
		if m.Mode != "prod" || m.Channel == nil {
			continue
		}
		on, behind, other, total = on+m.On, behind+m.Behind, other+m.OtherLine, total+m.Total
		vers = append(vers, m.Channel.Version)
	}
	if len(vers) == 0 {
		fw.Empty = "No prod firmware here: these models run their maker's firmware, or are lab only."
	} else {
		pct := 0
		if total > 0 {
			pct = int(math.Round(float64(on) * 100 / float64(total)))
		}
		lbl := "on prod firmware"
		if len(vers) == 1 {
			lbl = "on prod " + vers[0]
		}
		fw.A = ovMetric{Value: fmt.Sprintf("%d%%", pct), Label: lbl, Tone: "flat"}
		if fwMoved > 0 {
			fw.A.Delta, fw.A.Tone = fmt.Sprintf("▲ %d this week", fwMoved), "good"
		}
		fw.B = ovMetric{Value: fmt.Sprintf("%d", behind), Label: "waiting for their window", Tone: "flat"}
		fw.Bar = []luSeg{{luPct(on, total), "ok", fmt.Sprintf("%d on prod", on)},
			{luPct(behind, total), "wait", fmt.Sprintf("%d waiting", behind)},
			{luPct(other, total), "other", fmt.Sprintf("%d on another line", other)}}
	}

	// MDM client: the role's devices against each client's hosted build.
	cl := luCard{Title: "MDM client"}
	up, devs, beh, nob, unk := 0, 0, 0, 0, 0
	var first *ClientSlotView
	for _, c := range in.Clients {
		if !c.Hosted {
			continue
		}
		if first == nil {
			first = c
		}
		up, devs, beh, nob, unk = up+c.UpToDate, devs+c.Devices, beh+c.Behind, nob+c.NoBuild, unk+c.Unknown
	}
	if first == nil {
		cl.Empty = "No hosted client build for these devices."
	} else {
		cl.Href = "/lineup?client=" + first.Slot
		lbl := "on the hosted client"
		if len(in.Clients) == 1 {
			lbl = "on hosted " + first.Build.Version
		}
		cl.A = ovMetric{Value: fmt.Sprintf("%d", up), Label: lbl, Tone: "flat"}
		if clMoved > 0 {
			cl.A.Delta, cl.A.Tone = fmt.Sprintf("▲ %d this week", clMoved), "good"
		}
		auto := "Off"
		sub := "devices keep the client they have"
		if first.Auto.On {
			auto = "On"
			if first.Auto.Paused {
				auto = "Paused"
			}
			sub = "any time of day"
			if first.Auto.WindowStart != "" && first.Auto.WindowEnd != "" {
				sub = first.Auto.WindowStart + "–" + first.Auto.WindowEnd + " local"
			}
		}
		cl.B = ovMetric{Value: auto, Label: "auto-update · " + sub, Tone: "flat"}
		cl.Bar = []luSeg{{luPct(up, devs), "ok", fmt.Sprintf("%d up to date", up)},
			{luPct(beh, devs), "wait", fmt.Sprintf("%d behind", beh)},
			{luPct(nob, devs), "other", fmt.Sprintf("%d on an image that cannot take it", nob)},
			{luPct(unk, devs), "none", fmt.Sprintf("%d not reporting a version", unk)}}
	}

	// App: the role's app on its devices.
	app := luCard{Title: "App"}
	switch {
	case in.App.Base == "":
		app.Empty = "No app for " + in.RoleLabel + ". Lock on these devices offers the app picker instead."
	case in.App.Total == 0:
		app.Title = "App · " + in.App.Name
		app.Empty = "No " + in.RoleLabel + " devices yet."
	default:
		app.Title = "App · " + in.App.Name
		app.A = ovMetric{Value: fmt.Sprintf("%d", in.App.On), Label: fmt.Sprintf("with %s, of %d", in.App.Name, in.App.Total), Tone: "flat"}
		miss := in.App.Total - in.App.On
		app.B = ovMetric{Value: fmt.Sprintf("%d", miss), Label: "without it", Tone: "flat"}
		for _, bd := range in.App.Builds {
			tone := bd.EnvTone
			if tone == "" {
				tone = "none"
			}
			app.Bar = append(app.Bar, luSeg{fmt.Sprintf("%.2f", bd.Pct), "env-" + tone, fmt.Sprintf("%s: %d", bd.Env, bd.Devices)})
		}
	}
	return []luCard{fw, cl, app}
}

func buildLuRail(in luInputs) luRail {
	r := luRail{Full: in.Full, Deployed: in.Deployed, HasPctData: in.Deployed > 0}
	if in.Deployed > 0 {
		r.Pct = int(math.Round(float64(in.Full) * 100 / float64(in.Deployed)))
	}
	r.Offset = fmt.Sprintf("%.1f", 326.7*float64(100-r.Pct)/100)
	switch {
	case r.Pct >= 90:
		r.Tone = "ok"
	case r.Pct >= 60:
		r.Tone = "warn"
	default:
		r.Tone = "danger"
	}
	peak := 0
	for _, m := range in.Moves {
		if n := m.Firmware + m.Client; n > peak {
			peak = n
		}
	}
	for i, m := range in.Moves {
		n := m.Firmware + m.Client
		r.MovedWeek += n
		pct := 4
		if peak > 0 && n > 0 {
			if pct = n * 100 / peak; pct < 8 {
				pct = 8
			}
		}
		r.Bars = append(r.Bars, luMoveBar{Label: m.Day.Format("Mon")[:1], Pct: pct, Val: n, Cur: i == len(in.Moves)-1,
			Detail: fmt.Sprintf("%s: %d firmware, %d client", m.Day.Format("Mon 2"), m.Firmware, m.Client)})
	}

	q := "/devices?class=" + in.RoleKey
	for _, m := range in.Models {
		if m.Mode == "prod" && m.Channel != nil && m.Behind > 0 {
			r.Looks = append(r.Looks, luLook{"info", fmt.Sprintf("%d waiting for %s", m.Behind, m.Channel.Version),
				m.Label + " · they move inside the window, when present", q})
		}
	}
	if in.App.Base != "" && in.App.Total > in.App.On {
		n := in.App.Total - in.App.On
		r.Looks = append(r.Looks, luLook{"warn", fmt.Sprintf("%d without %s", n, in.App.Name),
			"Lock offers the app picker on these", q})
	}
	for _, c := range in.Clients {
		if c.Unknown > 0 {
			r.Looks = append(r.Looks, luLook{"", fmt.Sprintf("%d not reporting a %s version", c.Unknown, c.Label),
				"an older image, or a client from before version reporting", "/lineup?client=" + c.Slot})
		}
		if c.NoBuild > 0 {
			r.Looks = append(r.Looks, luLook{"", fmt.Sprintf("%d can't take %s %s", c.NoBuild, c.Label, c.Build.Version),
				"their image is signed with another key; they get no client update", "/lineup?client=" + c.Slot})
		}
	}
	return r
}

// buildLuTimeline lays a model's firmware on a 30-day track: each prod period from the
// audit log (the current one with its devices on it), newer builds still to promote,
// and builds published while the model is lab only.
func buildLuTimeline(m pipeModel, hist []db.FirmwareProdChange, now time.Time) luTimeline {
	t := luTimeline{Key: m.Key, Label: m.Label}
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	start := dayStart.AddDate(0, 0, -29)
	end := dayStart.AddDate(0, 0, 1)
	span := end.Sub(start).Seconds()
	for i := 0; i < 7; i++ { // a label every ~4 days, the last is Today
		d := start.AddDate(0, 0, i*5)
		l := d.Format("2 Jan")
		if i == 6 {
			l = "Today"
		}
		t.Days = append(t.Days, l)
	}
	pos := func(x time.Time) float64 {
		return math.Max(0, math.Min(100, x.Sub(start).Seconds()/span*100))
	}
	t.NowPct = fmt.Sprintf("%.2f", pos(now))

	type period struct {
		ver, by  string
		from, to time.Time
		current  bool
	}
	var ps []period
	for i, h := range hist {
		to := now
		if i+1 < len(hist) {
			to = hist[i+1].At
		}
		if n := len(ps); n > 0 && ps[n-1].ver == h.Version {
			ps[n-1].to = to
			continue
		}
		ps = append(ps, period{ver: h.Version, by: h.By, from: h.At, to: to})
	}
	if m.Channel != nil && m.Channel.Version != "" {
		if n := len(ps); n == 0 || ps[n-1].ver != m.Channel.Version {
			ps = append(ps, period{ver: m.Channel.Version, by: m.Channel.SetBy, from: m.Channel.SetAt, to: now})
		}
		ps[len(ps)-1].current = true
	}
	for _, p := range ps {
		if p.to.Before(start) {
			continue
		}
		who, _, _ := ovWho(p.by)
		b := ovBar{Name: p.ver, Icon: "FW", Tone: "ok", Count: "was prod",
			Sub: fmt.Sprintf("prod %s–%s · %s", p.from.Format("2 Jan"), p.to.Format("2 Jan"), who)}
		if p.current {
			b.Tone, b.Count = "run", fmt.Sprintf("%d / %d", m.On, m.Total)
			b.Sub = fmt.Sprintf("prod since %s · %s", p.from.Format("2 Jan"), who)
		}
		l, r := pos(p.from), pos(p.to)
		b.Left, b.Width = fmt.Sprintf("%.2f", l), fmt.Sprintf("%.2f", r-l)
		b.Title = b.Name + " · " + b.Sub
		t.Rows = append(t.Rows, b)
	}
	addRel := func(rel db.Release, tone, count, sub string) {
		at := rel.CreatedAt
		if rel.PublishedAt != nil {
			at = *rel.PublishedAt
		}
		if at.Before(start) {
			at = start
		}
		b := ovBar{Name: rel.Version, Icon: "NEW", Tone: tone, Count: count, Sub: sub + " " + at.Format("2 Jan"),
			Href: fmt.Sprintf("/releases/%d", rel.ID)}
		if tone == "q" {
			b.Icon = "LB"
		}
		l := pos(at)
		b.Left, b.Width = fmt.Sprintf("%.2f", l), fmt.Sprintf("%.2f", 100-l)
		b.Title = b.Name + " · " + b.Sub
		t.Rows = append(t.Rows, b)
	}
	switch m.Mode {
	case "prod":
		for i := len(m.Newer) - 1; i >= 0; i-- {
			addRel(m.Newer[i], "warn", "newer", "published")
		}
	case "lab":
		for i := len(m.Releases) - 1; i >= 0 && i >= len(m.Releases)-4; i-- {
			addRel(m.Releases[i], "q", "lab", "published")
		}
	}
	left := func(b ovBar) float64 { f, _ := strconv.ParseFloat(b.Left, 64); return f }
	sort.SliceStable(t.Rows, func(i, j int) bool { return left(t.Rows[i]) < left(t.Rows[j]) })
	if len(t.Rows) > 6 {
		t.Rows = t.Rows[len(t.Rows)-6:]
	}
	return t
}

// lineupFullOn counts the role's deployed devices (at a venue, not in a lab session)
// that have all three: prod firmware where the model has one, the hosted client where
// one is offered, and the role's app where it has one.
func lineupFullOn(devices []db.Device, role string, models []pipeModel, views map[string]*ClientSlotView,
	base string, installed map[uuid.UUID]bool, inLab func(d *db.Device) bool) (full, deployed int) {
	byModel := map[string]pipeModel{}
	for _, m := range models {
		byModel[m.Key] = m
	}
	for i := range devices {
		d := &devices[i]
		if d.Hidden || d.Retired() || d.Class() != role || d.RestaurantID == nil || inLab(d) {
			continue
		}
		deployed++
		ok := true
		if m, has := byModel[modelKey(d)]; has && !d.IsDPC() && m.Mode == "prod" && m.Channel != nil && m.Channel.Version != "" {
			if firmwareLine(d.BuildID) != m.Line || firmwareBehind(d.BuildID, m.Channel.Version) {
				ok = false
			}
		}
		if v := views[agentSlotFor(d)]; ok && v != nil && v.Hosted {
			if _, code := clientVersionOf(d); code == 0 || (versionComparable(d, v.Build.Package) && code < v.Build.VersionCode) {
				ok = false
			}
		}
		if ok && base != "" && !installed[d.ID] {
			ok = false
		}
		if ok {
			full++
		}
	}
	return
}
