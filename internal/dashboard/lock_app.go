package dashboard

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	prod "mdm/internal/product"
)

// Lock goes straight to the host app (picked 19 Oct 2026, "L1"): the device page's Lock
// button locks to the app the device is there to run, named per class by
// product.HostAppPattern, with no list of other apps. Only when several builds of that
// app are installed (….uatv2, ….qa next to prod) does the button open a menu asking which.
// A class with no host app keeps the app picker, since there is nothing to lock to by
// itself.

// LockApp is what the Lock button draws. Every string is final.
type LockApp struct {
	Name     string        // the app's name: "Nugget"
	Variants []LockVariant // installed builds: on screen first, then prod, then by package
	On       bool          // the stored config says locked (mode app)
	Current  string        // the locked package
	Foreign  bool          // locked to a package outside the family
	Label    string        // button text: "Lock" / "Locked"
	Sub      string        // what it is locked to: "Nugget UAT v2", "other app"
}

// LockVariant is one installed build of the host app.
type LockVariant struct {
	Package  string
	Env      string
	EnvTone  string
	Version  string
	OnScreen bool
	Current  bool // the locked package
}

// Menu reports whether the button opens a menu instead of acting by itself: several
// builds, none, or a device locked to some other app (the menu offers the switch and
// Unlock).
func (l *LockApp) Menu() bool {
	return len(l.Variants) != 1 || (l.On && l.Foreign)
}

// hostAppMatcher splits a HostAppPattern into its base package (for naming builds) and a
// match func. A trailing * is a prefix, anything else exact; several are comma-separated
// and the first names the family.
func hostAppMatcher(pattern string) (base string, match func(string) bool) {
	var exact []string
	var prefixes []string
	for _, p := range strings.Split(pattern, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "*") {
			p = strings.TrimSuffix(p, "*")
			prefixes = append(prefixes, p)
		} else {
			exact = append(exact, p)
		}
		if base == "" {
			base = strings.TrimSuffix(p, ".")
		}
	}
	return base, func(pkg string) bool {
		for _, e := range exact {
			if pkg == e {
				return true
			}
		}
		for _, p := range prefixes {
			if strings.HasPrefix(pkg, p) {
				return true
			}
		}
		return false
	}
}

// lockAppFor works out the Lock button for a device, or nil when its class names no
// host app. allowed is the fleet lock allowlist: a build it refuses is not offered.
func lockAppFor(d db.Device, pkgs []db.DevicePackage, cfg *db.DeviceConfig, allowed func(string) bool) *LockApp {
	pattern := prod.HostAppPattern(d.Class())
	if pattern == "" {
		return nil
	}
	base, match := hostAppMatcher(pattern)
	onScreen := screenPackages(d.LatestExtra)
	l := &LockApp{Name: hostAppName(base)}
	if cfg != nil && cfg.KioskEnabled && (cfg.KioskMode == "" || cfg.KioskMode == "app") {
		l.On, l.Current = true, cfg.KioskPackage
	}
	for _, p := range pkgs {
		if !match(p.PackageName) || !allowed(p.PackageName) {
			continue
		}
		env, tone := hostAppEnv(strings.TrimPrefix(strings.TrimPrefix(p.PackageName, base), "."))
		l.Variants = append(l.Variants, LockVariant{
			Package: p.PackageName, Env: env, EnvTone: tone, Version: p.VersionName,
			OnScreen: onScreen[p.PackageName], Current: l.On && p.PackageName == l.Current,
		})
	}
	sort.SliceStable(l.Variants, func(i, j int) bool {
		a, b := l.Variants[i], l.Variants[j]
		if a.OnScreen != b.OnScreen {
			return a.OnScreen
		}
		if (a.Package == base) != (b.Package == base) {
			return a.Package == base
		}
		return a.Package < b.Package
	})
	l.Label = "Lock"
	if l.On {
		l.Foreign = !match(l.Current)
		l.Label, l.Sub = "Locked", l.Name
		for _, v := range l.Variants {
			if v.Current && len(l.Variants) > 1 {
				l.Sub += " " + v.Env
			}
		}
		if l.Foreign {
			l.Sub = "other app"
		}
	}
	return l
}

