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
	"sync"
	"time"

	"github.com/google/uuid"

	"mdm/internal/config"
	"mdm/internal/db"
	prod "mdm/internal/product"
)

// Releases — the pipeline (static/releases-pipeline-demos.html, option B, picked 14 Oct):
// roles in a list on the side; for the chosen role an app lane (new builds, one column
// per lab package of the role's app, prod) and a firmware lane per hardware model.
//
// Builds arrive as a link from the software team; "Add build from link" fetches it in the
// background, parses it, stores it like an upload, and it shows up under New builds of
// the role whose app it belongs to. Putting a build on a column is the only decision:
// the column is the package, so a build can only go to its own package's column.

// pipeRole is one row of the side list.
type pipeRole struct {
	Key, Label    string
	Devices       int
	Deployed      int
	Status        string
	Tone          string // ok | warn | bad | muted
	On            bool   // the selected role
	HasApp        bool
}

// pipeClient is one MDM client in the side list.
type pipeClient struct {
	Slot, Label, Status, Tone string
	On                        bool
}

// pipeColumn is one package of the role's app: prod (the base package) or a lab package.
type pipeColumn struct {
	Package    string
	Label      string
	IsProd     bool
	Current    *db.App
	SetBy      string
	SetAt      time.Time
	LabDevices int
	Earlier    []db.ChannelEntry
	// Prod only: where the role's deployed devices stand against Current.
	On, Waiting, Ahead, Missing, Total int
}

// pipeInbox is a build not yet on its column.
type pipeInbox struct {
	App    db.App
	Label  string // its column
	IsProd bool
}

// pipeModel is one hardware model's firmware lane.
type pipeModel struct {
	Key, Label, Maker string
	Mode              string
	Firmware, Std     int
	Channel           *db.FirmwareChannel
	Releases          []db.Release
	Newer             []db.Release
	On, Behind, OtherLine, Total int
	Line              string
}

var (
	fetchMu     sync.Mutex
	fetchStatus = map[string]string{} // link → status line, newest few, for the page
	fetchOrder  []string
)

func noteFetch(link, status string) {
	fetchMu.Lock()
	defer fetchMu.Unlock()
	if _, ok := fetchStatus[link]; !ok {
		fetchOrder = append(fetchOrder, link)
		if len(fetchOrder) > 6 {
			delete(fetchStatus, fetchOrder[0])
			fetchOrder = fetchOrder[1:]
		}
	}
	fetchStatus[link] = status
}

func recentFetches() [][2]string {
	fetchMu.Lock()
	defer fetchMu.Unlock()
	out := make([][2]string, 0, len(fetchOrder))
	for i := len(fetchOrder) - 1; i >= 0; i-- {
		out = append(out, [2]string{fetchOrder[i], fetchStatus[fetchOrder[i]]})
	}
	return out
}

