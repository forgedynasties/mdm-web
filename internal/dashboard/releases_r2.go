package dashboard

import (
	"fmt"
	"net/url"
	"strings"

	"mdm/internal/db"
)

// Releases R2 (picked 21 Oct 2026, from the Releases demos): the list on the left stays;
// the selected release's detail is a banner of what this release needs, three cards
// (On devices, the latest update, Crashes), then Packages, Changes and Updates as cards.
// Every action the pane had stays in its header.

// r2Todo is one banner row: a link, or a POST form back to this page.
type r2Todo struct {
	Title, Sub   string
	Action, Href string
	Post         string // form action when set; the button posts here and comes back
	Field, Value string // an extra hidden field for the form (dev=0)
	Confirm      string
}

type r2Banner struct {
	Tone     string // bad | warn | calm
	Kicker   string
	Headline string
	Todos    []r2Todo
	More     int
	Calm     string
}

// r2Seg is one slice of a card's bar.
type r2Seg struct {
	Flex int
	Tone string // p (installed) | f (failed) | g (in progress) | "" (rest)
}

type r2Card struct {
	Title, Href string
	A, B        ovMetric
	Bar         []r2Seg
	Empty       string
}

// r2Pkg is one package on the Packages card.
type r2Pkg struct {
	Full          bool
	From          string // incremental: the build it updates from
	Size, Date    string
	Wipe, Yanked  bool
	Status        string
}

type r2Pane struct {
	Banner   r2Banner
	Cards    []r2Card
	Packages []r2Pkg
}

// r2Inputs is what the pane is built from, shared by the handler and the tests.
type r2Inputs struct {
	Row          versionRow
	Rel          *db.Release
	ProductLabel string
	Packages     []db.OTAPackage
	Rollouts     []rolloutView // newest first
	FleetTotal   int
	InstallsWeek int
	Crashes      db.BuildCrashCounts
	ShowCrashes  bool // the viewer sees the whole fleet (the crash page is fleet-wide)
	CanRelease   bool
	CanOTA       bool
}

const r2TodoRows = 3

func buildReleaseR2(in r2Inputs) r2Pane {
	var p r2Pane
	p.Banner = buildR2Banner(in)
	p.Cards = buildR2Cards(in)
	for _, pk := range in.Packages {
		v := r2Pkg{Full: pk.Type != "incremental", From: pk.SourceBuildID, Wipe: pk.Wipe,
			Yanked: pk.Status != "active", Status: pk.Status, Date: pk.CreatedAt.Format("2 Jan")}
		if pk.PayloadSize > 0 {
			v.Size = r2Size(pk.PayloadSize)
		}
		p.Packages = append(p.Packages, v)
	}
	return p
}

func r2Size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", (n+1023)/1024)
}

