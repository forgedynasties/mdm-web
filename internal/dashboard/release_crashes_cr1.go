package dashboard

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mdm/internal/db"
)

// CR1 · Release crashes: a list of crash groups (kind, signature, count, devices) on the
// left and the picked group explained on the right (trace, per-day trend, devices with
// the time, delete group / delete one event). Filters: kind and 7 d / 30 d.

const crGroupPage = 25   // groups per page, as before
const crEventLimit = 100 // newest occurrences listed for the picked group
const crTrendDays = 14   // days in the picked group's trend

type crKindChip struct {
	Kind, Label, URL string
	Count            int
	On               bool
}

type crWindowChip struct {
	Label, URL string
	On         bool
}

type crGroupRow struct {
	db.CrashGroup
	URL    string
	On     bool
	Hot    string // dot colour: r busy, o some, "" quiet
	Recent bool
}

type crTrendBar struct {
	N, H int
	Day  string
}

type crDetail struct {
	G         db.CrashGroup
	Events    []db.CrashGroupEvent
	More      int // occurrences not listed
	Trend     []crTrendBar
	TrendMax  int
	FirstSeen time.Time
	// BackURL is the page the delete forms return to (the list without this group picked).
	BackURL string
	// SelfURL is this group picked, where deleting one event returns to.
	SelfURL string
}

type crPage struct {
	Title       string
	Release     db.Release
	CrashGroups []crGroupRow
	Total       int
	Page        int
	TotalPages  int
	PrevURL     string
	NextURL     string
	Kinds       []crKindChip
	Windows     []crWindowChip
	Days        int
	Kind        string
	AllCount    int
	AllDevices  int
	Detail      *crDetail
}

func (h *Handler) buildCrashesCR1(r *http.Request, rel *db.Release) map[string]any {
	q := r.URL.Query()
	page := 1
	if p, e := strconv.Atoi(strings.TrimSpace(q.Get("page"))); e == nil && p > 0 {
		page = p
	}
	days := 0
	switch strings.TrimSpace(q.Get("days")) {
	case "7":
		days = 7
	case "30":
		days = 30
	}
	kind := strings.TrimSpace(q.Get("kind"))
	ctx := r.Context()

	kinds, allDevs, _ := h.db.CrashKindsOnBuild(ctx, rel.Version, days)
	known := false
	allCount := 0
	for _, k := range kinds {
		allCount += k.Count
		if k.Kind == kind {
			known = true
		}
	}
	if !known {
		kind = ""
	}

	// base builds a link that keeps the filters; page and pick are passed in.
	base := "/releases/" + strconv.Itoa(rel.ID) + "/crashes"
	link := func(k string, d, pg int, gk, gs string) string {
		v := url.Values{}
		if k != "" {
			v.Set("kind", k)
		}
		if d > 0 {
			v.Set("days", strconv.Itoa(d))
		}
		if pg > 1 {
			v.Set("page", strconv.Itoa(pg))
		}
		if gs != "" {
			v.Set("gk", gk)
			v.Set("gs", gs)
		}
		if len(v) == 0 {
			return base
		}
		return base + "?" + v.Encode()
	}

	groups, total, _ := h.db.CrashGroupsOnBuildF(ctx, rel.Version, kind, days, crGroupPage, (page-1)*crGroupPage)
	totalPages := (total + crGroupPage - 1) / crGroupPage
	if totalPages < 1 {
		totalPages = 1
	}
	// An out-of-range page (stale link) returns no rows; clamp and re-query.
	if len(groups) == 0 && page > 1 {
		page = 1
		groups, total, _ = h.db.CrashGroupsOnBuildF(ctx, rel.Version, kind, days, crGroupPage, 0)
		totalPages = (total + crGroupPage - 1) / crGroupPage
		if totalPages < 1 {
			totalPages = 1
		}
	}

	// The picked group: ?gk=&gs= when it is on this page, else the first.
	pk, ps := strings.TrimSpace(q.Get("gk")), strings.TrimSpace(q.Get("gs"))
	sel := -1
	for i, g := range groups {
		if g.Kind == pk && g.Signature == ps {
			sel = i
		}
	}
	if sel < 0 && len(groups) > 0 {
		sel = 0
	}

	rows := make([]crGroupRow, 0, len(groups))
	for i, g := range groups {
		hot := ""
		switch {
		case time.Since(g.LastOccurred) < 24*time.Hour && g.Count >= 10:
			hot = "r"
		case time.Since(g.LastOccurred) < 24*time.Hour:
			hot = "o"
		}
		rows = append(rows, crGroupRow{CrashGroup: g, URL: link(kind, days, page, g.Kind, g.Signature), On: i == sel, Hot: hot})
	}

	pg := crPage{
		Title: "Crashes · " + rel.Version, Release: *rel, CrashGroups: rows,
		Total: total, Page: page, TotalPages: totalPages, Days: days, Kind: kind,
		AllCount: allCount, AllDevices: allDevs,
	}
	if page > 1 {
		pg.PrevURL = link(kind, days, page-1, "", "")
	}
	if page < totalPages {
		pg.NextURL = link(kind, days, page+1, "", "")
	}
	pg.Kinds = append(pg.Kinds, crKindChip{Kind: "", Label: "All", Count: allCount, On: kind == "", URL: link("", days, 1, "", "")})
	for _, k := range kinds {
		pg.Kinds = append(pg.Kinds, crKindChip{Kind: k.Kind, Label: k.Kind, Count: k.Count, On: kind == k.Kind, URL: link(k.Kind, days, 1, "", "")})
	}
	pg.Windows = []crWindowChip{
		{"All time", link(kind, 0, 1, "", ""), days == 0},
		{"30 days", link(kind, 30, 1, "", ""), days == 30},
		{"7 days", link(kind, 7, 1, "", ""), days == 7},
	}

	if sel >= 0 {
		g := groups[sel]
		d := &crDetail{G: g, BackURL: link(kind, days, page, "", ""), SelfURL: link(kind, days, page, g.Kind, g.Signature)}
		d.Events, _ = h.db.CrashGroupEvents(ctx, rel.Version, g.Kind, g.Signature, days, crEventLimit)
		if g.Count > len(d.Events) {
			d.More = g.Count - len(d.Events)
		}
		if n := len(d.Events); n > 0 {
			d.FirstSeen = d.Events[n-1].OccurredAt
		}
		if daily, err := h.db.CrashDaily(ctx, rel.Version, g.Kind, g.Signature, crTrendDays); err == nil {
			now := time.Now().UTC()
			for i, n := range daily {
				if n > d.TrendMax {
					d.TrendMax = n
				}
				d.Trend = append(d.Trend, crTrendBar{N: n, Day: now.AddDate(0, 0, i-(crTrendDays-1)).Format("2 Jan")})
			}
			for i := range d.Trend {
				if d.TrendMax > 0 {
					d.Trend[i].H = 4 + d.Trend[i].N*28/d.TrendMax
					if d.Trend[i].N == 0 {
						d.Trend[i].H = 3
					}
				}
			}
		}
		pg.Detail = d
	}

	return map[string]any{
		"Title": pg.Title, "Release": *rel, "CR": pg, "Total": total,
	}
}