// Pipeline renders Lineup (Products, Setup) and, since 23 Oct 2026 (user: "instead of
// putting clients and apps tabs on lineup, i want separate pages"; S1), the MDM clients
// page (/clients) and the Host apps page (/host-apps), which were its tabs. The old tab
// links redirect.
func (h *Handler) Pipeline(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.URL.Path == "/lineup" {
		q := r.URL.Query()
		v := q.Get("view")
		if v == "apps" || v == "clients" || q.Get("client") != "" {
			q.Del("view")
			to := "/clients"
			if v == "apps" {
				to = "/apps/products"
			}
			if enc := q.Encode(); enc != "" {
				to += "?" + enc
			}
			http.Redirect(w, r, to, http.StatusMovedPermanently)
			return
		}
	}
	st, err := h.loadRoleAppState(ctx)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices, err := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 100000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	library, _ := h.db.ListApps(ctx)
	var bases []string
	for _, b := range st.Apps {
		bases = append(bases, b)
	}
	installed, _ := h.db.FamilyVersions(ctx, bases)

	sel := strings.TrimSpace(r.URL.Query().Get("role"))
	view := strings.TrimSpace(r.URL.Query().Get("view"))
	productions, _ := h.db.ListProductions(ctx, h.connectedSlice())
	h.reloadProductNames(ctx)
	hw := h.lineupHardware(ctx, productions)
	var roles []pipeRole
	for _, c := range prod.Classes() {
		if c == prod.ClassOther {
			continue
		}
		pr := pipeRole{Key: c, Label: prod.ClassLabel(c), HasApp: st.Apps[c] != ""}
		behind := 0
		for i := range devices {
			d := &devices[i]
			if d.Hidden || d.Retired() || d.Class() != c {
				continue
			}
			pr.Devices++
			if d.RestaurantID == nil {
				continue
			}
			pr.Deployed++
			if _, inLab := st.Labs[d.ID]; inLab {
				continue
			}
			if t := st.Channels[c][st.Apps[c]]; t != nil {
				if s := planAppStep(st.Apps[c], installed[d.ID], t); s.Install != nil {
					behind++
				}
			}
		}
		// Roles carry no app any more: the side list says what each role is.
		nm := len(hw.ByRole[c])
		switch {
		case nm == 0:
			pr.Status, pr.Tone = "no models", "muted"
		default:
			pr.Status = fmt.Sprintf("%d device%s · %d model%s", hw.Devices[c], plural(hw.Devices[c]), nm, plural(nm))
			pr.Tone = "ok"
			if hw.Devices[c] == 0 {
				pr.Tone = "muted"
			}
		}
		_ = behind
		roles = append(roles, pr)
	}
	// MDM clients sit in the same side list (merged from the Clients page): our own agents,
	// kept on their hosted build by the auto-update policy.
	views, _ := h.clientSlotSummaries(r, devices)
	clientSel := strings.TrimSpace(r.URL.Query().Get("client"))
	var clients []pipeClient
	for _, slot := range clientSlotOrder {
		v := views[slot]
		pc := pipeClient{Slot: slot, Label: v.Label, On: slot == clientSel}
		switch {
		case !v.Hosted:
			pc.Status, pc.Tone = "no hosted build", "muted"
		case v.Behind > 0:
			pc.Status, pc.Tone = fmt.Sprintf("%d behind %s", v.Behind, v.Build.Version), "warn"
		default:
			pc.Status, pc.Tone = "all on "+v.Build.Version, "ok"
		}
		clients = append(clients, pc)
	}
	// N1 (21 Oct 2026): three tabs. Products (the role chips and pane), MDM clients (both
	// clients and the chosen one's versions; the only place the auto-update switch lives)
	// and Setup (models without a role, production runs). Old links still land:
	// ?client= opens the MDM clients tab, ?role=none and ?view=runs open Setup.
	tab := "products"
	switch {
	case r.URL.Path == "/apps/products":
		tab = "apps"
	case r.URL.Path == "/clients":
		tab = "clients"
	case view == "setup" || view == "runs" || sel == "none":
		tab = "setup"
	}
	behindTotal := 0
	for _, slot := range clientSlotOrder {
		if v := views[slot]; v != nil && v.Hosted {
			behindTotal += v.Behind
		}
	}
	// Everything the tabs and every pane share.
	common := func(m map[string]any) map[string]any {
		m["Title"] = map[string]string{"apps": "Host apps", "clients": "MDM clients"}[tab]
		if m["Title"] == "" {
			m["Title"] = "Lineup"
		}
		m["Tab"] = tab
		if tab == "apps" {
			m["AppsTab"] = "products" // the Apps hub's By product tab (P1)
		}
		m["BehindTotal"] = behindTotal
		m["Roles"] = roles
		m["Clients"] = clients
		m["NoneCount"] = len(hw.None)
		m["AppsNoBuild"] = h.lineupAppsNoBuild(ctx, library, devices)
		m["NoneDevices"] = hw.NoneDevices
		m["RunCount"] = len(productions)
		if _, ok := m["View"]; !ok {
			m["View"] = view
		}
		m["ClientSel"] = clientSel
		m["CanEdit"] = canLab(h.role(r))
		m["Flash"] = r.URL.Query().Get("flash")
		return m
	}
	// Setup: models attached to no role, then the production runs.
	if tab == "setup" {
		h.render(w, r, "pipeline.html", common(map[string]any{"View": "setup", "None": hw.None, "ClassKeys": prod.Classes(),
			"Productions": productions, "Models": h.modelOptions(ctx)}))
		return
	}
	// Apps (A1, 23 Oct 2026): a card per product's app, then the chosen one's units.
	if tab == "apps" {
		cards, pane := h.lineupApps(r, library, devices)
		h.render(w, r, "pipeline.html", common(map[string]any{"View": "apps", "AppCards": cards, "AppsPane": pane, "AppsBento": hostAppsBentoOf(cards, pane),
			"AppChoicesLU": h.lineupApp(ctx, r.URL.Query().Get("app"), devices).Choices}))
		return
	}
	// MDM clients: a card per client, then the chosen one (the first by default) whole.
	// ?partial=1 is the chosen pane alone, for the refreshes it runs while an update is in
	// flight.
	if tab == "clients" {
		if clientSel == "" {
			clientSel = clientSlotOrder[0]
		}
		for i := range clients {
			clients[i].On = clients[i].Slot == clientSel
		}
		cards := lineupClientCards(views, devices, clientSel)
		v, ok := h.clientDetail(r, devices, clientSel)
		if !ok {
			http.Redirect(w, r, "/clients", http.StatusSeeOther)
			return
		}
		data := common(map[string]any{"View": "clients", "ClientCards": cards, "Client": v, "Selected": v, "ServerURL": h.baseURL(r),
			"ClientBento": clientBentoOf(v)})
		if r.URL.Query().Get("partial") == "1" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			h.tmpl.ExecuteTemplate(w, "clients-detail", h.withRole(r, data))
			return
		}
		data["KeysOwn"], data["KeysShared"], _ = h.db.DeviceKeyCounts(ctx)
		h.render(w, r, "pipeline.html", data)
		return
	}
	if sel == "" {
		sel = prod.ClassT7
		for _, pr := range roles {
			if pr.HasApp {
				sel = pr.Key
				break
			}
		}
	}
	var selLabel string
	for i := range roles {
		if roles[i].Key == sel {
			roles[i].On = true
			selLabel = roles[i].Label
		}
	}
	if selLabel == "" {
		http.Redirect(w, r, "/lineup", http.StatusSeeOther)
		return
	}

	base := st.Apps[sel]

	names := map[string]string{}
	for _, a := range library {
		if a.PackageName != "" && names[a.PackageName] == "" {
			names[a.PackageName] = a.Name
		}
	}

	models := h.pipelineModels(ctx, sel, devices, st)

	// The MDM clients this role's devices run, counted over this role only. The policy
	// that keeps devices on the hosted build is per client, so it shows the same on every
	// role that uses the client.
	var roleDevs []db.Device
	for i := range devices {
		if !devices[i].Hidden && !devices[i].Retired() && devices[i].Class() == sel {
			roleDevs = append(roleDevs, devices[i])
		}
	}
	roleViews, _ := h.clientSlotSummaries(r, roleDevs)
	var roleClients []*ClientSlotView
	for _, slot := range clientSlotOrder {
		v := roleViews[slot]
		if v.Devices == 0 {
			continue
		}
		var behind []string
		for i := range roleDevs {
			d := &roleDevs[i]
			real := agentSlotFor(d)
			if clientDisplaySlot(real) != slot {
				continue
			}
			_, code := clientVersionOf(d)
			if real != slot {
				if h.foldedStanding(d, real, code) == "behind" {
					behind = append(behind, d.SerialNumber)
				}
				continue
			}
			if v.Hosted && code > 0 && versionComparable(d, v.Build.Package) && code < v.Build.VersionCode {
				behind = append(behind, d.SerialNumber)
			}
		}
		v.BehindSerials = strings.Join(behind, ",")
		roleClients = append(roleClients, v)
	}

	// L1 (21 Oct 2026): banner, cards, timeline and rail for the role.
	app := h.lineupApp(ctx, sel, devices)
	hasApp := map[uuid.UUID]bool{}
	if app.Base != "" {
		if fv, err := h.db.FamilyVersions(ctx, []string{app.Base}); err == nil {
			for id, pkgs := range fv {
				if len(pkgs) > 0 {
					hasApp[id] = true
				}
			}
		}
	}
	full, deployed := lineupFullOn(devices, sel, models, roleViews, app.Base, hasApp,
		func(d *db.Device) bool { _, in := st.Labs[d.ID]; return in })
	moves, err := h.db.RoleMovesDaily(ctx, sel, 7)
	if err != nil {
		log.Printf("[lineup] moves: %v", err)
	}
	fwHistory, err := h.db.FirmwareProdHistory(ctx)
	if err != nil {
		log.Printf("[lineup] firmware history: %v", err)
	}
	strips := lineupClientStrips(roleClients)
	pane := buildLineupPane(luInputs{
		RoleKey: sel, RoleLabel: selLabel, Models: models, HW: hw.ByRole[sel], App: app,
		Clients: roleClients, Full: full, Deployed: deployed, Moves: moves, History: fwHistory,
		Pic: rolePic(sel, hw.ByRole[sel]), CanEdit: canLab(h.role(r)), Now: time.Now(),
	})

	h.render(w, r, "pipeline.html", common(map[string]any{
		"LU":          pane,
		"ClientStrips": strips,
		"HWModels":    hw.ByRole[sel],
		"RolePic":     rolePic(sel, hw.ByRole[sel]),
		"HWDevices":   hw.Devices[sel],
		"Candidates":  hw.hwCandidates(sel),
		"AppChoices":  appChoicesFrom(library),
		"RoleKey":     sel,
		"RoleLabel":   selLabel,
		"BasePackage": base,
		"BaseName":    names[base],
		"Models":      models,
		"RoleClients": roleClients,
		"App":         app,
		"FwRollout":   h.cfg.RoleRollout(config.RolloutFirmware),
	}))
}