func buildR2Banner(in r2Inputs) r2Banner {
	row, id := in.Row, 0
	if in.Rel != nil {
		id = in.Rel.ID
	}
	b := r2Banner{Tone: "calm"}
	kick := []string{row.Version}
	if in.ProductLabel != "" {
		kick = append(kick, in.ProductLabel)
	}
	status := row.Status
	if row.ReleasedAt != nil && status == "published" {
		status = "published " + row.ReleasedAt.Format("2 Jan")
	}
	if status != "" {
		kick = append(kick, status)
	}
	if row.IsDev {
		kick = append(kick, "dev")
	}
	b.Kicker = strings.Join(kick, " · ")

	var todos []r2Todo
	bad := false
	// A device that failed in an update still running, newest first.
	for _, r := range in.Rollouts {
		if r.Status == "active" && r.Failed > 0 {
			bad = true
			todos = append(todos, r2Todo{
				Title:  fmt.Sprintf("%d device%s failed in update #%d", r.Failed, sIfN(r.Failed), r.ID),
				Sub:    fmt.Sprintf("%d of %d installed, %d still going", r.Installed, r.Total, r.Installing+r.Pending()),
				Action: "Open update", Href: r.URL})
		}
	}
	if row.Problems.Blockers > 0 {
		bad = true
		todos = append(todos, r2Todo{
			Title:  fmt.Sprintf("%d blocking problem%s", row.Problems.Blockers, sIfN(row.Problems.Blockers)),
			Sub:    fmt.Sprintf("%d open in all", row.Problems.Open),
			Action: "See problems", Href: fmt.Sprintf("/releases/%d?tab=problems", id)})
	}
	if in.ShowCrashes && in.Crashes.Day >= 5 && in.Crashes.Day > in.Crashes.PrevDay {
		todos = append(todos, r2Todo{
			Title:  fmt.Sprintf("%d crashes on this build in 24 h", in.Crashes.Day),
			Sub:    fmt.Sprintf("on %d device%s · %d the day before", in.Crashes.DayDevices, sIfN(in.Crashes.DayDevices), in.Crashes.PrevDay),
			Action: "See crashes", Href: fmt.Sprintf("/releases/%d/crashes", id)})
	}
	hasFull := false
	for _, pk := range in.Packages {
		if pk.Status == "active" && pk.Type == "full" {
			hasFull = true
		}
	}
	if in.CanRelease && row.Status != "published" {
		if hasFull {
			todos = append(todos, r2Todo{Title: "A draft with its full package in",
				Sub: "publish it to offer it to devices", Action: "Publish",
				Post: fmt.Sprintf("/releases/%d/publish", id)})
		} else {
			todos = append(todos, r2Todo{Title: "A draft without a full package",
				Sub: "devices can only be sent it once a full package is attached", Action: "Add package",
				Href: fmt.Sprintf("/releases/%d?tab=packages", id)})
		}
	}
	if in.CanRelease && row.IsDev && row.Status == "published" {
		sub := "only super admins see it in push pickers"
		if row.DeviceCount > 0 {
			sub += fmt.Sprintf(" · %d device%s have it", row.DeviceCount, sIfN(row.DeviceCount))
		}
		todos = append(todos, r2Todo{Title: "Still a dev release", Sub: sub, Action: "Release to everyone",
			Post: fmt.Sprintf("/releases/%d/dev", id), Field: "dev", Value: "0",
			Confirm: fmt.Sprintf("Release %s to everyone? It stops being folded away behind the dev toggle.", row.Version)})
	}
	if row.SignedOffBy == "" && !row.TestingDone && row.QA.Total > 0 && !row.QA.Passed() {
		todos = append(todos, r2Todo{Title: "Not signed off yet",
			Sub:    fmt.Sprintf("QA: %d of %d cases passed", row.QA.Pass, row.QA.Total),
			Action: "Open QA", Href: fmt.Sprintf("/releases/%d/qa", id)})
	}

	if len(todos) == 0 {
		b.Headline = "Nothing waiting on this release"
		bits := []string{fmt.Sprintf("on %d device%s", row.DeviceCount, sIfN(row.DeviceCount))}
		if n := len(in.Rollouts); n > 0 {
			bits = append(bits, fmt.Sprintf("%d update%s sent", n, sIfN(n)))
		}
		if row.SignedOffBy != "" {
			bits = append(bits, "signed off by "+row.SignedOffBy)
		}
		b.Calm = strings.ToUpper(bits[0][:1]) + bits[0][1:]
		if len(bits) > 1 {
			b.Calm += " · " + strings.Join(bits[1:], " · ")
		}
		b.Calm += "."
		return b
	}
	b.Tone = "warn"
	if bad {
		b.Tone = "bad"
	}
	thing := "things"
	if len(todos) == 1 {
		thing = "thing"
	}
	b.Headline = fmt.Sprintf("%d %s for this release", len(todos), thing)
	if len(todos) > r2TodoRows {
		b.More = len(todos) - r2TodoRows
		todos = todos[:r2TodoRows]
	}
	b.Todos = todos
	return b
}

