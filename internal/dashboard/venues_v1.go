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

// Venues list (V1 "Fleet's twin", picked 30 Oct 2026): Fleet's layout, a rail of saved
// views (need a look, offline, open now, empty, by state), product tabs, search + sort,
// quick chips and one table row per venue. The earlier card grid (21 Oct) stays as the
// per-person Cards view (cookie mdm_venues_view); its rail is built here too.

// vcExtra is what the cards need beyond the shared venue row.
type vcExtra struct {
	Window   *db.ServiceWindow // effective service hours: the venue's own, else the fleet default
	TZ       string            // the venue's timezone, for a window without one
	DefaultPIN bool            // staff PIN never changed from db.DefaultStaffPIN
	HotNow   int               // devices online now at or over 45 °C
	Classes  map[string]int    // device type → devices
	Place    string            // "Benicia, CA" from the address, else ""
	State    string            // two-letter state code from the address, else ""
	TZ2      string            // the timezone's city, "Los Angeles"
}

type vcCard struct {
	venueRow
	Open, HasHours bool
	DefaultPIN     bool
	HotNow         int
	Classes        string // space-separated class keys, for the Type pill
	Rank           int
	Place, State   string
	TZ2            string
	Products       []vcProd // device types with counts, in product order
	OnPct          int      // online share, for the bar
	Flags          string   // space-separated filter keys for the table's rail and chips
}

type vcProd struct {
	Key, Label string
	N          int
}

type vcState struct {
	Code, Name string
	Venues     int
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
	Offline int // venues with an offline device
	Hot     int // venues with a device running hot
	PIN     int // venues on the default staff PIN
	States  []vcState
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
		c.Place, c.State, c.TZ2 = e.Place, e.State, e.TZ2
		for _, k := range product.Classes() {
			if n := e.Classes[k]; n > 0 {
				c.Products = append(c.Products, vcProd{k, product.ClassLabel(k), n})
			}
		}
		if r.Devices > 0 {
			c.OnPct = r.Online * 100 / r.Devices
		}
		var flags []string
		if r.Devices > 0 && r.NeedsLook {
			flags = append(flags, "look")
		}
		if r.Offline > 0 {
			flags = append(flags, "offline")
			p.Offline++
		}
		if c.Open {
			flags = append(flags, "open")
		}
		if c.HotNow > 0 {
			flags = append(flags, "hot")
			p.Hot++
		}
		if c.DefaultPIN {
			flags = append(flags, "pin")
			p.PIN++
		}
		if r.Devices == 0 {
			flags = append(flags, "empty")
		}
		if c.State != "" {
			flags = append(flags, "st-"+c.State)
		}
		c.Flags = strings.Join(flags, " ")
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
	states := map[string]int{}
	for _, c := range p.Cards {
		if c.State != "" {
			states[c.State]++
		}
	}
	for code, n := range states {
		p.States = append(p.States, vcState{code, usStateName(code), n})
	}
	sort.Slice(p.States, func(i, j int) bool {
		if p.States[i].Venues != p.States[j].Venues {
			return p.States[i].Venues > p.States[j].Venues
		}
		return p.States[i].Name < p.States[j].Name
	})

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

// venuePlace reads "City, ST" and the state code out of a free-text US address:
// "1321 Blossom Hill Rd, San Jose, CA 95118" → "San Jose, CA", "CA". When the city shares
// a part with the street ("609 1st St Brentwood, CA 94513") the street's last word is the
// city. Anything it can't read gives "", "".
func venuePlace(addr string) (string, string) {
	parts := strings.Split(addr, ",")
	for i := 1; i < len(parts); i++ {
		f := strings.Fields(parts[i])
		if len(f) == 0 || len(f[0]) != 2 || strings.ToUpper(f[0]) != f[0] || usStateName(f[0]) == f[0] {
			continue
		}
		city := strings.TrimSpace(parts[i-1])
		if city != "" && city[0] >= '0' && city[0] <= '9' {
			w := strings.Fields(city)
			city = w[len(w)-1]
		}
		if city == "" {
			return "", f[0]
		}
		return city + ", " + f[0], f[0]
	}
	return "", ""
}

// tzCity: "America/Los_Angeles" → "Los Angeles".
func tzCity(tz string) string {
	if i := strings.LastIndex(tz, "/"); i >= 0 {
		tz = tz[i+1:]
	}
	return strings.ReplaceAll(tz, "_", " ")
}

var usStates = map[string]string{"AL": "Alabama", "AK": "Alaska", "AZ": "Arizona", "AR": "Arkansas", "CA": "California",
	"CO": "Colorado", "CT": "Connecticut", "DE": "Delaware", "DC": "District of Columbia", "FL": "Florida", "GA": "Georgia",
	"HI": "Hawaii", "ID": "Idaho", "IL": "Illinois", "IN": "Indiana", "IA": "Iowa", "KS": "Kansas", "KY": "Kentucky",
	"LA": "Louisiana", "ME": "Maine", "MD": "Maryland", "MA": "Massachusetts", "MI": "Michigan", "MN": "Minnesota",
	"MS": "Mississippi", "MO": "Missouri", "MT": "Montana", "NE": "Nebraska", "NV": "Nevada", "NH": "New Hampshire",
	"NJ": "New Jersey", "NM": "New Mexico", "NY": "New York", "NC": "North Carolina", "ND": "North Dakota", "OH": "Ohio",
	"OK": "Oklahoma", "OR": "Oregon", "PA": "Pennsylvania", "RI": "Rhode Island", "SC": "South Carolina",
	"SD": "South Dakota", "TN": "Tennessee", "TX": "Texas", "UT": "Utah", "VT": "Vermont", "VA": "Virginia",
	"WA": "Washington", "WV": "West Virginia", "WI": "Wisconsin", "WY": "Wyoming"}

// usStateName is the state's name, or the code itself when it isn't one.
func usStateName(code string) string {
	if n, ok := usStates[code]; ok {
		return n
	}
	return code
}
