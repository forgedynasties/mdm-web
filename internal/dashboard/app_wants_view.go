package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// The Apps tab's "On its way" box (A2, picked 17 Nov 2026) and the endpoints behind Add
// app, Remove, Cancel and Try again (D1). See app_wants.go for the job.

// wantRow is one line of the box: an app being added or removed that isn't done yet.
type wantRow struct {
	Package  string
	Name     string
	Icon     string
	Channel  string // "UAT" for a channel build, "" for prod or a plain app
	Remove   bool
	State    string // waiting | installing | failed | removing | nolib
	Text     string
	Pct      int // download percent, -1 when unknown
	CanRetry bool
	Why      installWhy // the real reason a failed install failed (install_why.go)
}

// deviceWantsView is the box for one device: wants not met yet, oldest first.
func (h *Handler) deviceWantsView(ctx context.Context, d *db.Device) []wantRow {
	ws, err := h.db.AppWantsOf(ctx, d.ID)
	if err != nil || len(ws) == 0 {
		return nil
	}
	pkgs, _ := h.db.GetDevicePackages(ctx, d.ID)
	have := map[string]bool{}
	for _, p := range pkgs {
		have[p.PackageName] = true
	}
	reported := len(pkgs) > 0
	fams, _, _ := h.libraryData(ctx)
	pending, _ := h.db.PendingInstalls(ctx, d.ID)
	fails, _ := h.db.WantFailures(ctx, d.ID)
	online := h.deviceOnline(d)
	var out []wantRow
	for _, w := range ws {
		r := wantRow{Package: w.Package, Name: w.Package, Remove: w.Want == db.WantAbsent, Pct: -1}
		inLib := false
		for _, fam := range fams {
			for _, vr := range fam.Variants {
				if vr.Package == w.Package {
					r.Name, r.Icon, inLib = fam.Name, fam.Icon, vr.Latest.ApkURL != ""
					if len(fam.Variants) > 1 && vr.Label != "prod" {
						r.Channel = channelName(vr.Label)
					}
				}
			}
		}
		if r.Remove {
			if !have[w.Package] {
				continue // gone: done
			}
			r.State = "removing"
			switch f, ok := fails[w.Package]; {
			case ok && f.Count >= 3:
				r.State, r.CanRetry = "failed", true
				r.Text = "Couldn't remove · " + shortReason(f.Output) + fmt.Sprintf(" · tried %d times today", f.Count)
			case !online:
				r.Text = "Removing · waits for the device · offline " + ago(d.LastSeenAt)
			default:
				r.Text = "Removing"
			}
			out = append(out, r)
			continue
		}
		if have[w.Package] {
			continue // on the device: done
		}
		p, inFlight := pending[w.Package]
		f, failed := fails[w.Package]
		switch {
		case inFlight:
			r.State = "installing"
			r.Text = "Installing"
			if p.Status == "downloading" && p.Percent >= 0 {
				r.Pct = p.Percent
				r.Text = fmt.Sprintf("Downloading · %d %%", p.Percent)
			}
		case !inLib:
			r.State, r.Text = "nolib", "No build of it in the library"
		case failed && (f.Count >= 3 || online):
			r.State, r.CanRetry = "failed", true
			r.Text = "Couldn't install · " + shortReason(f.Output)
			if f.Count > 1 {
				r.Text += fmt.Sprintf(" · tried %d times today", f.Count)
			}
		case !online:
			r.State, r.Text = "waiting", "Waiting for the device · offline "+ago(d.LastSeenAt)
		case !reported:
			r.State, r.Text = "waiting", "Waiting for the device's app list"
		default:
			r.State, r.Text = "waiting", "Installs within a minute"
		}
		out = append(out, r)
	}
	return out
}

// channelName is a channel label as people say it.
func channelName(l string) string {
	switch l {
	case "uatv2", "uat":
		return "UAT"
	case "stagev2", "staging":
		return "Stage"
	case "qa":
		return "QA"
	case "internal":
		return "Internal"
	}
	if l == "" {
		return ""
	}
	return strings.ToUpper(l[:1]) + l[1:]
}

