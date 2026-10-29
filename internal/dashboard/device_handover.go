package dashboard

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

// Hand over (roles replan, decided 30 Oct 2026): a device placed at a venue is "setting
// up" until Field (or the Ops lead) hands it over; from then on it is "in service" and
// Support, Sales and the owner's staff see it. Hand over is allowed once the checklist is
// green: the role's app is installed, the device is locked, and it has checked in.

type handOverCheck struct {
	Label string
	OK    bool
	Fix   string // what is missing, in a few words
}

type handOverView struct {
	State     string // db.StateLab | StateSettingUp | StateInService
	Checks    []handOverCheck
	Ready     bool
	CanHand   bool // this viewer may hand it over
	HandedAt  *time.Time
	HandedBy  string
	VenueName string
	VenueID   string
	VisitEnds *time.Time // the viewer's open visit there, for Field
}

// handOverChecks is the device's checklist: role app, locked, checked in.
func (h *Handler) handOverChecks(ctx context.Context, d *db.Device, kioskOn bool) ([]handOverCheck, bool) {
	var out []handOverCheck
	base := h.db.RoleAppBase(ctx, d.Class())
	appOK := base == ""
	if !appOK {
		if pkgs, err := h.db.GetDevicePackages(ctx, d.ID); err == nil {
			for _, p := range pkgs {
				if p.PackageName == base || strings.HasPrefix(p.PackageName, base+".") {
					appOK = true
					break
				}
			}
		}
	}
	app := handOverCheck{Label: "app", OK: appOK}
	if !appOK {
		app.Fix = base + " not installed"
	}
	out = append(out, app)
	lock := handOverCheck{Label: "lock", OK: kioskOn}
	if !kioskOn {
		lock.Fix = "not locked"
	}
	out = append(out, lock)
	online := h.hub.IsConnected(d.ID) || (!d.LastSeenAt.IsZero() && time.Since(d.LastSeenAt) < 10*time.Minute)
	seen := handOverCheck{Label: "online", OK: online}
	if !online {
		seen.Fix = "not checked in lately"
	}
	out = append(out, seen)
	ready := true
	for _, c := range out {
		ready = ready && c.OK
	}
	return out, ready
}

// mayHandOver: the super admin, the Ops lead, and Field at a venue of their open visit.
func (h *Handler) mayHandOver(r *http.Request, d *db.Device) bool {
	switch h.role(r) {
	case "admin":
		return true
	case "fleet_admin", "fleet_manager", "field":
		return h.access(r).canDevice("notes", d.ID)
	}
	return false
}

// deviceHandOverView builds the device page's setup strip (nil for lab devices).
func (h *Handler) deviceHandOverView(r *http.Request, d *db.Device, kioskOn bool) *handOverView {
	if d.RestaurantID == nil {
		return nil
	}
	ctx := r.Context()
	at, by, _ := h.db.DeviceHandOver(ctx, d.ID)
	v := &handOverView{State: db.DeviceState(d.RestaurantID, at != nil), HandedAt: at, HandedBy: by,
		VenueName: d.RestaurantName, VenueID: d.RestaurantID.String()}
	if v.State == db.StateSettingUp {
		v.Checks, v.Ready = h.handOverChecks(ctx, d, kioskOn)
		v.CanHand = h.mayHandOver(r, d)
		if h.role(r) == "field" {
			if vs, err := h.db.OpenVisitsFor(ctx, h.currentUsername(r)); err == nil {
				for _, x := range vs {
					if x.RestaurantID == *d.RestaurantID {
						e := x.EndsAt
						v.VisitEnds = &e
						break
					}
				}
			}
		}
	}
	return v
}

// DeviceHandOver: POST /devices/{serial}/handover. Allowed once the checklist is green.
func (h *Handler) DeviceHandOverPost(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	back := "/devices/" + serial
	if b := r.FormValue("back"); strings.HasPrefix(b, "/") && !strings.HasPrefix(b, "//") {
		back = b
	}
	d, err := h.db.GetDevice(r.Context(), serial)
	if err != nil || d == nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.mayHandOver(r, d) {
		http.Error(w, "You can't hand this device over.", http.StatusForbidden)
		return
	}
	if d.RestaurantID == nil {
		h.hxDoneToast(w, r, back, "Place it at a venue first", "error")
		return
	}
	cfg, _ := h.db.GetOrCreateDeviceConfig(r.Context(), d.ID)
	if _, ready := h.handOverChecks(r.Context(), d, cfg != nil && cfg.KioskEnabled); !ready {
		h.hxDoneToast(w, r, back, "Not ready yet: finish its checklist first", "error")
		return
	}
	n, err := h.db.HandOver(r.Context(), []uuid.UUID{d.ID}, h.currentUsername(r))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if n == 0 {
		h.hxDoneToast(w, r, back, "It is in service already", "success")
		return
	}
	h.audit(r, "device.handover", serial, d.RestaurantName)
	h.hub.PublishDeviceUpdate(d.ID)
	h.hxDoneToast(w, r, back, serial+" handed over · in service at "+d.RestaurantName, "success")
}

// classOf is the device's class label for setup rows.
func classOf(d db.Device) string { return product.ClassLabel(d.Class()) }
