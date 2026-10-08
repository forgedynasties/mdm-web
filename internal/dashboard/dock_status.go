package dashboard

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"mdm/internal/db"
)

// dockStatus is what the live dock shows next to its links: the online ring on
// Fleet, the open and critical counts on Alerts, the spinner on Actions.
type dockStatus struct {
	Fleet    bool `json:"fleet"` // false when the viewer only sees part of the fleet
	Online   int  `json:"online,omitempty"`
	Total    int  `json:"total,omitempty"`
	Alerts   int  `json:"alerts"`
	Critical int  `json:"critical"`
	Running  int  `json:"running"`
}

// dockCacheTTL: every open tab polls this, so the fleet-wide figures are shared
// for a few seconds instead of re-queried per tab.
const dockCacheTTL = 15 * time.Second

// dockKey keys the shared dock figures by what the viewer's role changes about
// them: whether DPC devices are excluded, and whether identity ("Possible
// impersonation") alerts count — super admin only, so a non-admin must not be
// served an admin's cached figure, or the badge would show an alert they cannot open.
type dockKey struct{ hideDPC, hideIdentity bool }

var dockCache struct {
	sync.Mutex
	at  map[dockKey]time.Time
	val map[dockKey]dockStatus
}

// DockStatus serves the live dock's figures as JSON.
func (h *Handler) DockStatus(w http.ResponseWriter, r *http.Request) {
	acc := h.access(r)
	hideDPC := acc.hidesDPC()
	key := dockKey{hideDPC: hideDPC, hideIdentity: acc.hidesIdentityAlerts()}
	// Someone who sees part of the fleet gets counts of their part, never the shared
	// fleet-wide figures (and never from the shared cache).
	if acc.hidesDevices() {
		st := dockStatus{}
		st.Alerts, st.Critical = h.visibleAlertCounts(r)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(st)
		return
	}

	dockCache.Lock()
	st, fresh := dockCache.val[key], time.Since(dockCache.at[key]) < dockCacheTTL
	dockCache.Unlock()

	if !fresh {
		ctx := r.Context()
		var sum db.Summary
		var err error
		if hideDPC {
			sum, err = h.db.GetSummaryFiltered(ctx, db.DeviceFilter{AgentKind: "firmware", Connected: h.connectedSlice(), ActiveThresholdSecs: h.cfg.CheckinInterval() * 3})
		} else {
			sum, err = h.db.GetSummary(ctx, h.connectedSlice())
		}
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		st = dockStatus{Online: sum.RecentlyActive, Total: sum.Total}
		st.Alerts, _ = h.db.CountOpenAlerts(ctx, key.hideIdentity)
		st.Critical, st.Running, _ = h.db.DockCounts(ctx, key.hideIdentity)
		dockCache.Lock()
		if dockCache.at == nil {
			dockCache.at, dockCache.val = map[dockKey]time.Time{}, map[dockKey]dockStatus{}
		}
		dockCache.at[key], dockCache.val[key] = time.Now(), st
		dockCache.Unlock()
	}

	// Someone who only sees part of the fleet gets no fleet-wide ring.
	st.Fleet = !acc.hidesDevices()
	if !st.Fleet {
		st.Online, st.Total = 0, 0
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(st)
}

// visibleAlertCounts is the open and critical (unsnoozed) alert counts over the alerts
// this user can see.
func (h *Handler) visibleAlertCounts(r *http.Request) (open, critical int) {
	active, err := h.db.ListActiveAlerts(r.Context(), 5000)
	if err != nil {
		return 0, 0
	}
	now := time.Now()
	for _, a := range h.access(r).keepVisibleAlerts(active) {
		if a.Status != "open" {
			continue
		}
		open++
		if a.Severity == "critical" && (a.MutedUntil == nil || a.MutedUntil.Before(now)) {
			critical++
		}
	}
	return open, critical
}