// screenPackages is what the device says is in front: the host app report's foreground
// build (firmware client) and foreground_pkg (Standard client).
func screenPackages(extra json.RawMessage) map[string]bool {
	var probe struct {
		Fg      string `json:"foreground_pkg"`
		HostApp *struct {
			Variants []struct {
				Package    string `json:"package"`
				Foreground bool   `json:"foreground"`
			} `json:"variants"`
		} `json:"host_app"`
	}
	out := map[string]bool{}
	if json.Unmarshal(extra, &probe) != nil {
		return out
	}
	if probe.Fg != "" {
		out[probe.Fg] = true
	}
	if probe.HostApp != nil {
		for _, v := range probe.HostApp.Variants {
			if v.Foreground {
				out[v.Package] = true
			}
		}
	}
	return out
}

// lockConfirmWait is how long Lock waits for the device to answer a ping sent after the
// config frame. The socket is ordered, so the answer means the device has the lock.
const lockConfirmWait = 8 * time.Second

// DeviceLockHost locks a device to a build of its host app (on=1, package) or unlocks it
// (on=0), then waits for the device to answer. JSON: the new button label and whether
// the device has it ("delivered") or will get it at its next check-in.
func (h *Handler) DeviceLockHost(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	r.ParseForm()
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "Device not found")
		return
	}
	if !h.requireDeviceAction(w, r, "kiosk", device.ID) {
		return
	}
	cfg, err := h.db.GetOrCreateDeviceConfig(r.Context(), device.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal error")
		return
	}
	pkgs, err := h.db.GetDevicePackages(r.Context(), device.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal error")
		return
	}
	la := lockAppFor(*device, pkgs, cfg, h.cfg.KioskAppAllowed)
	if la == nil {
		writeJSONError(w, http.StatusBadRequest, "This device has no host app to lock to")
		return
	}
	on := r.FormValue("on") == "1"
	pkg := cfg.KioskPackage
	if on {
		pkg = strings.TrimSpace(r.FormValue("package"))
		ok := false
		for _, v := range la.Variants {
			ok = ok || v.Package == pkg
		}
		if !ok {
			writeJSONError(w, http.StatusBadRequest, la.Name+" isn't on this device. Install it, then lock.")
			return
		}
	}
	if err := h.db.SetKioskConfig(r.Context(), device.ID, on, pkg, 0); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal error")
		return
	}
	if err := h.db.SetKioskModeConfig(r.Context(), device.ID, "app", nil, "", nil); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal error")
		return
	}
	rule := h.kioskRuleFor(r.Context(), *device)
	_ = h.db.MarkKioskManual(r.Context(), device.ID, rule != nil)
	detail := boolWord(on)
	if on {
		detail += " (" + pkg + ")"
	}
	if rule != nil {
		detail += ", overrides rule " + rule.Name
	}
	h.auditDev(r, "device.kiosk", device.ID, serial, detail)
	h.pushKioskConfigToDevices(r.Context(), []uuid.UUID{device.ID})

	delivered := false
	if h.hub.IsConnected(device.ID) {
		nonce := uuid.New().String()
		ch := h.hub.RegisterPingWaiter(nonce)
		msg, _ := json.Marshal(map[string]any{"type": "ping_request", "nonce": nonce})
		if h.hub.Push(device.ID, msg) {
			select {
			case <-ch:
				delivered = true
			case <-time.After(lockConfirmWait):
			case <-r.Context().Done():
			}
		}
		h.hub.UnregisterPingWaiter(nonce)
	}

	cfg.KioskEnabled, cfg.KioskPackage, cfg.KioskMode = on, pkg, "app"
	after := lockAppFor(*device, pkgs, cfg, h.cfg.KioskAppAllowed)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"on": on, "package": pkg, "label": after.Label, "sub": after.Sub, "delivered": delivered,
	})
}
