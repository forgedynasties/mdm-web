package dashboard

import (
	"fmt"
	"strings"
	"time"

	"mdm/internal/db"
	"mdm/internal/product"
)

// attentionRow is one line of the Overview's "Needs attention" list: an active
// alert, or several of the same type at one restaurant folded together the way
// the Alerts page folds them, plus the onboarding inbox as an info row.
type attentionRow struct {
	Severity   string // critical | warning | info
	Issue      string
	Serial     string // set when exactly one device is affected
	Devices    int
	Restaurant string
	Since      time.Time // oldest member's fired_at; zero when unknown
	Href       string
}

// overviewAttentionLimit is how many rows the Overview shows before "View all".
const overviewAttentionLimit = 8

// buildAttention turns active alerts into the Overview's worst-first worklist.
// Snoozed alerts are left out: someone already decided they can wait. It returns
// the rows to show, the total number of rows, and how many are critical.
func buildAttention(alerts []db.Alert, inboxN int) (rows []attentionRow, total, critical int) {
	var crit, warn, info []humanAlert
	for _, a := range alerts {
		ha := humanizeAlert(a)
		if ha.Muted {
			continue
		}
		switch a.Severity {
		case "critical":
			crit = append(crit, ha)
		case "warning":
			warn = append(warn, ha)
		default:
			info = append(info, ha)
		}
	}
	var all []attentionRow
	for _, bucket := range [][]humanAlert{crit, warn, info} {
		for _, g := range groupAlerts(bucket) {
			all = append(all, attentionFromGroup(g))
		}
	}
	critical = len(groupAlerts(crit))
	if inboxN > 0 {
		all = append(all, attentionRow{
			Severity: "info", Issue: "Waiting for a restaurant", Devices: inboxN,
			Href: "/enrollment",
		})
	}
	total = len(all)
	if len(all) > overviewAttentionLimit {
		all = all[:overviewAttentionLimit]
	}
	return all, total, critical
}

func attentionFromGroup(g alertGroup) attentionRow {
	lead := g.Lead
	row := attentionRow{
		Severity: lead.Severity, Issue: attentionIssue(lead),
		Devices: g.DeviceCount, Restaurant: lead.Restaurant, Since: lead.FiredAt,
	}
	for _, m := range g.Members {
		if m.FiredAt.Before(row.Since) {
			row.Since = m.FiredAt
		}
	}
	view := "watching"
	if lead.Severity == "critical" {
		view = "needs"
	}
	row.Href = "/alerts?view=" + view
	if g.DeviceCount == 1 && lead.Serial != "" {
		row.Serial = lead.Serial
		row.Href = "/alerts?device=" + lead.Serial
	}
	return row
}

// attentionIssue names an alert by its catalog label ("Device offline"), which
// reads for one device or ten, falling back to the humanized headline for types
// outside the catalog. Crashes name the app when the summary carries it.
func attentionIssue(a humanAlert) string {
	if a.Type == "device_crash" {
		if pkg := a.PackageName; pkg != "" {
			return "App crash · " + pkg
		}
		return "App crash"
	}
	if l := alertTypeLabel(a.Type); l != a.Type {
		return l
	}
	return a.Headline
}

// classOnlineRow is one line of the Overview's devices-by-type table.
type classOnlineRow struct {
	Class  string
	Label  string
	Total  int
	Online int
}

func classOnlineRows(cs []db.ClassOnline) []classOnlineRow {
	out := make([]classOnlineRow, 0, len(cs))
	for _, c := range cs {
		label := "Unassigned"
		if c.Class != "" {
			label = product.ClassLabel(c.Class)
		}
		out = append(out, classOnlineRow{c.Class, label, c.Total, c.Online})
	}
	return out
}

// mainIssue is the first penalty behind a restaurant's score, without the
// "(−N)" points suffix, for the Overview's restaurants table.
func mainIssue(why []string) string {
	if len(why) == 0 {
		return ""
	}
	s := why[0]
	if i := strings.LastIndex(s, " (−"); i > 0 {
		s = s[:i]
	}
	return s
}

// sneakPeekOverviewExtras fills the Overview fields the live handler reads from
// alerts and the hub, from the synthetic fleet: one attention row per at-risk
// venue and a devices-by-type split of the synthetic totals.
func sneakPeekOverviewExtras(data map[string]any, summary db.Summary, groups []db.GroupHealth) {
	now := time.Now()
	var rows []attentionRow
	for i, g := range groups {
		if g.ScoreClass != "danger" {
			continue
		}
		if g.OfflineCount > 0 {
			rows = append(rows, attentionRow{Severity: "critical", Issue: "Device offline", Devices: g.OfflineCount,
				Restaurant: g.Name, Since: now.Add(-time.Duration(40+i*25) * time.Minute), Href: "/sneak-peek/alerts"})
		}
		if g.TempMax != nil && g.TempMaxSerial != nil {
			rows = append(rows, attentionRow{Severity: "warning", Issue: "Device overheating", Devices: 1, Serial: *g.TempMaxSerial,
				Restaurant: g.Name, Since: now.Add(-time.Duration(12+i*7) * time.Minute), Href: "/sneak-peek/alerts"})
		}
	}
	crit := 0
	for _, r := range rows {
		if r.Severity == "critical" {
			crit++
		}
	}
	data["AttentionRows"] = rows
	data["AttentionTotal"] = len(rows)
	data["AttentionCritical"] = crit

	split := []struct {
		class string
		pct   int
	}{{"t7", 62}, {"kiosk", 18}, {"dongle", 12}, {"kds", 8}}
	var cls []classOnlineRow
	left, leftOn := summary.Total, summary.RecentlyActive
	for i, c := range split {
		n, on := summary.Total*c.pct/100, summary.RecentlyActive*c.pct/100
		if i == len(split)-1 {
			n, on = left, leftOn
		}
		left, leftOn = left-n, leftOn-on
		cls = append(cls, classOnlineRow{c.class, product.ClassLabel(c.class), n, on})
	}
	data["ClassOnline"] = cls
	if summary.Total > 0 {
		data["OnlinePct"] = fmt.Sprintf("%.1f", float64(summary.RecentlyActive)*100/float64(summary.Total))
	}
}