func buildR2Cards(in r2Inputs) []r2Card {
	row, id := in.Row, 0
	if in.Rel != nil {
		id = in.Rel.ID
	}

	// On devices: devices on this build, installs finished this week, devices getting it.
	inFlight := 0
	for _, r := range in.Rollouts {
		if r.Status == "active" {
			inFlight += r.Installing + r.Pending()
		}
	}
	on := r2Card{Title: "On devices", Href: "/devices?build=" + url.QueryEscape(row.Version)}
	on.A = ovMetric{Value: fmt.Sprintf("%d", row.DeviceCount), Label: fmt.Sprintf("of %d devices", in.FleetTotal), Tone: "flat"}
	if in.InstallsWeek > 0 {
		on.A.Delta, on.A.Tone = fmt.Sprintf("▲ %d this week", in.InstallsWeek), "good"
	}
	on.B = ovMetric{Value: fmt.Sprintf("%d", inFlight), Label: "getting it now", Tone: "flat"}
	rest := in.FleetTotal - row.DeviceCount - inFlight
	if rest < 0 {
		rest = 0
	}
	on.Bar = []r2Seg{{row.DeviceCount, "p"}, {inFlight, "g"}, {rest, ""}}

	// The latest update (the running one first).
	up := r2Card{Title: "Updates"}
	var latest *rolloutView
	for i := range in.Rollouts {
		if in.Rollouts[i].Status == "active" {
			latest = &in.Rollouts[i]
			break
		}
	}
	if latest == nil && len(in.Rollouts) > 0 {
		latest = &in.Rollouts[0]
	}
	if latest == nil {
		up.Empty = "No update sent yet."
		if in.CanOTA && row.Status == "published" {
			up.Empty += " Update devices sends one."
		}
	} else {
		up.Title, up.Href = fmt.Sprintf("Update #%d", latest.ID), latest.URL
		state := "installed"
		if latest.Status == "complete" {
			state = "installed · done"
		}
		up.A = ovMetric{Value: fmt.Sprintf("%d/%d", latest.Installed, latest.Total), Label: state, Tone: "flat"}
		up.B = ovMetric{Value: fmt.Sprintf("%d", latest.Failed), Label: "failed", Tone: "flat"}
		if latest.Failed > 0 {
			up.B.Tone = "bad"
		}
		up.Bar = []r2Seg{{latest.Installed, "p"}, {latest.Failed, "f"}, {latest.Installing, "g"}, {latest.Pending(), ""}}
	}

	// Crashes on this build (the crash page is fleet-wide, and so are these numbers).
	cr := r2Card{Title: "Crashes", Href: fmt.Sprintf("/releases/%d/crashes", id)}
	if !in.ShowCrashes {
		cr.Href = ""
		cr.Empty = "Crash numbers are for people who see the whole fleet."
	} else {
		c := in.Crashes
		cr.A = ovMetric{Value: fmt.Sprintf("%d", c.Day), Label: fmt.Sprintf("24 h · %d device%s", c.DayDevices, sIfN(c.DayDevices)), Tone: "flat"}
		cr.A.Delta, cr.A.Tone = ovDelta(float64(c.Day-c.PrevDay), "", true, 0)
		cr.B = ovMetric{Value: fmt.Sprintf("%d", c.Week), Label: fmt.Sprintf("7 days · %d device%s", c.WeekDevices, sIfN(c.WeekDevices)), Tone: "flat"}
		if row.Crash != nil {
			cr.B.Delta = fmt.Sprintf("%d issue%s", row.Crash.Issues, sIfN(row.Crash.Issues))
			cr.B.Tone = "bad"
			if row.Crash.Warn {
				cr.B.Tone = "flat"
			}
		}
		other := row.DeviceCount - c.WeekDevices
		if other < 0 {
			other = 0
		}
		cr.Bar = []r2Seg{{c.WeekDevices, "f"}, {other, ""}}
	}
	return []r2Card{on, up, cr}
}
