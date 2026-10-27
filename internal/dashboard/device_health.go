package dashboard

import (
	"context"
	"net/http"
	"sort"
	"time"

	"mdm/internal/db"
)

// Device tabs (T1, picked 19 Oct 2026): the device page's Alerts and Compliance tabs are one
// tab, Health. It opens with what needs attention — the device's open alerts and its failing
// compliance checks together, worst first, each saying which it is — then the compliance
// checklist and the alert history, both still loaded on first open. The Host app tab joined
// Apps the same day. Old ?tab=alerts|compliance|hostapp links land on the merged tabs.

// healthItem is one row of "Needs attention".
type healthItem struct {
	Kind    string // alert | compliance
	CheckID string // the compliance check, for Kind compliance
	Title   string
	Note    string
	Tone    string // danger | warn | info
	Label   string // "alert · critical", "compliance · required"
	Since   time.Time
	rank    int
}

// deviceHealth is the Health tab's head and its badge count.
type deviceHealth struct {
	Items   []healthItem
	Count   int // open alerts + failing required checks: the tab's badge
	Passing int // checks this device passes
	Scored  int // checks scored for it
	Alerts  int // open alerts
}

func (h *Handler) deviceHealthView(ctx context.Context, r *http.Request, device *db.Device) deviceHealth {
	var v deviceHealth
	// The same alerts the old Alerts tab listed (keepVisibleAlerts drops the identity
	// alerts for anyone but a super admin).
	for _, a := range h.access(r).keepVisibleAlerts(mustAlerts(h.db.ListDeviceActiveAlerts(ctx, device.ID, 20))) {
		ha := humanizeAlert(a)
		it := healthItem{Kind: "alert", Title: ha.Headline, Since: ha.FiredAt, Label: "alert · " + ha.Severity}
		switch ha.Severity {
		case "critical":
			it.Tone, it.rank = "danger", 0
		case "warning":
			it.Tone, it.rank = "warn", 2
		default:
			it.Tone, it.rank = "info", 4
		}
		if ha.Muted {
			it.Note = "snoozed"
			it.rank += 1
		}
		v.Items = append(v.Items, it)
		v.Count++
		v.Alerts++
	}
	names := make(map[string]string, len(complianceChecks))
	for _, c := range complianceChecks {
		names[c.ID] = c.Problem
	}
	rs, _ := h.db.DeviceComplianceResults(ctx, device.ID)
	for _, res := range rs {
		name, known := names[res.CheckID]
		if !known {
			continue
		}
		v.Scored++
		switch res.State {
		case db.ComplianceFail:
			it := healthItem{Kind: "compliance", CheckID: res.CheckID, Title: name, Note: res.Detail, Since: res.Since, Label: "compliance · " + res.Level}
			if restates(name, res.Detail) {
				it.Note = ""
			}
			if res.Level == db.ComplianceRequired {
				it.Tone, it.rank = "warn", 1
				v.Count++
			} else {
				it.Tone, it.rank = "info", 3
			}
			v.Items = append(v.Items, it)
		case db.CompliancePass:
			v.Passing++
		}
	}
	sort.SliceStable(v.Items, func(i, j int) bool {
		if v.Items[i].rank != v.Items[j].rank {
			return v.Items[i].rank < v.Items[j].rank
		}
		return v.Items[i].Since.After(v.Items[j].Since)
	})
	return v
}
