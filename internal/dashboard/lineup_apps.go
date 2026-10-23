package dashboard

import (
	"context"
	"fmt"
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

// Lineup › Apps (A1, picked 23 Oct 2026): a card per product with the app it runs, the
// library build that goes out and how many of its units have the prod package, then the
// chosen product's pane: every unit with what it has now and where its prod app install
// is (RunRoleAppInstalls), coverage, the app and build (the role's App picker lives here
// now, with the auto-install switch) and the versions in service.

type luAppCard struct {
	Role, Label, Base, Build string
	On                       bool
	OnProd, Total, Running   int
	Missing                  int // units known to lack the prod package
	AutoInstall              bool
	Bar                      []luAppSeg
	Note, NoteTone           string
}

type luAppSeg struct {
	Tone string // p | g | v | n
	N    int
}

type luAppRow struct {
	Serial, Venue, Has, Key, Word, Detail string
	Pct                                   int
	Bar                                   bool
	CommandID                             string
	Retry                                 bool
}

type luAppVersion struct {
	Label string
	N     int
}

type luAppsPane struct {
	Card       luAppCard
	Rows       []luAppRow
	Counts     map[string]int
	Pct        int // on prod
	PctAfter   int // once the running installs finish
	Build      *db.App
	Newer      string // a prod version in the field newer than the library build
	NewerN     int
	Versions   []luAppVersion
	StateF     string
	Venues     []string
	VenueF     string
	StateOpts  []luAppOpt
	TotalShown int
}

type luAppOpt struct{ Key, Label string }

type luAppsInputs struct {
	Roles      []string // product classes, in order
	RoleApps   []db.RoleApp
	Lib        map[string]*db.App // base → newest library build
	Devices    []db.Device
	Installed  map[uuid.UUID]map[string]string // family packages per device
	Missing    map[string]map[uuid.UUID]bool   // base → devices that reported apps and lack it
	Installs   map[uuid.UUID]db.InstallState   // latest install of the library build
	Venues     map[uuid.UUID]string
	Selected   string
	StateF     string
	VenueF     string
	SeenWithin time.Duration
	Now        time.Time
}

var luAppStateOrder = map[string]int{"failed": 0, "run": 1, "wait": 2, "off": 3, "hold": 4, "unknown": 5, "ok": 6}

var luAppStateLabels = []luAppOpt{{"", "All"}, {"failed", "Failed"}, {"run", "Installing"}, {"wait", "Next in line"},
	{"off", "Offline"}, {"hold", "Not sent"}, {"unknown", "No app list"}, {"ok", "On prod"}}

// luDeviceState is where one unit is with its prod app.
func luDeviceState(d *db.Device, base string, has map[string]string, missing bool, ins *db.InstallState,
	build *db.App, auto bool, seenWithin time.Duration, now time.Time) (key, word, detail string, pct int, bar bool) {
	if v, ok := has[base]; ok {
		if v == "" {
			v = "version not reported"
		}
		return "ok", "On prod", v, 0, false
	}
	online := now.Sub(d.LastSeenAt) <= seenWithin
	if ins != nil && build != nil {
		switch ins.Status {
		case "", "pending", "delivered":
			if online {
				return "run", "Sent, not picked up yet", "sent " + dp1Ago(now, ins.SentAt), 0, false
			}
		case "downloading", "installing":
			p := 0
			if ins.Progress != nil {
				p = *ins.Progress
			}
			w := "Downloading"
			if ins.Status == "installing" {
				w = "Installing"
			}
			if online {
				return "run", w + " " + strconv.Itoa(p) + "%", build.VersionName + " · started " + dp1Ago(now, ins.SentAt), p, true
			}
		case "installed", "completed":
			return "run", "Installed, confirming", "waiting for its app list", 0, false
		case "failed":
			why := strings.TrimSpace(strings.TrimPrefix(ins.Output, "error:"))
			if why == "" {
				why = "no reason reported"
			}
			retry := ins.SentAt.Add(roleAppInstallRetry)
			when := "retries " + retry.Format("15:04")
			if !auto {
				when = "auto-install is off"
			}
			return "failed", "Failed", why + " · " + when, 0, false
		}
	}
	switch {
	case !missing:
		return "unknown", "No app list", "it hasn't reported its installed apps", 0, false
	case build == nil:
		return "hold", "No build to send", "no prod build in the library", 0, false
	case !auto:
		return "hold", "Not sent", "auto-install is off", 0, false
	case !d.Supports("install_apk"):
		return "hold", "Can't install apps", "its client can't take install commands", 0, false
	case !online:
		return "off", "Offline", "gets it when it is back · seen " + dp1Ago(now, d.LastSeenAt), 0, false
	}
	return "wait", "Next in line", "gets it within a minute", 0, false
}

// familyOf keeps the packages of base's family: the inventory query returns every
// product's packages for a device.
func familyOf(base string, pkgs map[string]string) map[string]string {
	out := map[string]string{}
	for p, v := range pkgs {
		if inFamily(base, p) {
			out[p] = v
		}
	}
	return out
}

// luHasNow is what a unit runs of the family other than the prod package, shortest form.
func luHasNow(base string, has map[string]string) string {
	if v, ok := has[base]; ok {
		return v
	}
	var out []string
	for pkg, v := range has {
		s := strings.TrimPrefix(pkg, base)
		if v != "" {
			s += " " + v
		}
		out = append(out, s)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "—"
	}
	return strings.Join(out, ", ")
}

func buildLineupApps(in luAppsInputs) ([]luAppCard, *luAppsPane) {
	ra := map[string]db.RoleApp{}
	for _, a := range in.RoleApps {
		ra[a.Role] = a
	}
	byRole := map[string][]*db.Device{}
	for i := range in.Devices {
		d := &in.Devices[i]
		if d.Hidden || d.Retired() {
			continue
		}
		byRole[d.Class()] = append(byRole[d.Class()], d)
	}
	var cards []luAppCard
	var pane *luAppsPane
	sel := in.Selected
	// Biggest product first; with none chosen, the biggest one with an app opens.
	roles := append([]string(nil), in.Roles...)
	sort.SliceStable(roles, func(i, j int) bool { return len(byRole[roles[i]]) > len(byRole[roles[j]]) })
	if sel == "" {
		for _, role := range roles {
			if ra[role].BasePackage != "" {
				sel = role
				break
			}
		}
	}
	for _, role := range roles {
		a, ok := ra[role]
		auto := !ok || a.AutoInstall
		base := a.BasePackage
		if base == "" && len(byRole[role]) == 0 {
			continue
		}
		c := luAppCard{Role: role, Label: prod.ClassLabel(role), Base: base, AutoInstall: auto, Total: len(byRole[role])}
		build := in.Lib[base]
		if build != nil {
			c.Build = build.VersionName
		}
		c.On = role == sel
		var rows []luAppRow
		counts := map[string]int{}
		variant := 0
		for _, d := range byRole[role] {
			if base == "" {
				continue
			}
			has := familyOf(base, in.Installed[d.ID])
			var ins *db.InstallState
			if s, ok := in.Installs[d.ID]; ok {
				ins = &s
			}
			key, word, detail, pct, bar := luDeviceState(d, base, has, in.Missing[base][d.ID], ins, build, auto, in.SeenWithin, in.Now)
			counts[key]++
			if key != "ok" && key != "run" && len(has) > 0 {
				variant++
			}
			if c.On {
				r := luAppRow{Serial: d.SerialNumber, Venue: "No venue", Has: luHasNow(base, has), Key: key, Word: word, Detail: detail, Pct: pct, Bar: bar}
				if d.RestaurantID != nil {
					if n := in.Venues[*d.RestaurantID]; n != "" {
						r.Venue = n
					}
				}
				if ins != nil {
					r.CommandID = ins.CommandID.String()
				}
				r.Retry = key == "failed" && build != nil
				rows = append(rows, r)
			}
		}
		c.OnProd, c.Running = counts["ok"], counts["run"]
		c.Missing = c.Total - c.OnProd - counts["unknown"]
		rest := c.Total - c.OnProd - c.Running - variant
		if rest < 0 {
			rest = 0
		}
		c.Bar = []luAppSeg{{"p", c.OnProd}, {"g", c.Running}, {"v", variant}, {"n", rest}}
		switch {
		case base == "":
			c.Note, c.NoteTone = "No app set", "muted"
		case build == nil && c.Missing > 0:
			c.Note, c.NoteTone = fmt.Sprintf("Upload a build to install on %d", c.Missing), "warn"
		case counts["failed"] > 0:
			c.Note, c.NoteTone = fmt.Sprintf("%d failed", counts["failed"]), "bad"
		case c.Running > 0:
			c.Note, c.NoteTone = fmt.Sprintf("%d installing", c.Running), "run"
		case !auto && c.Missing > 0:
			c.Note, c.NoteTone = fmt.Sprintf("Auto-install off · %d without it", c.Missing), "muted"
		case c.Missing > 0:
			c.Note, c.NoteTone = fmt.Sprintf("%d without it", c.Missing), "muted"
		case c.Total > 0:
			c.Note, c.NoteTone = "All on prod", "ok"
		}
		cards = append(cards, c)
		if !c.On {
			continue
		}
		p := &luAppsPane{Card: c, Counts: counts, Build: build, StateF: in.StateF, VenueF: in.VenueF}
		if c.Total > 0 {
			p.Pct = c.OnProd * 100 / c.Total
			p.PctAfter = (c.OnProd + c.Running) * 100 / c.Total
		}
		venues := map[string]bool{}
		for _, r := range rows {
			venues[r.Venue] = true
		}
		for v := range venues {
			p.Venues = append(p.Venues, v)
		}
		sort.Strings(p.Venues)
		for _, o := range luAppStateLabels {
			if o.Key == "" || counts[o.Key] > 0 {
				p.StateOpts = append(p.StateOpts, o)
			}
		}
		sort.SliceStable(rows, func(i, j int) bool {
			if luAppStateOrder[rows[i].Key] != luAppStateOrder[rows[j].Key] {
				return luAppStateOrder[rows[i].Key] < luAppStateOrder[rows[j].Key]
			}
			return rows[i].Serial < rows[j].Serial
		})
		for _, r := range rows {
			if (in.StateF == "" || r.Key == in.StateF) && (in.VenueF == "" || r.Venue == in.VenueF) {
				p.Rows = append(p.Rows, r)
			}
		}
		p.TotalShown = len(rows)
		// Versions in service: prod builds one by one, each variant as one line.
		prodVers := map[string]int{}
		varPkgs := map[string]map[string]bool{}
		varN := map[string]int{}
		for _, d := range byRole[role] {
			for pkg, v := range familyOf(base, in.Installed[d.ID]) {
				if pkg == base {
					prodVers[v]++
					continue
				}
				if varPkgs[pkg] == nil {
					varPkgs[pkg] = map[string]bool{}
				}
				varPkgs[pkg][v] = true
				varN[pkg]++
			}
		}
		for v, n := range prodVers {
			lbl := hostAppName(base) + " " + v
			if v == "" {
				lbl = hostAppName(base) + " (no version)"
			}
			p.Versions = append(p.Versions, luAppVersion{lbl, n})
			if build != nil && v != "" && compareVersionNames(v, build.VersionName) > 0 &&
				(p.Newer == "" || compareVersionNames(v, p.Newer) > 0) {
				p.Newer = v
			}
		}
		for v, n := range prodVers {
			if v == p.Newer && p.Newer != "" {
				p.NewerN = n
			}
		}
		sort.Slice(p.Versions, func(i, j int) bool { return p.Versions[i].N > p.Versions[j].N })
		var vl []luAppVersion
		for pkg, vs := range varPkgs {
			lbl := strings.TrimPrefix(pkg, base)
			if len(vs) > 1 {
				lbl += fmt.Sprintf(" (%d builds)", len(vs))
			}
			vl = append(vl, luAppVersion{lbl, varN[pkg]})
		}
		sort.Slice(vl, func(i, j int) bool { return vl[i].N > vl[j].N })
		p.Versions = append(p.Versions, vl...)
		pane = p
	}
	return cards, pane
}

// lineupApps loads what the Apps tab shows.
func (h *Handler) lineupApps(r *http.Request, library []db.App, devices []db.Device) ([]luAppCard, *luAppsPane) {
	ctx := r.Context()
	roleApps, err := h.db.ListRoleApps(ctx)
	if err != nil {
		log.Printf("[lineup-apps] role apps: %v", err)
	}
	bases := map[string]string{}
	var baseList []string
	for _, a := range roleApps {
		if a.BasePackage != "" {
			bases[a.Role] = a.BasePackage
			baseList = append(baseList, a.BasePackage)
		}
	}
	lib := newestProdBuilds(library, bases)
	installed, _ := h.db.FamilyVersions(ctx, baseList)
	byBase := map[string][]uuid.UUID{}
	var ids []uuid.UUID
	for i := range devices {
		d := &devices[i]
		if b := bases[d.Class()]; b != "" && !d.Hidden && !d.Retired() {
			byBase[b] = append(byBase[b], d.ID)
			ids = append(ids, d.ID)
		}
	}
	missing := map[string]map[uuid.UUID]bool{}
	for b, bids := range byBase {
		missing[b], _ = h.db.DevicesWithoutPackage(ctx, bids, b)
	}
	var urls []string
	for _, a := range lib {
		urls = append(urls, a.ApkURL)
	}
	installs, err := h.db.LatestInstallsOf(ctx, ids, urls, 24*time.Hour)
	if err != nil {
		log.Printf("[lineup-apps] installs: %v", err)
	}
	venues := map[uuid.UUID]string{}
	if rs, err := h.db.ListRestaurants(ctx); err == nil {
		for _, v := range rs {
			venues[v.ID] = v.Name
		}
	}
	var roles []string
	for _, c := range prod.Classes() {
		if c != prod.ClassOther {
			roles = append(roles, c)
		}
	}
	q := r.URL.Query()
	return buildLineupApps(luAppsInputs{
		Roles: roles, RoleApps: roleApps, Lib: lib, Devices: devices, Installed: installed, Missing: missing,
		Installs: installs, Venues: venues, Selected: strings.TrimSpace(q.Get("app")),
		StateF: strings.TrimSpace(q.Get("state")), VenueF: strings.TrimSpace(q.Get("venue")),
		SeenWithin: time.Duration(h.cfg.CheckinInterval()*roleAppSeenFactor) * time.Second, Now: time.Now(),
	})
}

// lineupAppsNoBuild counts the products whose app has units but no prod build in the
// library (the Apps tab's badge).
func (h *Handler) lineupAppsNoBuild(ctx context.Context, library []db.App, devices []db.Device) int {
	bases := h.db.RoleAppBases(ctx)
	lib := newestProdBuilds(library, bases)
	has := map[string]bool{}
	for i := range devices {
		if !devices[i].Hidden && !devices[i].Retired() {
			has[devices[i].Class()] = true
		}
	}
	n := 0
	for role, b := range bases {
		if b != "" && has[role] && lib[b] == nil {
			n++
		}
	}
	return n
}

// LineupAppAuto switches a product's prod app auto-install on or off (admin).
func (h *Handler) LineupAppAuto(w http.ResponseWriter, r *http.Request) {
	role := strings.TrimSpace(r.FormValue("role"))
	if !prod.IsClass(role) {
		http.Error(w, "Unknown product", http.StatusBadRequest)
		return
	}
	on := r.FormValue("on") == "1"
	if err := h.db.SetRoleAppAutoInstall(r.Context(), role, on, h.currentUsername(r)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	state := "off"
	if on {
		state = "on"
	}
	h.audit(r, "release.role_app_auto_install", role, state)
	msg := "Auto-install " + state + " for " + prod.ClassLabel(role) + "."
	q := url.Values{"view": {"apps"}, "app": {role}, "flash": {msg}, "flash_type": {"success"}}
	http.Redirect(w, r, "/lineup?"+q.Encode(), http.StatusSeeOther)
}

// LineupAppRetry sends a unit its product's prod app now, without waiting for the retry
// window (admin).
func (h *Handler) LineupAppRetry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	serial := strings.TrimSpace(r.FormValue("serial"))
	d, err := h.db.GetDevice(ctx, serial)
	if err != nil || d == nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	role := d.Class()
	bases := h.db.RoleAppBases(ctx)
	library, _ := h.db.ListApps(ctx)
	build := newestProdBuilds(library, bases)[bases[role]]
	if build == nil {
		http.Error(w, "No prod build in the library", http.StatusConflict)
		return
	}
	if err := h.queueAppInstall(ctx, d, build, h.currentUsername(r)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.role_app_retry", serial, build.PackageName+" "+build.VersionName)
	q := url.Values{"view": {"apps"}, "app": {role}, "flash": {"Sent " + hostAppName(build.PackageName) + " " + build.VersionName + " to " + serial + "."}, "flash_type": {"success"}}
	http.Redirect(w, r, "/lineup?"+q.Encode(), http.StatusSeeOther)
}
