package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/apkmeta"
	"mdm/internal/db"
	"mdm/internal/product"
)

// QA builds: follows (option C, picked 30 Oct 2026). QA tablets got their app builds from
// Gmail; now a device follows a channel package (aio.app.kiosk.qa, aio.app.pos.uatv2…)
// and the library's newest upload of it installs there by itself. Set in the device
// page's Install app dialog ("Keep this device on the newest <channel> build"), shown as
// the Apps card's Follows strip and a "follows <channel>" chip on the app's row. A device
// that follows anything is left out of the prod auto-install (role_app_install.go), so a
// QA tablet is not handed prod Nugget. Engineers may upload any channel build; a prod
// build (what the auto-install sends to restaurants) needs an Ops lead or super admin.
// A Lab device that refuses a build for its signing key has the app uninstalled and the
// build installed again (key clash, below); a device at a venue keeps the failure.

// appFollowJob is one install the follow job sends.
type appFollowJob struct {
	Device  *db.Device
	App     *db.App
	Upgrade bool
}

// followPick is what pickFollowInstalls decides from: per device the packages it follows,
// the library's newest build per package, each device's installed versions, the devices
// that reported an app list, those with an install on its way, and the APKs each device
// was sent lately.
type followPick struct {
	follows  map[uuid.UUID][]string
	lib      map[string]*db.App
	have     map[uuid.UUID]map[string]string
	reported map[uuid.UUID]bool
	skip     map[uuid.UUID]bool
	recent   map[uuid.UUID]map[string]bool
	batch    int
}

// followNeeds reports whether a device with cur installed (has = installed at all) should
// get lib: missing, or older than the library's newest. An unknown version on either side
// only counts when the app is missing.
func followNeeds(lib *db.App, cur string, has bool) bool {
	if !has {
		return true
	}
	return lib.VersionName != "" && cur != "" && cur != lib.VersionName && versionNewer(lib.VersionName, cur)
}

// pickFollowInstalls chooses this tick's follow installs: at most one per device (the
// next waits for it), present devices that reported their apps, p.batch in all.
func pickFollowInstalls(devices []db.Device, p followPick, seenWithin time.Duration, now time.Time) []appFollowJob {
	batch := p.batch
	if batch <= 0 {
		batch = roleAppInstallBatch
	}
	var out []appFollowJob
	for i := range devices {
		if len(out) >= batch {
			break
		}
		d := &devices[i]
		pkgs := p.follows[d.ID]
		if len(pkgs) == 0 || d.Hidden || d.Retired() || now.Sub(d.LastSeenAt) > seenWithin || p.skip[d.ID] ||
			!p.reported[d.ID] || !d.Supports("install_apk") {
			continue
		}
		for _, pkg := range pkgs {
			app := p.lib[pkg]
			if app == nil || p.recent[d.ID][app.ApkURL] {
				continue
			}
			cur, has := p.have[d.ID][pkg]
			if followNeeds(app, cur, has) {
				out = append(out, appFollowJob{Device: d, App: app, Upgrade: has})
				break
			}
		}
	}
	return out
}

// RunAppFollows keeps every following device on the newest library build of what it
// follows. Safe to call every minute.
func (h *Handler) RunAppFollows(ctx context.Context) {
	follows, err := h.db.AllFollows(ctx)
	if err != nil {
		log.Printf("[app-follow] follows: %v", err)
		return
	}
	if len(follows) == 0 {
		return
	}
	apps, err := h.db.ListApps(ctx)
	if err != nil {
		log.Printf("[app-follow] library: %v", err)
		return
	}
	want := map[string]bool{}
	var ids []uuid.UUID
	for id, pkgs := range follows {
		ids = append(ids, id)
		for _, p := range pkgs {
			want[p] = true
		}
	}
	lib := newestBuilds(apps, want)
	if len(lib) == 0 {
		return
	}
	devices, err := h.db.ListDevices(ctx, db.DeviceFilter{OnlyIDs: ids}, 0, len(ids)+10, "serial", "asc")
	if err != nil {
		log.Printf("[app-follow] devices: %v", err)
		return
	}
	var pkgs, urls []string
	for p, a := range lib {
		pkgs = append(pkgs, p)
		urls = append(urls, a.ApkURL)
	}
	have, err := h.db.FamilyVersions(ctx, pkgs)
	if err != nil {
		log.Printf("[app-follow] versions: %v", err)
		return
	}
	reported, err := h.db.PackagesReported(ctx, ids)
	if err != nil {
		log.Printf("[app-follow] inventory: %v", err)
		return
	}
	skip, err := h.db.InFlightInstalls(ctx, ids)
	if err != nil {
		log.Printf("[app-follow] in-flight: %v", err)
		return
	}
	recent, err := h.db.RecentInstallsOf(ctx, ids, urls, roleAppInstallRetry, roleAppInstallRetrySoon)
	if err != nil {
		log.Printf("[app-follow] recent: %v", err)
		return
	}
	seenWithin := time.Duration(h.cfg.CheckinInterval()*roleAppSeenFactor) * time.Second
	p := followPick{follows: follows, lib: lib, have: have, reported: reported, skip: skip, recent: recent, batch: h.appRolloutBatch()}
	for _, j := range pickFollowInstalls(devices, p, seenWithin, time.Now()) {
		if err := h.queueAppInstall(ctx, j.Device, j.App, "app-follow"); err != nil {
			log.Printf("[app-follow] %s on %s: %v", j.App.PackageName, j.Device.SerialNumber, err)
			continue
		}
		why := "missing"
		if j.Upgrade {
			why = "upgrade from " + have[j.Device.ID][j.App.PackageName]
		}
		log.Printf("[app-follow] %s %s → %s (%s)", j.App.PackageName, j.App.VersionName, j.Device.SerialNumber, why)
	}
}

