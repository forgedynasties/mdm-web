package dashboard

import (
	"encoding/json"
	"fmt"
	"html/template"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Access control AC1 with the powers matrix M2 (picked 21 Oct 2026). A banner of access
// risks (powers beyond a role, rules ending this week, setups the two questions can't
// show, a customer with no venue), Whole fleet / Part of the fleet / Extras cards, and the
// matrix as the main card: Person · Role · Sees · one column per power the server checks.
// The Role cell changes the role (POST /users/{id}/role); clicking a person opens today's
// two-question editor under their row (POST /users/{id}/access/simple, with
// /simple/preview for the "If you save" line). The rail: a whole-fleet ring, the access
// profiles and "Who can see a device?" across everyone (AccessWhoCanSee).

// ac1Col is a matrix column: the access action the server checks behind it.
type ac1Col struct{ Key, Short, Label string }

var ac1Cols = []ac1Col{
	{"view", "See", "See device"}, {"screenshot", "Shot", "Screenshot"}, {"reboot", "Reboot", "Reboot"},
	{"kiosk", "Lock", "Lock settings"}, {"install_apk", "Apps", "Install and uninstall apps"},
	{"remote", "Remote", "Remote control"}, {"ota", "OTA", "Firmware pushes"}, {"shell", "Shell", "Shell"},
}

// ac1Cell is one person × power: all (everywhere in the fleet), part (only part of it),
// off (the role allows it, but not here), above (the role can never have it), with
// Extra set when a rule gives a sensitive power the role does not hold by default.
type ac1Cell struct {
	Key, State string
	Extra      bool
	N          int // devices it reaches
	Title      string
}

type ac1Choice struct {
	Value string `json:"value"`
	Name  string `json:"name"`
	Desc  string `json:"desc"`
	Fits  bool   `json:"fits"`
}

type ac1Act struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Sensitive bool   `json:"sensitive"`
}

// ac1Edit is what the row editor starts from (JSON on the row).
type ac1Edit struct {
	OK      bool          `json:"ok"`
	Why     string        `json:"why,omitempty"`
	Every   bool          `json:"every"`
	Places  []scopeOption `json:"places"`
	Do      string        `json:"do"`
	Actions []string      `json:"actions"`
	Note    string        `json:"note"`
	Expires string        `json:"expires"` // datetime-local, "" = never
	Choices []ac1Choice   `json:"choices"`
	Custom  []ac1Act      `json:"custom"`
	Article string        `json:"article"`
}

type ac1Row struct {
	User      db.User
	Bubble    any
	Sees      string
	Devices   int
	Whole     bool
	Cells     []ac1Cell
	CanEdit   bool
	Sensitive bool
	Ending    bool // a rule ends within 7 days
	Odd       bool // a setup the two questions can't show
	NoVenue   bool // a customer with no venue
	Profile   string
	EditJSON  template.JS
}

type ac1Profile struct {
	Name, Desc string
	Users      int
}

type ac1Rail struct {
	Pct, Whole, Total, Limited int
	Offset                     string
	Profiles                   []ac1Profile
}

type ac1View struct {
	Banner dp1Banner
	Cards  []ovStat
	Rows   []ac1Row
	Rail   ac1Rail
	Cols   []ac1Col
	Roles  []string // the roles present, top down, for the Role pill
	Status string
}

// ac1Person is one account with its policy and scope evaluator, as the handler loads it.
type ac1Person struct {
	User    db.User
	Bubble  any
	Pol     db.AccessPolicy
	Acc     *access
	Simple  simpleAccess
	CanEdit bool
}

type ac1Inputs struct {
	People    []ac1Person
	Profiles  []db.AccessProfile
	FleetSize int
	DeviceIDs []uuid.UUID
	Now       time.Time
}

// ac1Cells asks the access evaluator, for each matrix column, how far a person's power
// reaches across the fleet.
func ac1Cells(p ac1Person, ids []uuid.UUID) []ac1Cell {
	role := p.User.Role
	ceiling := roleCeiling(role)
	out := make([]ac1Cell, 0, len(ac1Cols))
	for _, c := range ac1Cols {
		cell := ac1Cell{Key: c.Key}
		switch {
		case role == "admin":
			cell.State, cell.N = "all", len(ids)
		case ceiling == nil || !ceiling[c.Key]:
			cell.State = "above"
		default:
			for _, id := range ids {
				if p.Acc.canDevice(c.Key, id) {
					cell.N++
				}
			}
			switch {
			case cell.N == 0:
				cell.State = "off"
			case cell.N == len(ids):
				cell.State = "all"
			default:
				cell.State = "part"
			}
			cell.Extra = cell.N > 0 && accessActionByKey[c.Key].Sensitive && !sensitiveByDefault(role, c.Key)
		}
		switch cell.State {
		case "all":
			cell.Title = c.Label + ": every device"
		case "part":
			cell.Title = c.Label + ": " + dp1Plural(cell.N, "device", "devices")
		case "off":
			cell.Title = c.Label + ": off (" + roleArticle(role) + " can be given it)"
		default:
			cell.Title = c.Label + ": above " + roleArticle(role) + "; change the role first"
		}
		if cell.Extra {
			cell.Title += ", beyond the role"
		}
		out = append(out, cell)
	}
	return out
}

