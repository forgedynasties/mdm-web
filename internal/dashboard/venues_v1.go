package dashboard

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"mdm/internal/db"
	"mdm/internal/product"
)

// Venues list V1 (picked 21 Oct 2026, from the venues demos): the Overview's venue cards
// as a grid, worst first, with filter pills (health, open now, device type, search) and
// a rail — average health, venues open by hour (from each venue's service hours) and
// what needs a look. "Compact" (?view=compact) keeps the one-row-per-venue table.

// vcExtra is what the cards need beyond the shared venue row.
type vcExtra struct {
	Window   *db.ServiceWindow // effective service hours: the venue's own, else the fleet default
	TZ       string            // the venue's timezone, for a window without one
	DefaultPIN bool            // staff PIN never changed from db.DefaultStaffPIN
	HotNow   int               // devices online now at or over 45 °C
	Classes  map[string]int    // device type → devices
}

type vcCard struct {
	venueRow
	Open, HasHours bool
	DefaultPIN     bool
	HotNow         int
	Classes        string // space-separated class keys, for the Type pill
	Rank           int
}

type vcHour struct {
	Label string
	N     int
	Pct   int
	Cur   bool
}

type vcLook struct {
	Tone, Name, Sub, Href string
}

type vcType struct {
	Key, Label string
	Venues     int
}

type vcRail struct {
	Avg       int
	HasAvg    bool
	Offset    string // ring stroke-dashoffset (r52, 326.7)
	Tone      string // ok | warn | danger
	OpenNow   int
	WithHours int
	Venues    int
	NeedLook  int
	Hours     []vcHour
	Looks     []vcLook
	MoreLooks int
}

type vcPage struct {
	Cards   []vcCard
	Rail    vcRail
	Types   []vcType
	Look    int // venues that need a look
	Healthy int
	Empty   int
	Open    int
}

const vcLookRows = 6

var vcRank = map[string]int{"danger": 0, "warn": 1, "ok": 2, "empty": 3}

// buildVenuesV1 orders the cards worst first (at risk, watch, healthy, empty; lowest
// score first inside a band) and builds the rail.
func buildVenuesV1(rows []venueRow, ex map[string]vcExtra, now time.Time) vcPage {
	var p vcPage
	typeVenues := map[string]int{}
	scoreSum, scored := 0, 0
	for _, r := range rows {
		c := vcCard{venueRow: r, Rank: vcRank[r.Class]}
		e := ex[r.ID]
		if e.Window != nil {
			c.HasHours = true
			c.Open = db.ServiceOpenAt(*e.Window, e.TZ, now)
		}
		c.DefaultPIN = e.DefaultPIN && r.Devices > 0
		c.HotNow = e.HotNow
		var keys []string
		for k := range e.Classes {
			keys = append(keys, k)
			typeVenues[k]++
		}
		sort.Strings(keys)
		c.Classes = strings.Join(keys, " ")
		if r.Devices > 0 && r.HasH {
			scoreSum += r.H.Score
			scored++
		}
		switch {
		case r.Devices == 0:
			p.Empty++
		case r.NeedsLook:
			p.Look++
		default:
			p.Healthy++
		}
		if c.Open {
			p.Open++
		}
		p.Cards = append(p.Cards, c)
	}
	sort.SliceStable(p.Cards, func(i, j int) bool {
		a, b := p.Cards[i], p.Cards[j]
		if a.Rank != b.Rank {
			return a.Rank < b.Rank
		}
		if a.HasH && b.HasH && a.H.Score != b.H.Score {
			return a.H.Score < b.H.Score
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})

	for _, c := range product.Classes() {
		if n := typeVenues[c]; n > 0 {
			p.Types = append(p.Types, vcType{c, product.ClassLabel(c), n})
		}
	}
	sort.SliceStable(p.Types, func(i, j int) bool { return p.Types[i].Venues > p.Types[j].Venues })

	r := &p.Rail
	r.Venues, r.OpenNow, r.NeedLook = len(rows), p.Open, p.Look
	if scored > 0 {
		r.Avg, r.HasAvg = int(math.Round(float64(scoreSum)/float64(scored))), true
	}
	r.Offset = fmt.Sprintf("%.1f", 326.7*float64(100-r.Avg)/100)
	switch {
	case r.Avg >= 80:
		r.Tone = "ok"
	case r.Avg >= 50:
		r.Tone = "warn"
	default:
		r.Tone = "danger"
	}
	// Venues open from five hours ago to six hours ahead, each in its own timezone.
	peak := 0
	for off := -5; off <= 6; off++ {
		t := now.Add(time.Duration(off) * time.Hour)
		n := 0
		for _, c := range p.Cards {
			if e := ex[c.ID]; e.Window != nil && db.ServiceOpenAt(*e.Window, e.TZ, t) {
				n++
			}
		}
		lbl := fmt.Sprintf("%+d", off)
		if off == 0 {
			lbl = "now"
		}
		r.Hours = append(r.Hours, vcHour{Label: lbl, N: n, Cur: off == 0})
		if n > peak {
			peak = n
		}
	}
	for i := range r.Hours {
		r.Hours[i].Pct = 4
		if peak > 0 && r.Hours[i].N > 0 {
			if r.Hours[i].Pct = r.Hours[i].N * 100 / peak; r.Hours[i].Pct < 8 {
				r.Hours[i].Pct = 8
			}
		}
	}
	for _, c := range p.Cards {
		if c.HasHours {
			r.WithHours++
		}
	}

	// Needs a look: venues at risk, then watch, then a still-default staff PIN, then
	// venues with no devices.
	var looks []vcLook
	for _, c := range p.Cards {
		if !c.NeedsLook {
			continue
		}
		var bits []string
		if c.Offline > 0 {
			bits = append(bits, fmt.Sprintf("%d offline", c.Offline))
		}
		if c.HasH && c.H.OpenCritical > 0 {
			bits = append(bits, fmt.Sprintf("%d critical", c.H.OpenCritical))
		}
		if c.HotNow > 0 {
			bits = append(bits, "a unit running hot")
		}
		if c.Open {
			bits = append(bits, "open now")
		}
		if len(bits) == 0 && c.HasH {
			bits = append(bits, fmt.Sprintf("health %d", c.H.Score))
		}
		tone := "warn"
		if c.Class == "danger" {
			tone = "danger"
		}
		looks = append(looks, vcLook{tone, c.Name, strings.Join(bits, " · "), c.Href})
	}
	for _, c := range p.Cards {
		if c.DefaultPIN {
			looks = append(looks, vcLook{"", c.Name, "staff PIN is still the default " + db.DefaultStaffPIN, c.Href + "?open=lock"})
		}
	}
	for _, c := range p.Cards {
		if c.Devices == 0 {
			looks = append(looks, vcLook{"", c.Name, "no devices yet", c.Href})
		}
	}
	if len(looks) > vcLookRows {
		r.MoreLooks = len(looks) - vcLookRows
		looks = looks[:vcLookRows]
	}
	r.Looks = looks
	return p
}

// vcHotNow: a device at or over 45 °C that is online now. An offline device's
// temperature is its last reading, however old (user, 21 Oct).
func vcHotNow(online bool, temp float64, ok bool) bool { return online && ok && temp >= 45 }
