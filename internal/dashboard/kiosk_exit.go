package dashboard

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Kiosk exits on site (kiosk-exit demo: the strip on the Kiosk page, the notice on the
// device page, and an alert). A technician who leaves kiosk on the device with the exit
// PIN or code takes it out until someone here answers: "Lock again" puts it back to what
// locked it (its rule, or its own setting), "Leave it out" keeps it unlocked and, under
// a rule, makes it an exception like a device changed by hand.

// kioskExitBack is where an answer returns to: the page it was given from.
func kioskExitBack(r *http.Request, serial string) string {
	if b := r.FormValue("back"); strings.HasPrefix(b, "/") && !strings.HasPrefix(b, "//") {
		return b
	}
	return "/devices/" + url.PathEscape(serial)
}

// DeviceKioskRelock is POST /devices/{serial}/kiosk/relock.
func (h *Handler) DeviceKioskRelock(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	ctx := r.Context()
	dev, err := h.db.GetDevice(ctx, serial)
	if err != nil || dev == nil {
		http.NotFound(w, r)
		return
	}
	back := kioskExitBack(r, serial)
	ex, err := h.db.ClearKioskExit(ctx, dev.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if ex == nil {
		h.hxRedirect(w, r, withFlash(back, "It had already been answered."))
		return
	}
	how := "its own kiosk setting"
	if ex.RuleID != nil && h.kioskRuleFor(ctx, *dev) != nil {
		// Back under its rule: the enforcer locks it to whatever the rule says now.
		_ = h.db.SetKioskOverride(ctx, dev.ID, false)
		h.reconcileKiosk(ctx)
		how = "its kiosk rule"
	} else if ex.Package != "" {
		if err := h.db.SetKioskConfig(ctx, dev.ID, true, ex.Package, 0); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		_ = h.db.MarkKioskManual(ctx, dev.ID, false)
		h.pushKioskConfigToDevices(ctx, []uuid.UUID{dev.ID})
	}
	_, _ = h.db.ResolveOpenAlert(ctx, "kiosk_exited", dev.ID)
	h.hub.PublishAlertUpdate()
	h.auditDev(r, "kiosk.relock", dev.ID, serial, "after an exit on site, by "+how)
	h.hxRedirect(w, r, withFlash(back, "Locked again. It locks at its next check-in."))
}

// DeviceKioskLeaveOut is POST /devices/{serial}/kiosk/leave-out.
func (h *Handler) DeviceKioskLeaveOut(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	ctx := r.Context()
	dev, err := h.db.GetDevice(ctx, serial)
	if err != nil || dev == nil {
		http.NotFound(w, r)
		return
	}
	back := kioskExitBack(r, serial)
	ex, err := h.db.ClearKioskExit(ctx, dev.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if ex == nil {
		h.hxRedirect(w, r, withFlash(back, "It had already been answered."))
		return
	}
	// Under a rule it becomes an exception, so the enforcer leaves it unlocked.
	_ = h.db.MarkKioskManual(ctx, dev.ID, h.kioskRuleFor(ctx, *dev) != nil)
	_, _ = h.db.ResolveOpenAlert(ctx, "kiosk_exited", dev.ID)
	h.hub.PublishAlertUpdate()
	h.auditDev(r, "kiosk.leave_out", dev.ID, serial, "after an exit on site")
	h.hxRedirect(w, r, withFlash(back, "Left out of kiosk."))
}

// withFlash adds the page's flash message to a local URL.
func withFlash(to, msg string) string {
	sep := "?"
	if strings.Contains(to, "?") {
		sep = "&"
	}
	if i := strings.Index(to, "#"); i >= 0 {
		return to[:i] + sep + "flash=" + url.QueryEscape(msg) + to[i:]
	}
	return to + sep + "flash=" + url.QueryEscape(msg)
}

// kioskExitView is an exit decorated for the Kiosk page and the device page.
type kioskExitView struct {
	db.KioskExit
	AppName string
}

func appNameOf(apps map[string]db.FleetPackage, pkg string) string {
	if a, ok := apps[pkg]; ok && a.AppName != "" {
		return a.AppName
	}
	return pkg
}

// kioskExitFor is the device page's notice, nil when the device has no unanswered exit.
func (h *Handler) kioskExitFor(r *http.Request, dev *db.Device) *kioskExitView {
	if dev == nil {
		return nil
	}
	ex := h.db.GetKioskExit(r.Context(), dev.ID)
	if ex == nil {
		return nil
	}
	return &kioskExitView{KioskExit: *ex, AppName: appNameOf(h.fleetAppIndex(r), ex.Package)}
}

// lateSpansFor is the device page's first set of offline spans (readings sent later),
// the last two weeks; the chart adds more as it loads other windows.
func (h *Handler) lateSpansFor(r *http.Request, dev *db.Device) []db.TimeSpan {
	now := time.Now()
	spans, err := h.db.LateSpans(r.Context(), dev.ID, now.Add(-14*24*time.Hour), now, time.Duration(chartGapMs(dev.PollIntervalMs))*time.Millisecond)
	if err != nil || spans == nil {
		return []db.TimeSpan{}
	}
	return spans
}