// shortReason is the first useful line of a device's failure output.
func shortReason(out string) string {
	out = strings.TrimSpace(out)
	if r := ParseInstallFailure(out, ""); r.Code != "" && r.Plain != "" {
		return strings.ToLower(r.Plain[:1]) + strings.TrimSuffix(r.Plain[1:], ".")
	}
	switch {
	case out == "":
		return "no answer from the device"
	case strings.Contains(out, "INSUFFICIENT_STORAGE"), strings.Contains(strings.ToLower(out), "no space"):
		return "not enough space"
	case strings.Contains(out, "UPDATE_INCOMPATIBLE"), strings.Contains(out, "signatures do not match"):
		return "signed with another key than the app on the device"
	case strings.Contains(out, "VERSION_DOWNGRADE"):
		return "the device has a newer build"
	}
	if i := strings.IndexByte(out, '\n'); i > 0 {
		out = out[:i]
	}
	if len(out) > 90 {
		out = out[:90] + "…"
	}
	return out
}

// ago is "3 h", "2 d", "40 min".
func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case t.IsZero():
		return "a while"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d d", int(d.Hours()/24))
}

// wantsBusy is the packages the box shows, so the list's own "Installing…" rows leave them out.
func wantsBusy(rows []wantRow) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		out[r.Package] = true
	}
	return out
}

// dropWantRows leaves out of the in-flight install rows the apps the box already shows.
func dropWantRows(rows []map[string]string, apps []db.App, apkPkg map[string]string, busy map[string]bool) []map[string]string {
	if len(busy) == 0 {
		return rows
	}
	pkgOf := map[string]string{}
	for _, a := range apps {
		pkgOf[a.ApkURL] = a.PackageName
	}
	var out []map[string]string
	for _, r := range rows {
		u := r["ApkURL"]
		p := pkgOf[u]
		if p == "" {
			p = apkPkg[u]
		}
		if busy[p] {
			continue
		}
		out = append(out, r)
	}
	return out
}

// ── Endpoints ───────────────────────────────────────────────────────────────

func (h *Handler) wantDevice(w http.ResponseWriter, r *http.Request, cmdType string) *db.Device {
	if !h.commandTypeAllowed(h.role(r), cmdType) {
		http.Error(w, "Your role can't do that.", http.StatusForbidden)
		return nil
	}
	d, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil || d == nil {
		http.NotFound(w, r)
		return nil
	}
	return d
}

func (h *Handler) deviceOnline(d *db.Device) bool {
	// A connected socket is online whatever last_seen says: a device on a WebSocket checks in
	// rarely, and the page's own Online badge goes by the socket (20 Nov 2026: "Waiting for the
	// device · offline 4 min" on a device the header called Online).
	if h.hub != nil && h.hub.IsConnected(d.ID) {
		return true
	}
	return time.Since(d.LastSeenAt) <= time.Duration(h.cfg.CheckinInterval()*roleAppSeenFactor)*time.Second
}

// libPackages maps the form's apk_url / package values to library packages.
func (h *Handler) libPackages(ctx context.Context, r *http.Request) (pkgs []string, names map[string]string) {
	apps, _ := h.db.ListApps(ctx)
	byURL, byPkg := map[string]*db.App{}, map[string]*db.App{}
	for i := range apps {
		a := &apps[i]
		byURL[a.ApkURL] = a
		if a.PackageName != "" {
			byPkg[a.PackageName] = a
		}
	}
	names = map[string]string{}
	seen := map[string]bool{}
	add := func(a *db.App) {
		if a == nil || a.PackageName == "" || seen[a.PackageName] {
			return
		}
		seen[a.PackageName] = true
		pkgs = append(pkgs, a.PackageName)
		names[a.PackageName] = a.Name
	}
	for _, u := range r.Form["apk_url"] {
		add(byURL[strings.TrimSpace(u)])
	}
	for _, p := range r.Form["package"] {
		add(byPkg[strings.TrimSpace(p)])
	}
	return pkgs, names
}

