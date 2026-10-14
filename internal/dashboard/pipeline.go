package dashboard

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"mdm/internal/apkstore"
	"mdm/internal/config"
	"mdm/internal/db"
	prod "mdm/internal/product"
	"mdm/internal/safehttp"
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

// Pipeline renders the Releases page.
func (h *Handler) Pipeline(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
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
		switch {
		case !pr.HasApp:
			pr.Status, pr.Tone = "no app set", "muted"
		case st.Channels[c][st.Apps[c]] == nil:
			pr.Status, pr.Tone = "no prod build", "muted"
		case behind > 0:
			pr.Status, pr.Tone = fmt.Sprintf("%d behind prod", behind), "warn"
		default:
			pr.Status, pr.Tone = "all on prod", "ok"
		}
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
	if v, ok := views[clientSel]; ok {
		var behind []string
		for i := range devices {
			d := &devices[i]
			if agentSlotFor(d) != clientSel || !v.Hosted {
				continue
			}
			if _, code := clientVersionOf(d); code > 0 && versionComparable(d, v.Build.Package) && code < v.Build.VersionCode {
				behind = append(behind, d.SerialNumber)
			}
		}
		v.BehindSerials = strings.Join(behind, ",")
		h.render(w, r, "pipeline.html", map[string]any{
			"Title":   "Releases",
			"Roles":   roles,
			"Clients": clients,
			"Client":  v,
			"CanEdit": canLab(h.role(r)),
			"Flash":   r.URL.Query().Get("flash"),
		})
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
		http.Redirect(w, r, "/pipeline", http.StatusSeeOther)
		return
	}

	base := st.Apps[sel]
	history, _ := h.db.ChannelHistory(ctx, 4)
	var columns []pipeColumn
	var inbox []pipeInbox
	if base != "" {
		pkgs := map[string]bool{base: true}
		for _, a := range library {
			if inFamily(base, a.PackageName) {
				pkgs[a.PackageName] = true
			}
		}
		for p := range st.Channels[sel] {
			pkgs[p] = true
		}
		var ordered []string
		for p := range pkgs {
			if p != base {
				ordered = append(ordered, p)
			}
		}
		sort.Strings(ordered)
		ordered = append(ordered, base) // prod last, as in the demo
		chans, _ := h.db.ListChannels(ctx)
		meta := map[string]db.Channel{}
		for _, c := range chans {
			if c.Role == sel {
				meta[c.Package] = c
			}
		}
		for _, p := range ordered {
			col := pipeColumn{Package: p, Label: variantLabel(p, base), IsProd: p == base, Current: st.Channels[sel][p]}
			if m, ok := meta[p]; ok {
				col.SetBy, col.SetAt = m.SetBy, m.SetAt
			}
			for _, e := range history {
				if e.Role == sel && e.Package == p && (col.Current == nil || e.AppID == nil || *e.AppID != col.Current.ID) {
					col.Earlier = append(col.Earlier, e)
				}
			}
			if len(col.Earlier) > 3 {
				col.Earlier = col.Earlier[:3]
			}
			for _, s := range st.Labs {
				if s.Package == p {
					col.LabDevices++
				}
			}
			if col.IsProd && col.Current != nil {
				for i := range devices {
					d := &devices[i]
					if d.Hidden || d.Retired() || d.Class() != sel || d.RestaurantID == nil {
						continue
					}
					if _, inLab := st.Labs[d.ID]; inLab {
						continue
					}
					col.Total++
					s := planAppStep(base, installed[d.ID], col.Current)
					_, has := installed[d.ID][base]
					switch {
					case s.Ahead:
						col.Ahead++
					case s.Install == nil:
						col.On++
					case !has:
						col.Missing++
					default:
						col.Waiting++
					}
				}
			}
			columns = append(columns, col)
			// New builds of this package: newer than what its column holds.
			var since time.Time
			if col.Current != nil {
				since = col.Current.CreatedAt
			}
			var builds []db.App
			for _, a := range library {
				if a.PackageName == p && (col.Current == nil || (a.ID != col.Current.ID && a.CreatedAt.After(since))) {
					builds = append(builds, a)
				}
			}
			sort.Slice(builds, func(i, j int) bool { return builds[i].CreatedAt.After(builds[j].CreatedAt) })
			if col.Current == nil && len(builds) > 1 {
				builds = builds[:1] // a column never set: offer its latest build only
			}
			for _, b := range builds {
				inbox = append(inbox, pipeInbox{App: b, Label: col.Label, IsProd: col.IsProd})
			}
		}
		sort.Slice(inbox, func(i, j int) bool { return inbox[i].App.CreatedAt.After(inbox[j].App.CreatedAt) })
	}

	names := map[string]string{}
	for _, a := range library {
		if a.PackageName != "" && names[a.PackageName] == "" {
			names[a.PackageName] = a.Name
		}
	}

	models := h.pipelineModels(ctx, sel, devices, st)

	h.render(w, r, "pipeline.html", map[string]any{
		"Title":       "Releases",
		"Roles":       roles,
		"Clients":     clients,
		"Role":        sel,
		"RoleLabel":   selLabel,
		"BasePackage": base,
		"BaseName":    names[base],
		"Columns":     columns,
		"Inbox":       inbox,
		"Models":      models,
		"CanEdit":     canLab(h.role(r)),
		"AppRollout":  h.cfg.RoleRollout(config.RolloutApps),
		"FwRollout":   h.cfg.RoleRollout(config.RolloutFirmware),
		"Fetches":     recentFetches(),
		"CanFetch":    h.apk != nil,
		"Flash":       r.URL.Query().Get("flash"),
	})
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
	http.Redirect(w, r, "/pipeline?"+q.Encode(), http.StatusSeeOther)
}