func buildAccessAC1(in ac1Inputs) ac1View {
	now := in.Now
	v := ac1View{Cols: ac1Cols}
	profileName := map[string]string{}
	for _, p := range in.Profiles {
		profileName["profile:"+p.ID.String()] = p.Name
	}
	whole, limited, byProfile, extras, ending := 0, 0, 0, 0, 0
	var odd, noVenue, extraNames, endNames []string
	for _, p := range in.People {
		u := p.User
		r := ac1Row{User: u, Bubble: p.Bubble, CanEdit: p.CanEdit, Cells: ac1Cells(p, in.DeviceIDs)}
		r.Devices = r.Cells[0].N
		r.Whole = u.Role == "admin" || r.Devices == in.FleetSize
		for _, c := range r.Cells {
			if c.Extra {
				r.Sensitive = true
			}
		}
		for _, g := range p.Pol.Grants {
			if g.ExpiresAt != nil && g.ExpiresAt.After(now) && g.ExpiresAt.Sub(now) <= 7*24*time.Hour {
				r.Ending = true
			}
		}
		sa := p.Simple
		switch {
		case u.Role == "admin" || r.Whole:
			r.Sees = "Every device"
		case !sa.OK:
			r.Sees = fmt.Sprintf("%d of %d devices", r.Devices, in.FleetSize)
		case len(sa.Places) == 0:
			r.Sees = "Nothing"
		default:
			names := []string{}
			for i, pl := range sa.Places {
				if i == 2 {
					names = append(names, fmt.Sprintf("%d more", len(sa.Places)-2))
					break
				}
				names = append(names, pl.Name)
			}
			r.Sees = joinAnd(names)
			if len(sa.Places) == 1 && sa.Places[0].Type == "device" {
				r.Sees = "1 device"
			}
		}
		if n := profileName[sa.Do]; sa.OK && n != "" {
			r.Profile = n
		}
		if u.Role != "admin" {
			e := ac1Edit{OK: sa.OK, Why: sa.Why, Every: sa.Every, Places: sa.Places, Do: sa.Do, Actions: sa.Actions, Note: sa.Note, Article: roleArticle(u.Role)}
			if e.Places == nil {
				e.Places = []scopeOption{}
			}
			if e.Actions == nil {
				e.Actions = []string{}
			}
			if sa.Expires != nil {
				e.Expires = sa.Expires.Local().Format("2006-01-02T15:04")
			}
			for _, p := range in.Profiles {
				e.Choices = append(e.Choices, ac1Choice{Value: "profile:" + p.ID.String(), Name: p.Name, Desc: p.Description, Fits: profileFits(p, u.Role)})
			}
			if !roleIsCustomer(u.Role) {
				e.Choices = append(e.Choices, ac1Choice{Value: "role", Name: "Everything " + roleArticle(u.Role) + " can", Desc: roleEverything(u.Role), Fits: true})
			}
			for _, a := range grantableActions(u.Role) {
				if a.Key != "view" {
					e.Custom = append(e.Custom, ac1Act{Key: a.Key, Label: a.Label, Sensitive: a.Sensitive})
				}
			}
			b, _ := json.Marshal(e)
			r.EditJSON = template.JS(b)
		}

		if r.Whole {
			whole++
		} else {
			limited++
			if r.Profile != "" {
				byProfile++
			}
		}
		if r.Sensitive {
			extras++
			extraNames = append(extraNames, u.DisplayName())
		}
		if r.Ending {
			ending++
			endNames = append(endNames, u.DisplayName())
		}
		if u.Role != "admin" && !sa.OK {
			r.Odd = true
			odd = append(odd, u.DisplayName())
		}
		if roleIsCustomer(u.Role) && r.Devices == 0 {
			r.NoVenue = true
			noVenue = append(noVenue, u.DisplayName())
		}
		v.Rows = append(v.Rows, r)
	}
	// Limited first (the rows an audit reads), then by role top down, then by name.
	sort.SliceStable(v.Rows, func(i, j int) bool {
		a, b := v.Rows[i], v.Rows[j]
		if a.Sensitive != b.Sensitive {
			return a.Sensitive
		}
		if a.Whole != b.Whole {
			return !a.Whole
		}
		if ra, rb := rank(a.User.Role), rank(b.User.Role); ra != rb {
			return ra > rb
		}
		return strings.ToLower(a.User.DisplayName()) < strings.ToLower(b.User.DisplayName())
	})
	total := len(in.People)
	present := map[string]bool{}
	for _, p := range in.People {
		present[p.User.Role] = true
	}
	for _, d := range roleDefs {
		if present[d.Key] {
			v.Roles = append(v.Roles, d.Key)
		}
	}

	// Banner.
	b := dp1Banner{Kicker: "Access · " + dp1Plural(total, "account", "accounts")}
	short := func(names []string) string {
		if len(names) > 3 {
			return strings.Join(names[:3], ", ") + fmt.Sprintf(" and %d more", len(names)-3)
		}
		return joinAnd(names)
	}
	var todos []dp1Todo
	if len(extraNames) > 0 {
		todos = append(todos, dp1Todo{Title: dp1Plural(len(extraNames), "person has", "people have") + " remote control or firmware beyond their role",
			Sub: short(extraNames) + " · check each rule is still needed", Action: "Show them", Href: "/users/access?show=extras#ac1-matrix"})
	}
	if len(endNames) > 0 {
		todos = append(todos, dp1Todo{Title: dp1Plural(len(endNames), "person's access ends", "people's access ends") + " this week",
			Sub: short(endNames) + " · extend it or let it lapse", Action: "Show them", Href: "/users/access?show=ending#ac1-matrix"})
	}
	if len(odd) > 0 {
		todos = append(todos, dp1Todo{Title: dp1Plural(len(odd), "setup", "setups") + " the two questions can't show",
			Sub: short(odd) + " · read them on the person's page", Action: "Show them", Href: "/users/access?show=odd#ac1-matrix"})
	}
	if len(noVenue) > 0 {
		todos = append(todos, dp1Todo{Title: dp1Plural(len(noVenue), "customer has", "customers have") + " no venue, so they see nothing",
			Sub: short(noVenue) + " · give them their venue", Action: "Show them", Href: "/users/access?show=novenue#ac1-matrix"})
	}
	if len(todos) > ovTodoRows {
		b.More = len(todos) - ovTodoRows
		todos = todos[:ovTodoRows]
	}
	b.Todos = todos
	if n := len(todos) + b.More; n > 0 {
		b.Headline = dp1Plural(n, "thing", "things") + " to look at"
	} else {
		b.Tone, b.Headline = "calm", "Nothing risky in the rules"
		line := "No rule gives remote control or firmware beyond a role, and none end this week."
		if limited > 0 {
			line += " " + dp1Plural(limited, "person sees", "people see") + " only part of the fleet."
		}
		b.Line = line
	}
	v.Banner = b

	v.Cards = []ovStat{
		{Title: "Whole fleet", Href: "/users/access?show=whole#ac1-matrix",
			A: ovMetric{Value: strconv.Itoa(whole), Label: "people", Tone: "flat"},
			B: ovMetric{Value: strconv.Itoa(in.FleetSize), Label: "devices each", Tone: "flat"}},
		{Title: "Part of the fleet", Href: "/users/access?show=part#ac1-matrix",
			A: ovMetric{Value: strconv.Itoa(limited), Label: "people", Tone: "flat"},
			B: ovMetric{Value: strconv.Itoa(byProfile), Label: "by profile", Tone: "flat"}},
		{Title: "Extras", Href: "/users/access?show=extras#ac1-matrix",
			A: ovMetric{Value: strconv.Itoa(extras), Label: "beyond the role", Tone: "good"},
			B: ovMetric{Value: strconv.Itoa(ending), Label: "ending this week", Tone: "flat"}},
	}
	if extras > 0 {
		v.Cards[2].A.Tone, v.Cards[2].A.Delta = "bad", "check them"
	}

	rl := ac1Rail{Whole: whole, Total: total, Limited: limited, Pct: dp1Pct(whole, total)}
	rl.Offset = fmt.Sprintf("%.1f", 326.7*float64(100-rl.Pct)/100)
	for _, p := range in.Profiles {
		labels := []string{}
		for _, k := range p.Actions {
			if a, ok := accessActionByKey[k]; ok {
				labels = append(labels, a.Label)
			}
		}
		desc := p.Description
		if desc == "" {
			desc = joinAnd(labels)
		}
		rl.Profiles = append(rl.Profiles, ac1Profile{Name: p.Name, Desc: desc, Users: len(p.Users)})
	}
	v.Rail = rl
	v.Status = fmt.Sprintf("%d see the whole fleet · %d see part of it · %d with extras beyond the role", whole, limited, extras)
	return v
}