// ── Key clash ────────────────────────────────────────────────────────────────

// keyClashAction is what to do with an install refused for its signing key: on a Lab
// device (no venue) whose app is known and that can uninstall, "uninstall" (then install
// again); anywhere else "gave_up", so the failure stands as today.
func keyClashAction(atVenue bool, pkg string, canUninstall bool) string {
	if atVenue || pkg == "" || !canUninstall {
		return "gave_up"
	}
	return "uninstall"
}

// keyClashNext is what a waiting retry does once its uninstall has a result: "install"
// after a clean uninstall, "gave_up" if it failed or nothing came within 2 hours, ""
// to keep waiting.
func keyClashNext(uninstallStatus string, age time.Duration) string {
	switch uninstallStatus {
	case "completed", "installed":
		return "install"
	case "":
		if age > 2*time.Hour {
			return "gave_up"
		}
		return ""
	}
	return "gave_up"
}

// RunKeyClashRetries replaces apps whose new build is signed with another key, on Lab
// devices: uninstall (the app's data goes), then the same build again. Every minute.
func (h *Handler) RunKeyClashRetries(ctx context.Context) {
	clashes, err := h.db.NewKeyClashes(ctx)
	if err != nil {
		log.Printf("[key-clash] list: %v", err)
		return
	}
	for _, k := range clashes {
		d, err := h.db.GetDeviceByID(ctx, k.DeviceID)
		if err != nil || d == nil {
			continue
		}
		act := keyClashAction(k.AtVenue, k.Package, d.Supports("uninstall"))
		if act != "uninstall" {
			_ = h.db.StartKeyClashRetry(ctx, k, "gave_up", nil)
			continue
		}
		id, err := h.queueCommand(ctx, d, "uninstall", "", map[string]string{"package": k.Package}, "key-clash")
		if err != nil {
			log.Printf("[key-clash] uninstall %s on %s: %v", k.Package, d.SerialNumber, err)
			continue
		}
		_ = h.db.StartKeyClashRetry(ctx, k, "uninstall", &id)
		log.Printf("[key-clash] %s on %s refused its signing key: uninstalling, then installing again", k.Package, d.SerialNumber)
	}
	waiting, err := h.db.WaitingKeyClashRetries(ctx)
	if err != nil {
		log.Printf("[key-clash] waiting: %v", err)
		return
	}
	for _, k := range waiting {
		switch keyClashNext(k.UninstallStatus, time.Since(k.CreatedAt)) {
		case "install":
			d, err := h.db.GetDeviceByID(ctx, k.DeviceID)
			if err != nil || d == nil {
				continue
			}
			id, err := h.queueCommand(ctx, d, "install_apk", k.ApkURL, nil, "key-clash")
			if err != nil {
				log.Printf("[key-clash] reinstall %s on %s: %v", k.Package, d.SerialNumber, err)
				continue
			}
			_ = h.db.SetKeyClashStage(ctx, k.CommandID, k.DeviceID, "install", &id)
			log.Printf("[key-clash] %s uninstalled on %s: installing the new build", k.Package, d.SerialNumber)
		case "gave_up":
			_ = h.db.SetKeyClashStage(ctx, k.CommandID, k.DeviceID, "gave_up", nil)
		}
	}
}

// queueCommand queues one command for one device and pushes it, returning its id.
func (h *Handler) queueCommand(ctx context.Context, d *db.Device, typ, apkURL string, payload any, by string) (uuid.UUID, error) {
	raw := json.RawMessage("{}")
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return uuid.Nil, err
		}
		raw = b
	}
	if typ == "install_apk" {
		raw = apkmeta.Augment(ctx, apkURL, raw)
	}
	cmd, err := h.db.CreateCommandBy(ctx, typ, apkURL, raw, "devices", []uuid.UUID{d.ID}, by)
	if err != nil {
		return uuid.Nil, err
	}
	h.pushCommand(ctx, cmd, "devices", []uuid.UUID{d.ID})
	return cmd.ID, nil
}

