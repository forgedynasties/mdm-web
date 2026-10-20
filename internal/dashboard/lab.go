package dashboard

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	prod "mdm/internal/product"
)

// Lab sessions (static/device-lab-view-demos.html, entry A): a device is taken out of
// its restaurant behaviour for a set time and a stated reason. While the session runs it
// does not follow prod, it is not locked, and it raises no restaurant alerts; the lab
// view at /devices/{serial}/lab holds the tools. When the session ends — by its timer,
// by "End lab now", or because the device left its venue — the minute jobs put it back:
// prod build, lock, and the lab-only apps removed.

// labDurations are the lengths the entry dialog offers, in hours.
var labDurations = []int{2, 8, 24, 72}

// canLab reports whether a dashboard role may open lab sessions: the testing and
// firmware teams' accounts and super admins.
func canLab(role string) bool { return roleAtLeast(role, "fleet_manager") }

// requireLab guards the lab routes.
func (h *Handler) requireLab(next http.HandlerFunc) http.HandlerFunc {
	return h.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if !canLab(h.role(r)) {
			http.Error(w, "Lab sessions are for the testing and firmware teams.", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

// labVariant is one package of the role's app, as the lab view's build picker shows it.
type labVariant struct {
	Package   string
	Label     string  // prod, internal, uatv2 …
	Build     *db.App // the channel's build, nil when none is set
	Installed string  // version on the device, "" when not installed
	Chosen    bool
}

// LabView renders the lab view. A device not in lab is sent to its page with the entry
// dialog open, so a stale link can never act on a device outside a session.
func (h *Handler) LabView(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	serial := r.PathValue("serial")
	dev, err := h.db.GetDevice(ctx, serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	sess, err := h.db.GetLabSession(ctx, dev.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if sess == nil {
		http.Redirect(w, r, "/devices/"+url.PathEscape(serial)+"?lab=open", http.StatusSeeOther)
		return
	}
	st, err := h.loadRoleAppState(ctx)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	role := dev.Class()
	base := st.Apps[role]
	var variants []labVariant
	if base != "" {
		versions, _ := h.db.FamilyVersions(ctx, []string{base})
		have := versions[dev.ID]
		seen := map[string]bool{}
		add := func(pkg string) {
			if seen[pkg] {
				return
			}
			seen[pkg] = true
			v := labVariant{Package: pkg, Label: variantLabel(pkg, base), Build: st.Channels[role][pkg], Installed: have[pkg], Chosen: sess.Package == pkg}
			variants = append(variants, v)
		}
		add(base)
		pkgs := make([]string, 0, len(st.Channels[role]))
		for p := range st.Channels[role] {
			pkgs = append(pkgs, p)
		}
		sort.Strings(pkgs)
		for _, p := range pkgs {
			add(p)
		}
		for p := range have {
			add(p)
		}
	}
	labApps, _ := h.db.LabInstalls(ctx, dev.ID)
	library, _ := h.db.ListApps(ctx)
	var installable []db.App
	for _, a := range library {
		if a.PackageName == "" || (base != "" && inFamily(base, a.PackageName)) {
			continue
		}
		installable = append(installable, a)
	}
	modes, _ := h.db.FirmwareModes(ctx)
	h.render(w, r, "device_lab.html", map[string]any{
		"Title":        "Lab · " + serial,
		"Device":       dev,
		"Session":      sess,
		"Online":       h.hub.IsConnected(dev.ID),
		"ShellEnabled": h.cfg.ShellEnabled(),
		"ShellSession": supportsShellSession(dev),
		"ShellQueue":   h.shellQueue(r, dev.ID),
		"RoleLabel":    prod.ClassLabel(role),
		"BasePackage":  base,
		"Variants":     variants,
		"LabApps":      labApps,
		"Installable":  installable,
		"FirmwareMode": db.FirmwareModeOf(modes, modelKey(dev)),
		"AtVenue":      dev.RestaurantID != nil,
		"Durations":    labDurations,
		"Flash":        r.URL.Query().Get("flash"),
	})
}

// labHours reads and validates the requested length.
func labHours(r *http.Request) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("hours")))
	if err != nil {
		return 0, false
	}
	for _, d := range labDurations {
		if d == n {
			return n, true
		}
	}
	return 0, false
}

// LabOpen starts a session. A reason is required: it is what makes "why is Table 14
// unlocked?" answerable by anyone.
func (h *Handler) LabOpen(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	serial := r.PathValue("serial")
	dev, err := h.db.GetDevice(ctx, serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	hours, ok := labHours(r)
	if !ok {
		http.Error(w, "Choose how long the device stays in lab.", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		http.Error(w, "Say why the device goes into lab.", http.StatusBadRequest)
		return
	}
	if len(reason) > 300 {
		reason = reason[:300]
	}
	until := time.Now().Add(time.Duration(hours) * time.Hour)
	if err := h.db.OpenLab(ctx, dev.ID, until, reason, h.currentUsername(r)); err != nil {
		log.Printf("[lab] open %s: %v", serial, err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "device.lab_open", serial, strconv.Itoa(hours)+"h: "+reason)
	go h.reconcileKiosk(context.Background()) // releases its lock now, not in a minute
	h.hub.PublishDeviceUpdate(dev.ID)
	http.Redirect(w, r, "/devices/"+url.PathEscape(serial)+"/lab", http.StatusSeeOther)
}

// LabExtend sets a new end, counted from now.
func (h *Handler) LabExtend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	serial := r.PathValue("serial")
	dev, sess, ok := h.labDevice(w, r)
	if !ok {
		return
	}
	hours, okH := labHours(r)
	if !okH {
		http.Error(w, "Choose how long to extend.", http.StatusBadRequest)
		return
	}
	until := time.Now().Add(time.Duration(hours) * time.Hour)
	if err := h.db.OpenLab(ctx, dev.ID, until, sess.Reason, h.currentUsername(r)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "device.lab_extend", serial, strconv.Itoa(hours)+"h")
	http.Redirect(w, r, "/devices/"+url.PathEscape(serial)+"/lab?flash=extended", http.StatusSeeOther)
}

// LabEnd closes the session now.
func (h *Handler) LabEnd(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	dev, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if err := h.endLab(r.Context(), dev, h.currentUsername(r)); err != nil {
		log.Printf("[lab] end %s: %v", serial, err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "device.lab_end", serial, "")
	http.Redirect(w, r, "/devices/"+url.PathEscape(serial)+"?flash=lab-ended", http.StatusSeeOther)
}

// LabInstall installs a library app that is not the role's app. It is removed again
// when the session ends.
func (h *Handler) LabInstall(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	serial := r.PathValue("serial")
	dev, _, ok := h.labDevice(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(strings.TrimSpace(r.FormValue("app_id")))
	if err != nil {
		http.Error(w, "Choose an app.", http.StatusBadRequest)
		return
	}
	app, err := h.db.GetApp(ctx, id)
	if err != nil || app.PackageName == "" {
		http.Error(w, "App not found", http.StatusNotFound)
		return
	}
	if err := h.queueAppInstall(ctx, dev, app, h.currentUsername(r)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if err := h.db.RecordLabInstall(ctx, dev.ID, app.PackageName); err != nil {
		log.Printf("[lab] record install %s on %s: %v", app.PackageName, serial, err)
	}
	h.audit(r, "device.lab_install", serial, app.PackageName+" "+app.VersionName)
	http.Redirect(w, r, "/devices/"+url.PathEscape(serial)+"/lab?flash=installing", http.StatusSeeOther)
}

// labDevice loads the device and its open session, answering the request itself when
// either is missing.
func (h *Handler) labDevice(w http.ResponseWriter, r *http.Request) (*db.Device, *db.LabSession, bool) {
	dev, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return nil, nil, false
	}
	sess, err := h.db.GetLabSession(r.Context(), dev.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return nil, nil, false
	}
	if sess == nil {
		http.Error(w, "This device is not in lab any more.", http.StatusConflict)
		return nil, nil, false
	}
	return dev, sess, true
}

// endLab closes a session and starts putting the device back: lab-only apps removed,
// lock re-applied. The app job moves it back to prod in its quiet window.
func (h *Handler) endLab(ctx context.Context, dev *db.Device, by string) error {
	pkgs, err := h.db.EndLab(ctx, dev.ID)
	if err != nil {
		return err
	}
	for _, p := range pkgs {
		if !dev.Supports("uninstall") {
			break
		}
		if err := h.queueUninstall(ctx, dev, p, "lab-end"); err != nil {
			log.Printf("[lab] remove %s from %s: %v", p, dev.SerialNumber, err)
		}
	}
	go h.reconcileKiosk(context.Background())
	h.hub.PublishDeviceUpdate(dev.ID)
	return nil
}

// RunLabExpiry ends the sessions whose time is up. Safe to call every minute.
func (h *Handler) RunLabExpiry(ctx context.Context) {
	ids, err := h.db.ExpiredLabDevices(ctx)
	if err != nil {
		log.Printf("[lab] expired: %v", err)
		return
	}
	for _, id := range ids {
		dev, err := h.db.GetDeviceByID(ctx, id)
		if err != nil {
			_, _ = h.db.EndLab(ctx, id)
			continue
		}
		if err := h.endLab(ctx, dev, "lab-expiry"); err != nil {
			log.Printf("[lab] expire %s: %v", dev.SerialNumber, err)
			continue
		}
		log.Printf("[lab] session on %s ended by its timer", dev.SerialNumber)
	}
}
