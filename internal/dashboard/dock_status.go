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

var dockCache struct {
	sync.Mutex
	at  map[bool]time.Time // keyed by "hides DPC devices"
	val map[bool]dockStatus
}

// DockStatus serves the live dock's figures as JSON.
func (h *Handler) DockStatus(w http.ResponseWriter, r *http.Request) {
	acc := h.access(r)
	hideDPC := acc.hidesDPC()
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
	st, fresh := dockCache.val[hideDPC], time.Since(dockCache.at[hideDPC]) < dockCacheTTL
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
		st.Alerts, _ = h.db.CountOpenAlerts(ctx)
		st.Critical, st.Running, _ = h.db.DockCounts(ctx)
		dockCache.Lock()
		if dockCache.at == nil {
			dockCache.at, dockCache.val = map[bool]time.Time{}, map[bool]dockStatus{}
		}
		dockCache.at[hideDPC], dockCache.val[hideDPC] = time.Now(), st
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