// ── Who may upload a prod build ──────────────────────────────────────────────

// prodPackages is every package the prod auto-install can send: each role's app and the
// channel a venue chose for it (Venue settings › Host app).
func (h *Handler) prodPackages(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	apps, err := h.db.ListRoleApps(ctx)
	if err != nil {
		return out
	}
	byRole := map[string]string{}
	for _, a := range apps {
		if a.BasePackage != "" {
			out[a.BasePackage] = true
			byRole[a.Role] = a.BasePackage
		}
	}
	if ch, err := h.db.VenueHostChannels(ctx); err == nil {
		for _, m := range ch {
			for role, c := range m {
				if base := byRole[role]; base != "" && c != "" {
					out[hostAppTarget(base, c)] = true
				}
			}
		}
	}
	return out
}

// mayUploadPackage: Ops lead and super admin upload anything; an Engineer only what the
// prod auto-install never sends to restaurants.
func mayUploadPackage(role, pkg string, prod map[string]bool) bool {
	return roleAtLeast(role, "fleet_manager") || !prod[pkg]
}

// prodUploadRefusal is the message an Engineer sees for a prod package.
func prodUploadRefusal(pkg string) string {
	return fmt.Sprintf("%s is a prod build: it goes to restaurants by itself, so an Ops lead or super admin adds it. Channel builds (.qa, .uatv2, .stagev2…) are fine.", pkg)
}

// ── The device page: follows strip, chips, follow / stop ─────────────────────

// followView is one followed package as the device page shows it.
type followView struct {
	Package string
	Name    string // the product ("KIOSK")
	Channel string // "qa", "uatv2", "prod"
	Icon    string
	State   string // ok | run | wait
	Pct     int    // while downloading, else -1
	Have    string // installed version, "" if missing
	Lib     string // the library's newest version, "" if none
	Title   string // the dot's explanation
}

// deviceFollowsView builds the strip and the chips' data for one device.
func (h *Handler) deviceFollowsView(ctx context.Context, d *db.Device) []followView {
	fs, err := h.db.DeviceFollows(ctx, d.ID)
	if err != nil || len(fs) == 0 {
		return nil
	}
	fams, _, _ := h.libraryData(ctx)
	have, _ := h.db.DevicePackageVersions(ctx, d.ID)
	pending, _ := h.db.PendingInstalls(ctx, d.ID)
	out := make([]followView, 0, len(fs))
	for _, f := range fs {
		v := followView{Package: f.Package, Pct: -1}
		for _, fam := range fams {
			for _, vr := range fam.Variants {
				if vr.Package == f.Package {
					v.Name, v.Icon, v.Channel, v.Lib = fam.Name, fam.Icon, vr.Label, vr.Latest.VersionName
				}
			}
		}
		if v.Name == "" {
			v.Name = f.Package
			if i := strings.LastIndexByte(f.Package, '.'); i >= 0 {
				v.Channel = f.Package[i+1:]
			}
		}
		cur, has := have[f.Package]
		if has {
			v.Have = cur
		}
		v.State, v.Title = followState(v, has, pending[f.Package])
		if p, ok := pending[f.Package]; ok && p.Status == "downloading" && p.Percent >= 0 {
			v.Pct = p.Percent
		}
		out = append(out, v)
	}
	return out
}

// followState is the dot: amber while an install is on its way, green when the device has
// the library's newest (or newer), grey while it waits.
func followState(v followView, has bool, p db.InstallProgress) (string, string) {
	switch {
	case p.Status != "":
		return "run", "Installing the newest " + v.Channel + " build"
	case v.Lib == "":
		if has {
			return "ok", "On " + v.Have + " · no build of it in the library yet"
		}
		return "wait", "No build of it in the library yet"
	case has && (v.Have == v.Lib || !versionNewer(v.Lib, v.Have)):
		return "ok", "Up to date · " + v.Have
	case has:
		return "wait", v.Have + " → " + v.Lib + " installs within a minute while the device is online"
	}
	return "wait", v.Lib + " installs within a minute while the device is online"
}

// followSet is package → its view, for the app rows' chips.
func followSet(fs []followView) map[string]followView {
	out := map[string]followView{}
	for _, f := range fs {
		out[f.Package] = f
	}
	return out
}