// DeviceAppAdd is POST /devices/{serial}/apps/add (apk_url or package, one or more):
// adds the apps to the device; online, the first install goes now.
func (h *Handler) DeviceAppAdd(w http.ResponseWriter, r *http.Request) {
	d := h.wantDevice(w, r, "install_apk")
	if d == nil {
		return
	}
	r.ParseForm()
	back := "/devices/" + d.SerialNumber + "?tab=apps"
	pkgs, names := h.libPackages(r.Context(), r)
	if len(pkgs) == 0 {
		h.hxDoneToast(w, r, back, "That app has no build in the library", "error")
		return
	}
	by := h.currentUsername(r)
	for _, p := range pkgs {
		if err := h.db.SetAppWant(r.Context(), d.ID, p, db.WantPresent, by); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		h.audit(r, "app.add", d.SerialNumber, p)
	}
	h.applyWants(r.Context(), []uuid.UUID{d.ID}, true)
	what := names[pkgs[0]]
	if what == "" {
		what = pkgs[0]
	}
	if len(pkgs) > 1 {
		what = fmt.Sprintf("%d apps", len(pkgs))
	}
	msg := "Added " + what + " · installing now"
	if !h.deviceOnline(d) {
		msg = "Added " + what + " · installs when the device is back online"
	}
	h.hxDoneToastEvents(w, r, back, msg, "success", "device-updated")
}

// DeviceAppRemove is POST /devices/{serial}/apps/remove {package}: removes the app and keeps
// it off. The role app can't be removed.
func (h *Handler) DeviceAppRemove(w http.ResponseWriter, r *http.Request) {
	d := h.wantDevice(w, r, "uninstall")
	if d == nil {
		return
	}
	r.ParseForm()
	pkg := strings.TrimSpace(r.FormValue("package"))
	if pkg == "" || len(pkg) > 200 {
		http.Error(w, "package required", http.StatusBadRequest)
		return
	}
	// The MDM clients themselves: removing one would cut the device off from us.
	switch pkg {
	case "com.aioapp.mdm", "aio.app.mdmclient.dpc":
		http.Error(w, "The MDM client can't be removed.", http.StatusForbidden)
		return
	}
	if h.roleAppPackages(r.Context(), d)[pkg] {
		http.Error(w, "The role app can't be removed.", http.StatusForbidden)
		return
	}
	if err := h.db.SetAppWant(r.Context(), d.ID, pkg, db.WantAbsent, h.currentUsername(r)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "app.remove", d.SerialNumber, pkg)
	h.applyWants(r.Context(), []uuid.UUID{d.ID}, true)
	if !h.deviceOnline(d) {
		// Nothing goes out yet: the page's spinner stops and the box says it waits.
		w.Header().Set("X-App-Waits", "1")
	}
	w.Header().Set("HX-Trigger", "device-updated")
	w.WriteHeader(http.StatusOK)
}

// DeviceAppCancel is POST /devices/{serial}/apps/cancel {package}: forgets an add or a
// removal that hasn't happened yet.
func (h *Handler) DeviceAppCancel(w http.ResponseWriter, r *http.Request) {
	d := h.wantDevice(w, r, "install_apk")
	if d == nil {
		return
	}
	r.ParseForm()
	pkg := strings.TrimSpace(r.FormValue("package"))
	back := "/devices/" + d.SerialNumber + "?tab=apps"
	if _, err := h.db.DeleteAppWant(r.Context(), d.ID, pkg); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "app.cancel", d.SerialNumber, pkg)
	label := strings.TrimSpace(r.FormValue("name"))
	if label == "" || len(label) > 80 {
		label = pkg
	}
	h.hxDoneToastEvents(w, r, back, "Canceled "+label, "success", "device-updated")
}

// DeviceAppRetry is POST /devices/{serial}/apps/retry {package}: Try again, now, past the
// back-off.
func (h *Handler) DeviceAppRetry(w http.ResponseWriter, r *http.Request) {
	d := h.wantDevice(w, r, "install_apk")
	if d == nil {
		return
	}
	back := "/devices/" + d.SerialNumber + "?tab=apps"
	if !h.deviceOnline(d) {
		h.hxDoneToast(w, r, back, d.SerialNumber+" is offline · it tries again when it's back", "info")
		return
	}
	h.applyWants(r.Context(), []uuid.UUID{d.ID}, true)
	h.hxDoneToastEvents(w, r, back, "Trying again", "success", "device-updated")
}