// pipelineModels builds the firmware lane: every hardware model whose devices serve the
// role, with its mode, prod release and the releases published for it.
func (h *Handler) pipelineModels(ctx context.Context, role string, devices []db.Device, st *roleAppState) []pipeModel {
	rows, err := h.db.ProductOverview(ctx)
	if err != nil {
		return nil
	}
	modes, _ := h.db.FirmwareModes(ctx)
	chans, _ := h.db.ListFirmwareChannels(ctx)
	chanBy := map[string]db.FirmwareChannel{}
	for _, c := range chans {
		chanBy[c.Model] = c
	}
	releases, _ := h.db.ListReleases(ctx)
	names, _ := h.db.ProductNames(ctx)
	var out []pipeModel
	for _, row := range rows {
		mrole := row.Role
		if p, ok := prod.Resolve(row.Key); ok && p.Class != "" && mrole == "" {
			mrole = p.Class
		}
		if mrole != role {
			continue
		}
		m := pipeModel{Key: row.Key, Maker: row.Manufacturer, Mode: db.FirmwareModeOf(modes, row.Key), Firmware: row.Firmware, Std: row.DPC + row.Lite}
		m.Label = prod.Label(row.Key)
		if n := names[row.Key]; n != "" {
			m.Label = n
		} else if row.Model != "" && !prod.IsKnown(row.Key) {
			m.Label = strings.TrimSpace(row.Manufacturer + " " + row.Model)
			if strings.HasPrefix(strings.ToLower(row.Model), strings.ToLower(row.Manufacturer)) {
				m.Label = row.Model // "ZC" + "ZC-3568K" reads as a stutter
			}
		}
		if m.Maker == "" && prod.IsKnown(row.Key) {
			m.Maker = "AIO"
		}
		if c, ok := chanBy[row.Key]; ok {
			c := c
			m.Channel = &c
			m.Line = firmwareLine(c.Version)
		}
		for _, rel := range releases {
			if prod.Normalize(rel.Product) != row.Key || rel.Status != "published" || rel.Hidden {
				continue
			}
			m.Releases = append(m.Releases, rel)
		}
		sort.Slice(m.Releases, func(i, j int) bool { return compareVersionNames(m.Releases[i].Version, m.Releases[j].Version) > 0 })
		if m.Channel != nil {
			for _, rel := range m.Releases {
				if firmwareLine(rel.Version) == m.Line && compareVersionNames(rel.Version, m.Channel.Version) > 0 {
					m.Newer = append(m.Newer, rel)
				}
			}
		}
		if len(m.Releases) > 8 {
			m.Releases = m.Releases[:8]
		}
		if m.Channel != nil && m.Channel.Version != "" {
			for i := range devices {
				d := &devices[i]
				if d.Hidden || d.Retired() || d.IsDPC() || d.RestaurantID == nil || modelKey(d) != row.Key {
					continue
				}
				if _, inLab := st.Labs[d.ID]; inLab {
					continue
				}
				m.Total++
				switch {
				case firmwareLine(d.BuildID) != m.Line:
					m.OtherLine++
				case firmwareBehind(d.BuildID, m.Channel.Version):
					m.Behind++
				default:
					m.On++
				}
			}
		}
		out = append(out, m)
	}
	return out
}