// PipelineRoleApp sets (or clears) the app a role runs.
func (h *Handler) PipelineRoleApp(w http.ResponseWriter, r *http.Request) {
	role := strings.TrimSpace(r.FormValue("role"))
	if !prod.IsClass(role) {
		http.Error(w, "Unknown role", http.StatusBadRequest)
		return
	}
	base := strings.TrimSpace(r.FormValue("base_package"))
	if err := h.db.SetRoleApp(r.Context(), role, base, h.currentUsername(r)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.role_app", role, base)
	http.Redirect(w, r, "/products?flash=app#role-"+role, http.StatusSeeOther)
}

// PipelineChannel puts a build on its package's column. On the base package this is
// "promote to prod": every deployed device of the role moves to it in its window.
func (h *Handler) PipelineChannel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	role := strings.TrimSpace(r.FormValue("role"))
	appID, err := uuid.Parse(strings.TrimSpace(r.FormValue("app_id")))
	if err != nil {
		http.Error(w, "Choose a build", http.StatusBadRequest)
		return
	}
	app, err := h.db.GetApp(ctx, appID)
	if err != nil {
		http.Error(w, "Build not found", http.StatusNotFound)
		return
	}
	st, err := h.loadRoleAppState(ctx)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	base := st.Apps[role]
	if base == "" || !inFamily(base, app.PackageName) {
		http.Error(w, "That build is not this role's app.", http.StatusBadRequest)
		return
	}
	if err := h.db.SetChannel(ctx, role, app.PackageName, app.ID, h.currentUsername(r)); err != nil {
		http.Error(w, "Could not save: "+err.Error(), http.StatusBadRequest)
		return
	}
	what := "release.app_channel"
	if app.PackageName == base {
		what = "release.app_prod"
	}
	h.audit(r, what, role, app.PackageName+" "+app.VersionName)
	go h.reconcileKiosk(context.Background()) // a first prod build turns the role lock on
	h.pipelineBack(w, r, role, "channel")
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
	http.Redirect(w, r, "/products?flash=mode#model-"+model, http.StatusSeeOther)
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

// PipelineFetch adds a build from a link. The download runs in the background, because
// a large APK outlasts a request; the result shows under New builds, or as an error line
// on the page.
func (h *Handler) PipelineFetch(w http.ResponseWriter, r *http.Request) {
	if h.apk == nil {
		http.Error(w, "APK storage is not configured on this server.", http.StatusServiceUnavailable)
		return
	}
	link := strings.TrimSpace(r.FormValue("url"))
	if err := safehttp.CheckURL(link, false); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	base := h.baseURL(r)
	by := h.currentUsername(r)
	noteFetch(link, "fetching…")
	h.audit(r, "release.fetch", link, "")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		app, err := h.fetchBuild(ctx, link, base)
		if err != nil {
			log.Printf("[releases] fetch %s (by %s): %v", link, by, err)
			noteFetch(link, "failed: "+err.Error())
			return
		}
		noteFetch(link, "added "+app.PackageName+" "+app.VersionName)
	}()
	h.pipelineBack(w, r, r.FormValue("role"), "fetching")
}

// fetchBuild downloads, parses and stores one APK.
func (h *Handler) fetchBuild(ctx context.Context, link, base string) (*db.App, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	resp, err := safehttp.Client(20 * time.Minute).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the link answered %s", resp.Status)
	}
	f, err := os.CreateTemp("", "fetched-*.apk")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxFetchedAPK+1))
	f.Close()
	if err != nil {
		return nil, err
	}
	if n > maxFetchedAPK {
		return nil, fmt.Errorf("larger than 1 GB")
	}
	meta, err := apkstore.ParseFile(f.Name())
	if err != nil {
		return nil, fmt.Errorf("not an APK: %w", err)
	}
	body, err := os.ReadFile(f.Name())
	if err != nil {
		return nil, err
	}
	key := h.apk.Key(uuid.NewString() + ".apk")
	if err := h.apk.Put(ctx, key, "application/vnd.android.package-archive", body); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	name := meta.Label
	if name == "" {
		name = meta.Package
	}
	app, err := h.db.CreateS3App(ctx, name, meta.Package, meta.VersionName, key, base)
	if err != nil {
		return nil, err
	}
	if meta.IconPNGB64 != "" && len(meta.IconPNGB64) <= 128*1024 {
		_ = h.db.UpsertAppIcon(ctx, meta.Package, meta.IconPNGB64)
	}
	go func() {
		if err := h.apk.EnsureCached(context.Background(), key); err != nil {
			log.Printf("[releases] cache %s: %v", key, err)
		}
	}()
	return app, nil
}

// appChoice is one app a role can be set to: a root package of the library.
type appChoice struct{ Package, Name string }

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
		n := names[p]
		if n == "" {
			n = p
		}
		out = append(out, appChoice{p, n})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}
