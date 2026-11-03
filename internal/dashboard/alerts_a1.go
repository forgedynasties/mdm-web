package dashboard

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
)

// Alerts L1 "Fleet's twin" (picked 3 Nov 2026; user: "make alerts page also like our fleet page
// list. we can remove the handle, make the list lean and no card and make table"). Replaces A1
// (21 Oct: to-do banner, count cards, rail with ring and crash bars). Fleet's layout: a rail of
// views (Needs action, Everything open, Acknowledged, Snoozed, Crashes), By problem and By venue
// with counts; search, sort, quick chips; one table row per problem (alerts open on one device
// worked as one, alert_problems.go) with every action it had: Acknowledge with a note, and
// Assign / Snooze / Resolve with a reason / Lock again under ⋯. Every row is on the page and
// coll_list.js filters in place; the crash feed and release issues are the Crashes view
// (?view=crashes), also tables.

// alL1 is the list's data: the rows and the rail's and chips' counts.
type alL1 struct {
	Rows                         []alL1Row
	Needs, Open, Acked, Snoozed  int
	Kinds, Venues                []alCount
	NoVenue                      int
	Crit, Warn, AtVenue, Mine    int
	Unassigned, NewSince, Closed int
}

// alL1Row is one problem with what the list filters and sorts it by.
type alL1Row struct {
	P     problemView
	Flags string // data-flags: needs open acked snoozed crit warn atvenue v-<id> nov k-<kind> mine unassigned newsince closed
	Text  string // data-text, what search matches
	Since int64  // data-s-since (unix)
	Sev   int    // data-s-sev: 2 critical, 1 warning, 0 info
}

// alCount is a rail entry: a flag key, its label and how many rows carry it.
type alCount struct {
	Key, Label string
	N          int
}

// alKind is a problem's kind for "By problem": its cause's title, else its own title.
func alKind(p problemView) (key, label string) {
	label = p.Cause.Title
	if label == "" {
		label = p.Title
	}
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(label)))
	return fmt.Sprintf("k-%08x", h.Sum32()), label
}

// buildAlertsL1 lays out the open problems (needs, watching and snoozed, as the handler
// splits them) for the list; me is the viewer's username for "Mine".
func buildAlertsL1(me string, lists ...[]problemView) alL1 {
	var out alL1
	kinds, venues := map[string]*alCount{}, map[string]*alCount{}
	for _, list := range lists {
		for _, p := range list {
			var f []string
			switch {
			case p.Snoozed:
				f = append(f, "snoozed")
				out.Snoozed++
			case p.Acked && !p.NewSymptom:
				f = append(f, "open", "acked")
				out.Open++
				out.Acked++
			default:
				f = append(f, "open", "needs")
				out.Open++
				out.Needs++
			}
			sev := 0
			switch p.Severity {
			case "critical":
				f, sev = append(f, "crit"), 2
				out.Crit++
			case "warning":
				f, sev = append(f, "warn"), 1
				out.Warn++
			}
			if p.RestID != "" {
				f = append(f, "atvenue", "v-"+p.RestID)
				out.AtVenue++
				if venues[p.RestID] == nil {
					venues[p.RestID] = &alCount{Key: "v-" + p.RestID, Label: p.Restaurant}
				}
				venues[p.RestID].N++
			} else {
				f = append(f, "nov")
				out.NoVenue++
			}
			k, label := alKind(p)
			f = append(f, k)
			if kinds[k] == nil {
				kinds[k] = &alCount{Key: k, Label: label}
			}
			kinds[k].N++
			switch {
			case p.Assignee == "":
				f = append(f, "unassigned")
				out.Unassigned++
			case me != "" && p.Assignee == me:
				f = append(f, "mine")
				out.Mine++
			}
			if p.NewSymptom {
				f = append(f, "newsince")
				out.NewSince++
			}
			if p.AfterHours {
				f = append(f, "closed")
				out.Closed++
			}
			out.Rows = append(out.Rows, alL1Row{P: p, Flags: strings.Join(f, " "), Sev: sev, Since: p.Since.Unix(),
				Text: strings.Join([]string{p.Title, p.Why, p.Serial, p.Restaurant, p.Assignee, label, p.Search}, " ")})
		}
	}
	// Newest first, as the list opens.
	sort.SliceStable(out.Rows, func(i, j int) bool { return out.Rows[i].Since > out.Rows[j].Since })
	byN := func(m map[string]*alCount) []alCount {
		var s []alCount
		for _, c := range m {
			s = append(s, *c)
		}
		sort.Slice(s, func(i, j int) bool {
			if s[i].N != s[j].N {
				return s[i].N > s[j].N
			}
			return strings.ToLower(s[i].Label) < strings.ToLower(s[j].Label)
		})
		return s
	}
	out.Kinds, out.Venues = byN(kinds), byN(venues)
	return out
}

// alAnchor is a row's HTML id: problem keys can hold a restaurant name.
func alAnchor(key string) string {
	h := fnv.New32a()
	h.Write([]byte(key))
	return fmt.Sprintf("p-%08x", h.Sum32())
}

// alAnchors sets each problem's Anchor.
func alAnchors(ps []problemView) {
	for i := range ps {
		ps[i].Anchor = alAnchor(ps[i].Key)
	}
}
