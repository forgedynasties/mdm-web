package dashboard

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"time"

	"mdm/internal/db"
)

// deviceActivityItems phrases a device's audit entries for reading, newest first, at most
// limit of them. Admin activity is for admins: the built-in admin login, the admin API key
// and any admin account; everyone else sees what operators and the device itself did.
func (h *Handler) deviceActivityItems(ctx context.Context, role string, entries []db.AuditEntry, limit int) []deviceActivityItem {
	names := h.actorDisplayNames(ctx)
	adminActors := map[string]bool{"admin": true, "api": true}
	if role != "admin" {
		if users, err := h.db.ListUsers(ctx); err == nil {
			for _, u := range users {
				if u.Role == "admin" {
					adminActors[u.Username] = true
				}
			}
		}
	}
	items := make([]deviceActivityItem, 0, limit)
	for _, e := range entries {
		if len(items) == limit {
			break
		}
		if role != "admin" && adminActors[e.Actor] {
			continue
		}
		actor := e.Actor
		if dn, ok := names[actor]; ok && dn != "" {
			actor = dn
		}
		items = append(items, deviceActivityItem{At: e.CreatedAt, Actor: actor, ActorKey: e.Actor, Did: humanizeAudit(e), Action: e.Action})
	}
	return items
}

// ── The device page's Activity tab: one timeline (A1, picked 16 Oct) ──────────
//
// Commands sent to the device and the changes made from its page, in one list, newest
// first. The queue keeps its own strip above it (it is live and has actions). The daily
// control-probe canary is hidden unless asked for: a heartbeat should not push the
// actions people took down the list.

type timelineItem struct {
	At     time.Time
	Cmd    *db.DeviceCommand
	Change *deviceActivityItem
}

const timelinePage = 30

// timelineRecent is how many rows the Recent on this device card shows.
const timelineRecent = 10

func timelineIsProbe(c db.DeviceCommand) bool {
	return c.Type == "canary" || c.CreatedBy == "control probe"
}

func (h *Handler) DeviceTimelinePartial(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	device, err := h.db.GetDevice(ctx, r.PathValue("serial"))
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	role := h.role(r)
	show := r.URL.Query().Get("show")
	if show != "commands" && show != "changes" {
		show = "all"
	}
	probe := r.URL.Query().Get("probe") == "1"
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n < timelinePage {
		n = timelinePage
	}
	// The "Recent on this device" card under the split: the latest few, no probes.
	tpl := "device-timeline"
	if r.URL.Query().Get("recent") == "1" {
		tpl, show, probe, n = "device-recent", "all", false, timelineRecent
	}

	cmds, err := h.db.GetDeviceCommands(ctx, device.ID, h.cfg.CommandExpiry())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	redactDeviceCommandURLs(role, cmds)
	cmds = filterShellDeviceCommands(role, cmds)
	entries, _ := h.db.ListAuditForDevice(ctx, device.ID, 500)
	changes := h.deviceActivityItems(ctx, role, entries, 500)

	var items []timelineItem
	nCmd, nProbe, pending := 0, 0, false
	for i := range cmds {
		c := &cmds[i]
		if timelineIsProbe(*c) {
			nProbe++
			if !probe {
				continue
			}
		}
		nCmd++
		if c.Status == "pending" || c.Status == "delivered" || c.Status == "running" || c.Status == "downloading" || c.Status == "installing" {
			pending = true
		}
		if show != "changes" {
			items = append(items, timelineItem{At: c.CreatedAt, Cmd: c})
		}
	}
	if show != "commands" {
		for i := range changes {
			items = append(items, timelineItem{At: changes[i].At, Change: &changes[i]})
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].At.After(items[j].At) })
	more := len(items) > n
	if more {
		items = items[:n]
	}
	h.renderCachedHTML(w, r, tpl, h.withRole(r, map[string]any{
		"Device":   device,
		"Items":    items,
		"Show":     show,
		"Probe":    probe,
		"NCmd":     nCmd,
		"NChange":  len(changes),
		"NProbe":   nProbe,
		"More":     more,
		"Next":     n + timelinePage*2,
		"Pending":  pending,
		"Online":   h.hub.IsConnectedForDisplay(device.ID),
	}))
}
