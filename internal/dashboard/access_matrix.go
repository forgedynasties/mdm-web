package dashboard

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"mdm/internal/db"
)

// The Access page's "Who can do what" matrix and its risks (AC1, picked 18 Nov 2026).
// Rows are roles, then the people who hold grants of their own. The first three columns
// are the access levels (View, Operate, Manage) read from the grants; the last three are
// kept for super admins and no level grants them. Everything is derived from the grants
// the page already loads; nothing new is stored.

var axColumns = []struct{ Name, Sub string }{
	{"View", "look · screenshot · logs"}, {"Operate", "reboot · lock · apps"}, {"Manage", "remote · Wi-Fi · move"},
	{"Firmware", "super admin only"}, {"Shell", "super admin only"}, {"People", "super admin only"},
}

type axCell struct {
	Class string // f everything · p part · e extra grant · n none
	Text  string
	Lines []string // the cell's explanation
	Tip   string   // Lines joined, for the page
	Fixed bool     // a grant can't change it (super admin, customer, or a super-admin-only power)
}

type axRow struct {
	Key, Name, Sub, Role, ID string
	Person, Divider          bool
	Cells                    []axCell
}

type axItem struct {
	Name, Sub, ID, GrantID string
}

type axRisk struct {
	Key, Tone, Title, Desc string
	Items                  []axItem
}

// axShort names a grant's scope in a few words, for a matrix cell.
func axShort(g db.AccessLevel) string {
	switch g.ScopeType {
	case "all":
		return "All devices"
	case "state":
		switch g.State {
		case db.StateLab:
			return "Lab"
		case db.StateToPlace:
			return "To place"
		case db.StateSettingUp:
			return "Setting up"
		case db.StateInService:
			return "In service"
		}
	case "group", "venue":
		if g.ScopeName != "" {
			return g.ScopeName
		}
	case "visit":
		return "Visit venues"
	case "own":
		return "Own enrolments"
	}
	return scopeText(g)
}

func axCells(gs []db.AccessLevel, now time.Time, extra bool) []axCell {
	out := make([]axCell, 0, 6)
	for c := 0; c < 3; c++ {
		var short, lines []string
		all := false
		seen := map[string]bool{}
		for _, g := range gs {
			if g.Expired(now) || g.Rank() < c+1 {
				continue
			}
			if g.ScopeType == "all" {
				all = true
			}
			if s := axShort(g); !seen[s] {
				seen[s] = true
				short = append(short, s)
			}
			l := levelWord(g.Rank()) + " on " + scopeText(g)
			if g.Until != nil {
				l += " until " + g.Until.Local().Format("2 Jan 2006")
			}
			lines = append(lines, l)
		}
		cell := axCell{Class: "n", Text: "—", Lines: []string{"Nothing at this level."}}
		switch {
		case extra && len(short) == 0:
			cell.Text = "as role"
			cell.Lines = []string{"Nothing added beyond the role."}
		case len(short) > 0:
			cell.Text, cell.Lines = strings.Join(short, ", "), lines
			cell.Class = "p"
			if all {
				cell.Class, cell.Text = "f", "All devices"
			}
			if extra {
				cell.Class = "e"
			}
		}
		out = append(out, cell)
	}
	for c := 3; c < 6; c++ {
		out = append(out, axCell{Class: "n", Text: "—", Fixed: true, Lines: []string{"Only super admins hold this. No level grants it."}})
	}
	return out
}