// DeviceFollow: POST /devices/{serial}/follows {action: follow|unfollow, package}.
func (h *Handler) DeviceFollow(w http.ResponseWriter, r *http.Request) {
	if !h.commandTypeAllowed(h.role(r), "install_apk") {
		http.Error(w, "Your role can't install apps.", http.StatusForbidden)
		return
	}
	d, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil || d == nil {
		http.NotFound(w, r)
		return
	}
	r.ParseForm()
	pkg := strings.TrimSpace(r.FormValue("package"))
	back := "/devices/" + d.SerialNumber + "?tab=apps"
	if pkg == "" || len(pkg) > 200 {
		http.Error(w, "package required", http.StatusBadRequest)
		return
	}
	switch r.FormValue("action") {
	case "follow":
		apps, _ := h.db.ListApps(r.Context())
		if newestBuilds(apps, map[string]bool{pkg: true})[pkg] == nil {
			h.hxDoneToast(w, r, back, pkg+" has no build in the library yet", "error")
			return
		}
		if err := h.db.FollowApp(r.Context(), d.ID, pkg, h.currentUsername(r)); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		h.audit(r, "app.follow", d.SerialNumber, pkg)
		h.hxDoneToastEvents(w, r, back, "Follows "+pkg+": its newest build installs by itself", "success", "device-updated")
	case "unfollow":
		if err := h.db.UnfollowApp(r.Context(), d.ID, pkg); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		h.audit(r, "app.unfollow", d.SerialNumber, pkg)
		h.hxDoneToastEvents(w, r, back, "Stopped following "+pkg+" (the app stays installed)", "success", "device-updated")
	default:
		http.Error(w, "Unknown action", http.StatusBadRequest)
	}
}

// installChannels is the Install dialog's channel data for one family: each channel with
// its package and versions (newest first), for the dialog's second pane.
type installChannel struct {
	Label    string   `json:"label"`
	Package  string   `json:"pkg"`
	Versions []string `json:"vers"`
	URLs     []string `json:"urls"`
}

// installChannelsJSON is a family's channels as the dialog reads them (data-channels).
func installChannelsJSON(f LibFamily) string {
	var out []installChannel
	for _, v := range f.Variants {
		c := installChannel{Label: v.Label, Package: v.Package}
		for _, ver := range v.Versions {
			if ver.App.ApkURL == "" {
				continue
			}
			c.Versions = append(c.Versions, ver.App.VersionName)
			c.URLs = append(c.URLs, ver.App.ApkURL)
		}
		if len(c.URLs) > 0 {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Label == "prod" && out[j].Label != "prod" })
	b, _ := json.Marshal(out)
	return string(b)
}

// installChannelsByFamily is each family's channels for the dialog, by family id.
func installChannelsByFamily(fams []LibFamily) map[string]string {
	out := map[string]string{}
	for _, f := range fams {
		out[f.ID] = installChannelsJSON(f)
	}
	return out
}

// followRow is one role app in the Follow apps list (F2, picked 30 Oct 2026): its channels
// with their newest library builds, and the one this device follows ("" if none).
type followRow struct {
	Name, Product, Icon string
	Channels            []followChan
	Following           string // the followed channel package
	FollowingLabel      string
}

type followChan struct{ Label, Package, Newest string }

// followRows lists the role apps (Lineup's apps, not every library app) for the Follow apps
// dialog, in the library's channel order (prod first).
func (h *Handler) followRows(ctx context.Context, fams []LibFamily, follows []followView) []followRow {
	ras, err := h.db.ListRoleApps(ctx)
	if err != nil {
		return nil
	}
	followed := map[string]bool{}
	for _, f := range follows {
		followed[f.Package] = true
	}
	seen := map[string]bool{}
	var out []followRow
	for _, ra := range ras {
		if ra.BasePackage == "" || seen[ra.BasePackage] {
			continue
		}
		seen[ra.BasePackage] = true
		for _, f := range fams {
			if f.BasePackage != ra.BasePackage && f.Prod().Package != ra.BasePackage {
				continue
			}
			r := followRow{Name: f.Name, Product: product.ClassLabel(ra.Role), Icon: f.Icon}
			for _, v := range f.Variants {
				c := followChan{Label: v.Label, Package: v.Package}
				for _, ver := range v.Versions {
					if ver.App.ApkURL != "" {
						c.Newest = ver.App.VersionName
						break
					}
				}
				if c.Newest == "" {
					continue
				}
				r.Channels = append(r.Channels, c)
				if followed[v.Package] {
					r.Following, r.FollowingLabel = v.Package, v.Label
				}
			}
			sort.SliceStable(r.Channels, func(i, j int) bool { return r.Channels[i].Label == "prod" && r.Channels[j].Label != "prod" })
			if len(r.Channels) > 0 {
				out = append(out, r)
			}
			break
		}
	}
	return out
}