func (h *Handler) pipelineBack(w http.ResponseWriter, r *http.Request, role, flash string) {
	q := url.Values{}
	if role != "" {
		q.Set("role", role)
	}
	if flash != "" {
		q.Set("flash", flash)
	}
	http.Redirect(w, r, "/lineup?"+q.Encode(), http.StatusSeeOther)
}

// PipelineRoleApp sets (or clears) the app a role runs.
func (h *Handler) PipelineRoleApp(w http.ResponseWriter, r *http.Request) {
	role := strings.TrimSpace(r.FormValue("role"))
	if !prod.IsClass(role) {
		http.Error(w, "Unknown role", http.StatusBadRequest)
		return
	}
	base := strings.TrimSpace(r.FormValue("base_package"))
	if base != "" && !roleAppAllowed(base) {
		http.Error(w, "A role can only run an aio.app package", http.StatusBadRequest)
		return
	}
	if err := h.db.SetRoleApp(r.Context(), role, base, h.currentUsername(r)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	msg := "App for " + prod.ClassLabel(role) + ": " + hostAppName(base) + "."
	if base == "" {
		base = "(none)"
		msg = "No app for " + prod.ClassLabel(role) + "."
	}
	h.audit(r, "release.role_app", role, base)
	// One message, as a toast: the page's own banner only knows its short codes.
	// The App picker lives on the Host apps page (A1).
	q := url.Values{"app": {role}, "flash": {msg}, "flash_type": {"success"}}
	http.Redirect(w, r, "/apps/products?"+q.Encode(), http.StatusSeeOther)
}

// PipelineFirmware sets a model's prod firmware release.
func (h *Handler) PipelineFirmware(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	role := strings.TrimSpace(r.FormValue("role"))
	model := prod.Normalize(r.FormValue("model"))
	relID, err := strconv.Atoi(strings.TrimSpace(r.FormValue("release_id")))
	if err != nil || model == "" {
		http.Error(w, "Choose a release", http.StatusBadRequest)
		return
	}
	rel, err := h.db.GetRelease(ctx, relID)
	if err != nil || prod.Normalize(rel.Product) != model || rel.Status != "published" {
		http.Error(w, "That release is not a published build for this model.", http.StatusBadRequest)
		return
	}
	modes, _ := h.db.FirmwareModes(ctx)
	if db.FirmwareModeOf(modes, model) != db.FirmwareProd {
		http.Error(w, "Firmware for this model is not set to prod.", http.StatusBadRequest)
		return
	}
	if err := h.db.SetFirmwareChannel(ctx, model, rel.ID, h.currentUsername(r)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.firmware_prod", model, rel.Version)
	h.pipelineBack(w, r, role, "firmware")
}

// PipelineFirmwareMode sets a model's firmware mode. Prod and lab only need the model's
// devices to run the Firmware client.
func (h *Handler) PipelineFirmwareMode(w http.ResponseWriter, r *http.Request) {
	model := prod.Normalize(r.FormValue("model"))
	mode := strings.TrimSpace(r.FormValue("mode"))
	if model == "" || !db.IsFirmwareMode(mode) {
		http.Error(w, "Choose a mode", http.StatusBadRequest)
		return
	}
	if mode != db.FirmwareOff {
		if p, ok := prod.Resolve(model); !ok || p.Kind != prod.KindFirmware || !prod.IsKnown(model) {
			http.Error(w, "Only our own hardware, on the Firmware client, can take firmware from here.", http.StatusBadRequest)
			return
		}
	}
	if err := h.db.SetFirmwareMode(r.Context(), model, mode, h.currentUsername(r)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.firmware_mode", model, mode)
	cls, _ := prod.Resolve(model)
	http.Redirect(w, r, "/lineup?role="+cls.Class+"&flash=mode#model-"+model, http.StatusSeeOther)
}

// PipelineRollout pauses or resumes, and sets the window of, the app or firmware rollout.
func (h *Handler) PipelineRollout(w http.ResponseWriter, r *http.Request) {
	kind := strings.TrimSpace(r.FormValue("kind"))
	if kind != config.RolloutApps && kind != config.RolloutFirmware {
		http.Error(w, "Unknown rollout", http.StatusBadRequest)
		return
	}
	pol := config.ClientAutoUpdate{
		On:          true,
		Paused:      r.FormValue("paused") == "1",
		WindowStart: strings.TrimSpace(r.FormValue("window_start")),
		WindowEnd:   strings.TrimSpace(r.FormValue("window_end")),
		By:          h.currentUsername(r),
		At:          time.Now(),
	}
	if err := h.cfg.SetRoleRollout(kind, pol); err != nil {
		http.Error(w, "Could not save", http.StatusInternalServerError)
		return
	}
	state := "on"
	if pol.Paused {
		state = "paused"
	}
	h.audit(r, "release.rollout", kind, state+" "+pol.WindowStart+"–"+pol.WindowEnd)
	h.pipelineBack(w, r, r.FormValue("role"), "rollout")
}

// maxFetchedAPK caps a fetched build.
const maxFetchedAPK = 1 << 30

// appChoice is one app a role can be set to: a root package of the library.
type appChoice struct{ Package, Name string }

// roleAppAllowed: a role runs one of our own apps, so only aio.app* packages can be set.
func roleAppAllowed(pkg string) bool { return strings.HasPrefix(pkg, "aio.app") }

// appChoicesFrom lists every root package in the library (variants fold into their
// base), named after the base build, sorted by name.
func appChoicesFrom(library []db.App) []appChoice {
	var pkgNames []string
	names := map[string]string{}
	for _, a := range library {
		if a.PackageName != "" {
			pkgNames = append(pkgNames, a.PackageName)
			if names[a.PackageName] == "" {
				names[a.PackageName] = a.Name
			}
		}
	}
	roots := deriveRoots(pkgNames)
	set := map[string]bool{}
	for _, p := range pkgNames {
		if r := roots[p]; r != "" {
			set[r] = true
		} else {
			set[p] = true
		}
	}
	var out []appChoice
	for p := range set {
		if !roleAppAllowed(p) {
			continue
		}
		n := names[p]
		if n == "" {
			n = p
		}
		out = append(out, appChoice{p, n})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}