// buildAccessMatrix makes the matrix rows and the risk lists.
func buildAccessMatrix(users []db.User, all []db.AccessLevel, now time.Time) ([]axRow, []axRisk) {
	count := map[string]int{}
	byName := map[string]db.User{}
	for _, u := range users {
		count[u.Role]++
		byName[u.Username] = u
	}
	byRole := map[string][]db.AccessLevel{}
	byUser := map[string][]db.AccessLevel{}
	for _, g := range all {
		if g.SubjectType == "role" {
			byRole[g.Subject] = append(byRole[g.Subject], g)
		} else {
			byUser[g.Subject] = append(byUser[g.Subject], g)
		}
	}
	fixed := func(ts ...string) []axCell {
		cs := make([]axCell, 6)
		for i := range cs {
			cs[i] = axCell{Class: "n", Text: "—", Fixed: true, Lines: []string{"Not part of the role."}}
			if i < len(ts) && ts[i] != "" {
				cs[i] = axCell{Class: "p", Text: ts[i], Fixed: true, Lines: []string{"Customers see only their own venues; set on the venue's People row."}}
			}
		}
		return cs
	}
	var rows []axRow
	for _, d := range roleDefs {
		if d.Legacy && count[d.Key] == 0 {
			continue
		}
		r := axRow{Key: d.Key, Name: d.Label, Role: d.Key, Sub: peopleN(count[d.Key])}
		switch {
		case d.Key == "admin":
			r.Cells = make([]axCell, 6)
			for i := range r.Cells {
				r.Cells[i] = axCell{Class: "f", Text: "Yes", Fixed: true, Lines: []string{"Super admins hold everything."}}
			}
		case d.Key == "owner":
			r.Cells = fixed("Own venues", "Own venues", "", "", "", "Own staff")
		case d.Customer:
			r.Cells = fixed("Own venues")
		default:
			r.Cells = axCells(byRole[d.Key], now, false)
		}
		rows = append(rows, r)
	}
	var names []string
	for n := range byUser {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		u, ok := byName[n]
		if !ok || u.Role == "admin" {
			continue
		}
		active := false
		for _, g := range byUser[n] {
			if !g.Expired(now) {
				active = true
			}
		}
		if !active {
			continue
		}
		rows = append(rows, axRow{Key: "p-" + n, Name: u.DisplayName(), Sub: roleByKey[u.Role].Label + " + grants", Role: u.Role,
			ID: u.ID.String(), Person: true, Cells: axCells(byUser[n], now, true)})
	}

	// Risks.
	nameOf := func(un string) string {
		if u, ok := byName[un]; ok {
			return u.DisplayName()
		}
		return un
	}
	demo := axRisk{Key: "demo", Tone: "r", Title: "Demo accounts that can act on the fleet", Desc: "They hold real access. Delete them from their page when the demo is over."}
	adm := axRisk{Key: "admin", Tone: "y", Title: "Super admins", Desc: "Can push firmware, open a shell and manage people."}
	ext := axRisk{Key: "extra", Tone: "o", Title: "Access beyond the role", Desc: "Given to one person on top of their role."}
	exp := axRisk{Key: "ending", Tone: "y", Title: "Grants ending within 7 days", Desc: "Extend them or let them lapse."}
	for _, u := range users {
		it := axItem{Name: u.DisplayName(), Sub: roleByKey[u.Role].Label, ID: u.ID.String()}
		if u.Role == "admin" {
			adm.Items = append(adm.Items, it)
		}
		em := ""
		if u.Email != nil {
			em = *u.Email
		}
		if (strings.HasPrefix(strings.ToLower(u.Username), "demo.") || strings.HasPrefix(strings.ToLower(em), "demo.")) && !roleByKey[u.Role].Customer {
			demo.Items = append(demo.Items, it)
		}
	}
	for _, g := range all {
		if g.Expired(now) {
			continue
		}
		who := rolePlural(g.Subject)
		if g.SubjectType != "role" {
			who = nameOf(g.Subject)
		}
		it := axItem{Name: who, Sub: levelWord(g.Rank()) + " on " + scopeText(g), GrantID: g.ID.String()}
		if g.SubjectType != "role" {
			if u, ok := byName[g.Subject]; ok {
				it.ID = u.ID.String()
			}
			ext.Items = append(ext.Items, it)
		}
		if g.Until != nil && g.Until.Before(now.Add(7*24*time.Hour)) {
			it.Sub += " · until " + g.Until.Local().Format("2 Jan")
			exp.Items = append(exp.Items, it)
		}
	}
	firstP := true
	for i := range rows {
		for j := range rows[i].Cells {
			rows[i].Cells[j].Tip = strings.Join(rows[i].Cells[j].Lines, "|")
		}
		if rows[i].Person && firstP {
			rows[i].Divider, firstP = true, false
		}
	}
	return rows, []axRisk{demo, adm, ext, exp}
}

func peopleN(n int) string {
	if n == 1 {
		return "1 person"
	}
	return strconv.Itoa(n) + " people"
}
