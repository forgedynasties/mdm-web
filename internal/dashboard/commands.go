package dashboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"mdm/internal/db"
	"mdm/internal/product"

	"github.com/google/uuid"
)

// CommandList renders the Actions page, /jobs (the same data without the send
// console) and the history table's live fragment.
func (h *Handler) CommandList(w http.ResponseWriter, r *http.Request) {
	data, err := h.commandListData(r)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Live status: the Actions page's history table re-fetches just this fragment on a
	// command-update SSE event (and a slow poll), morphing it in place so statuses move
	// buckets without a reload or a flash.
	if r.URL.Query().Get("partial") == "histlive" {
		h.renderCachedHTML(w, r, "cmd-timeline-inner", h.withRole(r, data))
		return
	}
	// /jobs is the same data without the send console: in flight, needs attention, and
	// what has run. The console is being dismantled (its verbs are moving to the thing
	// they act on — the app for installs, the device selection for diagnostics), and this
	// is the half that has to survive it, because nothing else shows work in flight.
	if r.URL.Path == "/jobs" {
		data["JobsView"] = true
		h.render(w, r, "jobs.html", data)
		return
	}
	h.render(w, r, "commands.html", data)
}

// commandListInputs is everything the Actions page reads from the database up front.
type commandListInputs struct {
	cmds          []db.Command
	groups        []db.Group
	apps          []db.App
	fleetPackages []db.FleetPackage
	shellRecent   []string
	shellPopular  []string
	productions   []db.Production
	builds        []string
	summaries     map[uuid.UUID]db.CommandDeliverySummary
}

// loadCommandListInputs runs the Actions page's independent reads concurrently instead
// of one round trip after another — same pattern as Overview/DeviceDetail. The first
// four are hard errors; the rest tolerate errors silently.
func (h *Handler) loadCommandListInputs(r *http.Request) (commandListInputs, error) {
	ctx := r.Context()
	var in commandListInputs
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	run(func() {
		c, err := h.db.ListCommandsSince(ctx, actionsWindowDays)
		if err != nil {
			fail(err)
			return
		}
		c = filterShellCommands(h.role(r), c) // operators never see shell history
		in.cmds = h.filterHiddenCommands(r, h.filterAdminCommands(r, c))
	})
	run(func() {
		g, err := h.db.ListGroups(ctx)
		if err != nil {
			fail(err)
			return
		}
		in.groups = g
	})
	run(func() {
		a, err := h.db.ListApps(ctx)
		if err != nil {
			fail(err)
			return
		}
		in.apps = a
	})
	run(func() {
		// Distinct packages seen across all devices — populates the Uninstall dropdown.
		fp, err := h.db.SearchFleetPackages(ctx, "")
		if err != nil {
			fail(err)
			return
		}
		in.fleetPackages = fp
	})
	run(func() { in.shellRecent, in.shellPopular, _ = h.db.ShellCommandSuggestions(ctx, 6) })
	run(func() { in.productions, _ = h.db.ListProductions(ctx, h.connectedSlice()) })
	run(func() { in.builds, _ = h.db.GetDistinctBuildIDs(ctx) })
	run(func() {
		in.summaries, _ = h.db.GetCommandDeliverySummaries(ctx, h.cfg.CommandExpiry(), actionsWindowDays)
	})
	wg.Wait()
	if firstErr != nil {
		return in, firstErr
	}
	h.resolveCommandActors(ctx, in.cmds)
	return in, nil
}

// commandListData is the load step of CommandList: the template data for the
// Actions page, /jobs and the histlive fragment.
func (h *Handler) commandListData(r *http.Request) (map[string]any, error) {
	in, err := h.loadCommandListInputs(r)
	if err != nil {
		return nil, err
	}
	cmds, summaries, groups, apps := in.cmds, in.summaries, in.groups, in.apps
	role := h.role(r)

	prefill := h.commandClonePrefill(r)

	groupNames := make(map[uuid.UUID]string, len(groups))
	for _, g := range groups {
		groupNames[g.ID] = g.Name
	}
	recipes := h.commandRecipes(r, cmds, groupNames)

	// ── Triage buckets (Option D): classify every command by its delivery
	// summary. Needs-attention (has failures) and In-progress (still in flight)
	// are always shown in full; Completed is paginated. Expiry is already applied
	// in the summary, so a stuck "pending" becomes failed → surfaces in attention
	// rather than sitting in-progress forever.
	// Failures older than this stop being "needs attention" — they drop into
	// Completed (still rendered as failed, just no longer flagged for triage).
	dismissed, _ := h.db.ListDismissedCommandIDs(r.Context())
	var batches map[uuid.UUID][]db.Command
	cmds, summaries, batches = collapseBatches(cmds, summaries)
	var clusters map[uuid.UUID][]db.Command
	cmds, summaries, clusters = clusterSystemReboots(cmds, summaries)
	attn, prog, doneAll := classifyCommands(cmds, summaries, dismissed)

	// The Actions page shows a capped triage preview of each bucket; "Show all →"
	// links jump to the standalone /commands/history page (with pagination).
	const attnLimit, progLimit, doneLimit = 3, 8, 6
	attnShown, progShown, doneShown := capCmds(attn, attnLimit), capCmds(prog, progLimit), capCmds(doneAll, doneLimit)

	// Serial lookups only for the commands actually rendered this request, in one
	// batched query (avoids a per-command N+1).
	var serialIDs []uuid.UUID
	for _, set := range [][]db.Command{attnShown, progShown, doneShown} {
		for _, c := range set {
			if c.TargetType == "devices" {
				serialIDs = append(serialIDs, c.ID)
			}
			for _, m := range batches[c.ID] { // a folded batch names every member's devices
				if m.ID != c.ID && m.TargetType == "devices" {
					serialIDs = append(serialIDs, m.ID)
				}
			}
		}
	}
	targetSerials, _ := h.db.GetCommandTargetSerialsBatch(r.Context(), serialIDs)

	// Collections for the palette's target dropdown (restaurants + releases with counts).
	scopeRestaurants, _ := h.db.GetRestaurantHealth(r.Context(), h.connectedSlice(), 7)
	if acc := h.access(r); acc.hidesDevices() {
		seenR, _ := acc.visibleCollections()
		scopeRestaurants = scopeHealth(scopeRestaurants, seenR)
		groups = h.visibleGroupList(r, groups)
	}
	scopeReleases, _ := h.db.ListPublishedReleasesForRail(r.Context())
	scopeReleases = visibleRail(h.role(r), scopeReleases)

	// Diagnostics catalog for the palette (admin/dev/operator run them; the action
	// is hidden for viewers and when the catalog is empty).
	var actionQueries []db.DeviceQuery
	if roleCanOperate(role) {
		actionQueries, _ = h.db.ListEnabledDeviceQueries(r.Context())
	}

	// ── Palette datasets + "Recent sends" — full-page renders only (the histlive
	// partial re-runs this handler on every feed tick and doesn't need them). ──
	var paletteJSON []byte
	var recents []recentSendView
	if r.URL.Query().Get("partial") != "histlive" {
		palActions, palAllowed := h.paletteActions(role, actionQueries)
		paletteJSON = h.commandPaletteJSON(r, palActions, in, groups, scopeRestaurants, actionQueries)
		recents = h.recentSends(r, cmds, apps, batches, clusters, palAllowed, groupNames)
	}

	return map[string]any{
		"Title":            "Actions",
		"DeviceQueries":    actionQueries,
		"Commands":         cmds,
		"Attention":        attnShown,
		"InProgress":       progShown,
		"Completed":        doneShown,
		"AttnCount":        len(attn),
		"ProgCount":        len(prog),
		"DoneCount":        len(doneAll),
		"Groups":           groups,
		"ScopeRestaurants": scopeRestaurants,
		"ScopeReleases":    scopeReleases,
		"Productions":      in.productions,
		"Builds":           in.builds,
		"Apps":             apps,
		"AppNames":         apkURLToName(apps),
		"FleetPackages":    in.fleetPackages,
		"Recipes":          recipes,
		"Summaries":        summaries,
		"Batches":          batches,
		"Clusters":         clusters,
		"AppIcons":         apkURLToIcon(apps),
		"PkgIcons":         pkgToIcon(apps),
		"TargetSerials":    targetSerials,
		"ShellRecent":      in.shellRecent,
		"ShellPopular":     in.shellPopular,
		"AIEnabled":        h.cfg.AIEnabled(),
		"Prefill":          prefill,
		"PaletteJSON":      template.JS(paletteJSON),
		"RecentSends":      recents,
		"RequireReason":    h.cfg.RequireReason(),
	}, nil
}

// commandClonePrefill: ?clone=<id> prefills the builder from an existing command, so
// the detail page's "Duplicate & edit" opens the builder ready to tweak and re-send.
func (h *Handler) commandClonePrefill(r *http.Request) template.JS {
	prefill := template.JS("null")
	cid := r.URL.Query().Get("clone")
	if cid == "" {
		return prefill
	}
	id, perr := uuid.Parse(cid)
	if perr != nil {
		return prefill
	}
	// Only a command the user can open, and shell text only for shell roles.
	c, gerr := h.db.GetCommand(r.Context(), id)
	if gerr != nil || !h.commandVisible(r, *c) || (c.Type == "shell" && hideShellForRole(h.role(r))) {
		return prefill
	}
	pf := map[string]any{"type": c.Type, "target": c.TargetType}
	if c.ApkURL != "" {
		pf["apk_url"] = c.ApkURL
	}
	if c.Type == "shell" {
		var p struct {
			Cmd string `json:"cmd"`
		}
		_ = json.Unmarshal(c.Payload, &p)
		pf["shell_cmd"] = p.Cmd
	}
	if c.TargetType == "devices" {
		if ser, e := h.db.GetCommandTargetSerials(r.Context(), id); e == nil {
			pf["serials"] = h.keepVisibleSerials(r, ser)
		}
	} else if c.TargetType == "groups" {
		if gids, e := h.db.GetCommandTargetIDs(r.Context(), id); e == nil {
			gs := make([]string, len(gids))
			for i, g := range gids {
				gs[i] = g.String()
			}
			pf["groups"] = gs
		}
	}
	if b, e := json.Marshal(pf); e == nil {
		prefill = template.JS(b)
	}
	return prefill
}

// recipeView carries a recipe plus the JSON the builder's prefill path consumes
// and a human target summary for the card.
type recipeView struct {
	ID        uuid.UUID
	Name      string
	Type      string
	Label     string
	Summary   string
	Prefill   template.JS
	Deletable bool
}

// recipePrefillJSON is the builder prefill object for a recipe or a past command.
func recipePrefillJSON(cmdType, apkURL string, payload json.RawMessage, targetType string, serials []string, groupIDs []uuid.UUID) template.JS {
	pf := map[string]any{"type": cmdType, "target": targetType}
	if apkURL != "" {
		pf["apk_url"] = apkURL
	}
	switch cmdType {
	case "shell":
		var p struct {
			Cmd string `json:"cmd"`
		}
		_ = json.Unmarshal(payload, &p)
		pf["shell_cmd"] = p.Cmd
	case "uninstall":
		var p struct {
			Package string `json:"package"`
		}
		_ = json.Unmarshal(payload, &p)
		pf["package"] = p.Package
	case "query":
		var p struct {
			QueryID int `json:"query_id"`
		}
		_ = json.Unmarshal(payload, &p)
		if p.QueryID > 0 {
			pf["query_id"] = p.QueryID
		}
	}
	if len(serials) > 0 {
		pf["serials"] = serials
	}
	if len(groupIDs) > 0 {
		gs := make([]string, len(groupIDs))
		for i, g := range groupIDs {
			gs[i] = g.String()
		}
		pf["groups"] = gs
	}
	b, _ := json.Marshal(pf)
	return template.JS(b)
}

// commandTargetSummary names a command's target for a card: a serial, "N devices",
// a group name, "N groups" or "All devices".
func commandTargetSummary(groupNames map[uuid.UUID]string, targetType string, serials []string, groupIDs []uuid.UUID) string {
	switch targetType {
	case "devices":
		if len(serials) == 1 {
			return serials[0]
		}
		return fmt.Sprintf("%d devices", len(serials))
	case "groups":
		if len(groupIDs) == 1 {
			if n, ok := groupNames[groupIDs[0]]; ok {
				return n
			}
		}
		return fmt.Sprintf("%d groups", len(groupIDs))
	default:
		return "All devices"
	}
}

// commandRecipes is the recipes strip: saved presets plus a "Re-run last" derived
// from history.
func (h *Handler) commandRecipes(r *http.Request, cmds []db.Command, groupNames map[uuid.UUID]string) []recipeView {
	var recipes []recipeView
	// "Re-run last" from the most recent command the builder can actually replay
	// (OTA isn't a builder action, so skip it).
	var last *db.Command
	for i := range cmds {
		if cmds[i].Type != "ota" {
			last = &cmds[i]
			break
		}
	}
	if last != nil {
		var serials []string
		var gids []uuid.UUID
		if last.TargetType == "devices" {
			serials, _ = h.db.GetCommandTargetSerials(r.Context(), last.ID)
		} else if last.TargetType == "groups" {
			gids, _ = h.db.GetCommandTargetIDs(r.Context(), last.ID)
		}
		recipes = append(recipes, recipeView{
			Name:    "Re-run last",
			Type:    last.Type,
			Label:   cmdTypeLabel(last.Type),
			Summary: cmdTypeLabel(last.Type) + " → " + commandTargetSummary(groupNames, last.TargetType, serials, gids),
			Prefill: recipePrefillJSON(last.Type, last.ApkURL, last.Payload, last.TargetType, serials, gids),
		})
	}
	saved, _ := h.db.ListRecipes(r.Context())
	canOp := roleCanOperate(h.role(r))
	for _, rec := range saved {
		// For roles that can act, hide recipes they aren't allowed to issue (so a
		// click never leads to a rejected send). Viewers see every recipe — the
		// strip is read-only for them, so it's just a catalogue of what's set up.
		if canOp && !h.commandTypeAllowed(h.role(r), rec.Type) {
			continue
		}
		recipes = append(recipes, recipeView{
			ID:        rec.ID,
			Name:      rec.Name,
			Type:      rec.Type,
			Label:     cmdTypeLabel(rec.Type),
			Summary:   cmdTypeLabel(rec.Type) + " → " + commandTargetSummary(groupNames, rec.TargetType, rec.TargetSerials, rec.TargetGroups),
			Prefill:   recipePrefillJSON(rec.Type, rec.ApkURL, rec.Payload, rec.TargetType, rec.TargetSerials, rec.TargetGroups),
			Deletable: h.role(r) == "admin",
		})
	}
	return recipes
}

// palAction describes one entry of the ⌘K composer's action dropdown. The list
// is filtered by the SAME commandRoles gate the send path enforces (plus the
// set_kiosk special case, which isn't a queued command), so the palette never
// offers an action the current role can't actually send. Cap tags mark client
// capability requirements: "system app" needs the privileged AOSP build,
// "DPC only" needs the Device-Owner agent.
type palAction struct {
	Type        string `json:"t"`
	Name        string `json:"n"`
	Desc        string `json:"d"`
	Payload     string `json:"p"`             // apps|pkgs|shell|query|kiosk|splash|none
	Cap         string `json:"cap,omitempty"` // "system app" | "DPC only"
	Destructive bool   `json:"destr,omitempty"`
}

// paletteActions is the composer's action list for a role, and the set of types in it.
func (h *Handler) paletteActions(role string, actionQueries []db.DeviceQuery) ([]palAction, map[string]bool) {
	// logcat and ota are deliberately absent: neither is a builder action here —
	// logcat fans out via logcat_requests (device pages), OTA via the releases
	// pages — so a POST /commands of either would queue a dead command. The host-app
	// actions (update, reload, restart, clear cache, update check) are gone too: they
	// are single-device nudges for the menu board and live on the device page.
	allActions := []palAction{
		{Type: "install_apk", Name: "Install app", Desc: "push apps from the library, silently", Payload: "apps"},
		{Type: "uninstall", Name: "Uninstall", Desc: "remove packages from the target", Payload: "pkgs"},
		{Type: "screenshot", Name: "Screenshot", Desc: "capture the live screen", Payload: "none"},
		{Type: "query", Name: "Device query", Desc: "vetted read-only diagnostic", Payload: "query"},
		{Type: "collect_logs", Name: "Collect logs", Desc: "logcat, dumpsys, getprop, OTA log — a file per device", Payload: "none", Cap: "system app"},
		{Type: "shell", Name: "Shell", Desc: "raw shell command", Payload: "shell", Cap: "system app"},
		{Type: "reboot", Name: "Reboot", Desc: "restart devices — confirm to send", Payload: "none", Destructive: true},
		{Type: "set_kiosk", Name: "Locked mode", Desc: "lock to one app, or unlock", Payload: "kiosk"},
		{Type: "update_splash", Name: "Boot splash", Desc: "replace the boot logo from an image URL", Payload: "splash", Cap: "system app", Destructive: true},
		{Type: "wipe", Name: "Factory wipe", Desc: "erase completely — typed confirm", Payload: "none", Cap: "DPC; firmware client 1.10.0+", Destructive: true},
		{Type: "unenroll", Name: "Unenroll", Desc: "stop managing it and keep its data — the device leaves the fleet", Payload: "none", Cap: "DPC only", Destructive: true},
	}
	var palActions []palAction
	palAllowed := map[string]bool{}
	for _, a := range allActions {
		switch a.Type {
		case "set_kiosk":
			if !roleCanOperate(role) {
				continue
			}
		case "query":
			if !h.commandTypeAllowed(role, a.Type) || len(actionQueries) == 0 {
				continue
			}
		case "shell":
			if !h.commandTypeAllowed(role, a.Type) || !h.cfg.ShellEnabled() {
				continue
			}
		default:
			if !h.commandTypeAllowed(role, a.Type) {
				continue
			}
		}
		palActions = append(palActions, a)
		palAllowed[a.Type] = true
	}
	return palActions, palAllowed
}

// commandPaletteJSON is the ⌘K composer's datasets: actions, apps, targets,
// diagnostics, packages and shell suggestions.
func (h *Handler) commandPaletteJSON(r *http.Request, palActions []palAction, in commandListInputs, groups []db.Group, scopeRestaurants []db.GroupHealth, actionQueries []db.DeviceQuery) []byte {
	type palApp struct {
		URL     string `json:"url"`
		Name    string `json:"name"`
		Ver     string `json:"ver,omitempty"`
		Pkg     string `json:"pkg,omitempty"`
		Icon    string `json:"icon,omitempty"`
		Family  string `json:"family,omitempty"`
		Variant string `json:"variant,omitempty"`
		Latest  bool   `json:"latest,omitempty"`
	}
	// Ordered by family, prod first, newest first, so the picker reads as a catalogue.
	palFams, _, _ := h.libraryData(r.Context())
	palApps := make([]palApp, 0, len(in.apps))
	for _, f := range palFams {
		for _, v := range f.Variants {
			for _, ver := range v.Versions {
				a := ver.App
				palApps = append(palApps, palApp{URL: a.ApkURL, Name: a.Name, Ver: a.VersionName, Pkg: a.PackageName, Icon: a.Icon, Family: f.Name, Variant: v.Label, Latest: ver.Latest})
			}
		}
	}
	type palCollection struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	palGroups := make([]palCollection, 0, len(groups))
	for _, g := range groups {
		palGroups = append(palGroups, palCollection{ID: g.ID.String(), Name: g.Name, Count: g.DeviceCount})
	}
	palRests := make([]palCollection, 0, len(scopeRestaurants))
	for _, g := range scopeRestaurants {
		palRests = append(palRests, palCollection{ID: g.GroupID.String(), Name: g.Name, Count: g.DeviceCount})
	}
	type palQuery struct {
		ID    int    `json:"id"`
		Label string `json:"label"`
		Cat   string `json:"cat,omitempty"`
	}
	palQueries := make([]palQuery, 0, len(actionQueries))
	for _, q := range actionQueries {
		palQueries = append(palQueries, palQuery{ID: q.ID, Label: q.Label, Cat: q.Category})
	}
	type palPkg struct {
		Pkg   string `json:"pkg"`
		Name  string `json:"name"`
		Count int    `json:"count"`
		Icon  string `json:"icon,omitempty"`
	}
	palPkgs := make([]palPkg, 0, len(in.fleetPackages))
	for _, p := range in.fleetPackages {
		nm := p.AppName
		if nm == "" {
			nm = p.PackageName
		}
		palPkgs = append(palPkgs, palPkg{Pkg: p.PackageName, Name: nm, Count: p.DeviceCount, Icon: p.Icon})
	}
	b, _ := json.Marshal(map[string]any{
		"actions":       palActions,
		"apps":          palApps,
		"groups":        palGroups,
		"restaurants":   palRests,
		"queries":       palQueries,
		"packages":      palPkgs,
		"shellRecent":   in.shellRecent,
		"shellPopular":  in.shellPopular,
		"requireReason": h.cfg.RequireReason(),
	})
	return b
}

// recentSendView is one "Recent sends" row: a past send the palette reloads in one click.
type recentSendView struct {
	ID      uuid.UUID
	Type    string
	Label   string
	Detail  string
	Target  string
	When    time.Time
	By      string
	Prefill template.JS
	Count   int // "Most frequent" pane: how many times this exact send was made
}

// recentSendPrefill is the palette prefill for a past send. Batch installs and
// uninstalls carry every URL/package of the batch.
func recentSendPrefill(c db.Command, serials []string, batches map[uuid.UUID][]db.Command) template.JS {
	pf := map[string]any{"type": c.Type, "target": c.TargetType}
	if members, ok := batches[c.ID]; ok && len(members) > 1 {
		var urls, pkgs []string
		for _, m := range members {
			if m.ApkURL != "" {
				urls = append(urls, m.ApkURL)
			}
			if c.Type == "uninstall" {
				var p struct {
					Package string `json:"package"`
				}
				if json.Unmarshal(m.Payload, &p) == nil && p.Package != "" {
					pkgs = append(pkgs, p.Package)
				}
			}
		}
		if len(urls) > 0 {
			pf["apk_urls"] = urls
		}
		if len(pkgs) > 0 {
			pf["packages"] = pkgs
		}
	} else if c.ApkURL != "" {
		pf["apk_url"] = c.ApkURL
	}
	switch c.Type {
	case "shell":
		var p struct {
			Cmd string `json:"cmd"`
		}
		_ = json.Unmarshal(c.Payload, &p)
		pf["shell_cmd"] = p.Cmd
	case "uninstall":
		if _, ok := pf["packages"]; !ok {
			var p struct {
				Package string `json:"package"`
			}
			_ = json.Unmarshal(c.Payload, &p)
			pf["package"] = p.Package
		}
	case "query":
		var p struct {
			QueryID int `json:"query_id"`
		}
		_ = json.Unmarshal(c.Payload, &p)
		if p.QueryID > 0 {
			pf["query_id"] = p.QueryID
		}
	}
	if len(serials) > 0 {
		pf["serials"] = serials
	}
	b, _ := json.Marshal(pf)
	return template.JS(b)
}

// recentSendDetail is the one-line "what" of a past send: app, package, shell text or query.
func recentSendDetail(c db.Command, batches map[uuid.UUID][]db.Command, appNamesByURL map[string]string) string {
	if members, ok := batches[c.ID]; ok && len(members) > 1 {
		return fmt.Sprintf("%d apps", len(members))
	}
	switch c.Type {
	case "install_apk":
		if n := appNamesByURL[c.ApkURL]; n != "" {
			return n
		}
		if i := strings.LastIndex(c.ApkURL, "/"); i >= 0 && i < len(c.ApkURL)-1 {
			return c.ApkURL[i+1:]
		}
	case "uninstall":
		var p struct {
			Package string `json:"package"`
		}
		if json.Unmarshal(c.Payload, &p) == nil {
			return p.Package
		}
	case "shell":
		var p struct {
			Cmd string `json:"cmd"`
		}
		if json.Unmarshal(c.Payload, &p) == nil {
			return p.Cmd
		}
	case "query":
		var p struct {
			Query string `json:"query"`
		}
		if json.Unmarshal(c.Payload, &p) == nil {
			return p.Query
		}
	}
	return ""
}

// recentSends is the last few DISTINCT sends (this user's own preferred), each
// carrying a prefill object the palette reloads in one click. Batch
// installs/uninstalls collapse to one entry carrying every URL/package.
func (h *Handler) recentSends(r *http.Request, cmds []db.Command, apps []db.App, batches, clusters map[uuid.UUID][]db.Command, palAllowed map[string]bool, groupNames map[uuid.UUID]string) []recentSendView {
	appNamesByURL := apkURLToName(apps)
	meNames := map[string]bool{}
	if u := h.currentUsername(r); u != "" {
		meNames[u] = true
	}
	if dn := h.currentDisplayName(r); dn != "" {
		meNames[dn] = true
	}
	const recentMax = 6
	// Only operator sends count as presets: admin accounts drive maintenance and
	// tests, and those must not shape what the console suggests to operators.
	resendable := func(c db.Command) bool {
		if !palAllowed[c.Type] || c.CreatedBy == "" || userIsAdminFn(c.CreatedBy) {
			return false
		}
		_, isCluster := clusters[c.ID]
		return !isCluster // system/OTA reboots aren't resendable presets
	}
	// viewOf builds the row once per command; the target lookups are the cost, so
	// the signature (what makes two sends "the same") comes with it.
	viewOf := func(c db.Command) (recentSendView, string) {
		serials, _ := h.db.GetCommandTargetSerials(r.Context(), c.ID)
		var gids []uuid.UUID
		if c.TargetType == "groups" {
			gids, _ = h.db.GetCommandTargetIDs(r.Context(), c.ID)
		}
		detail := recentSendDetail(c, batches, appNamesByURL)
		target := commandTargetSummary(groupNames, c.TargetType, serials, gids)
		return recentSendView{
			ID:      c.ID,
			Type:    c.Type,
			Label:   cmdTypeLabel(c.Type),
			Detail:  detail,
			Target:  target,
			When:    c.CreatedAt,
			By:      c.CreatedBy,
			Prefill: recentSendPrefill(c, serials, batches),
		}, c.Type + "|" + c.ApkURL + "|" + detail + "|" + target
	}
	var recents []recentSendView
	seenSig := map[string]bool{}
	addRecent := func(c db.Command) {
		if len(recents) >= recentMax || !resendable(c) {
			return
		}
		v, sig := viewOf(c)
		if seenSig[sig] {
			return
		}
		seenSig[sig] = true
		recents = append(recents, v)
	}
	for pass := 0; pass < 2 && len(recents) < recentMax; pass++ {
		scanned := 0
		for i := range cmds {
			if len(recents) >= recentMax || scanned > 60 {
				break
			}
			own := cmds[i].CreatedBy != "" && meNames[cmds[i].CreatedBy]
			if (pass == 0) != own {
				continue
			}
			scanned++
			addRecent(cmds[i])
		}
	}
	return recents
}

// capCmds returns the first n commands (or all if fewer).
func capCmds(cmds []db.Command, n int) []db.Command {
	if len(cmds) > n {
		return cmds[:n]
	}
	return cmds
}

// CommandHistory is the standalone, paginated history view reached from the
// Actions page's "Show all →" links. It shows one status bucket (or all) as a
// dense table with numbered pagination — the deep-browse counterpart to the
// capped triage preview on /commands.
func (h *Handler) CommandHistory(w http.ResponseWriter, r *http.Request) {
	cmds, err := h.db.ListCommands(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	cmds = filterShellCommands(h.role(r), cmds) // operators never see shell history
	cmds = h.filterHiddenCommands(r, h.filterAdminCommands(r, cmds))
	h.resolveCommandActors(r.Context(), cmds)
	summaries, _ := h.db.GetCommandDeliverySummaries(r.Context(), h.cfg.CommandExpiry(), 0) // full history
	dismissed, _ := h.db.ListDismissedCommandIDs(r.Context())
	apps, _ := h.db.ListApps(r.Context())
	var batches map[uuid.UUID][]db.Command
	cmds, summaries, batches = collapseBatches(cmds, summaries)
	var clusters map[uuid.UUID][]db.Command
	cmds, summaries, clusters = clusterSystemReboots(cmds, summaries)
	attn, prog, doneAll := classifyCommands(cmds, summaries, dismissed)

	status := r.URL.Query().Get("status")
	var rows []db.Command
	var title string
	switch status {
	case "attention":
		rows, title = attn, "Needs attention"
	case "inprogress":
		rows, title = prog, "In progress"
	case "completed":
		rows, title = doneAll, "Completed"
	default:
		status = "all"
		title = "All actions"
		rows = make([]db.Command, 0, len(cmds))
		for _, c := range cmds { // everything except OTA, newest first
			if c.Type != "ota" {
				rows = append(rows, c)
			}
		}
	}

	// Optional server-side type filter — applied before pagination so it composes
	// with paging (selecting a type then paging keeps the type).
	cmdType := r.URL.Query().Get("type")
	if cmdType != "" {
		filtered := make([]db.Command, 0, len(rows))
		for _, c := range rows {
			if c.Type == cmdType {
				filtered = append(filtered, c)
			}
		}
		rows = filtered
	}

	// Pagination.
	const pageSize = 40
	total := len(rows)
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}
	page := 1
	if p, e := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("page"))); e == nil && p > 0 {
		page = p
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if end > total {
		end = total
	}
	var pageRows []db.Command
	if total > 0 {
		pageRows = rows[start:end]
	}

	// Per-row bucket (for state/retry) + serials (one batched query), for the page.
	bucketByID := make(map[uuid.UUID]string, len(pageRows))
	var serialIDs []uuid.UUID
	for _, c := range pageRows {
		bucketByID[c.ID] = commandBucket(c, summaries[c.ID], dismissed[c.ID])
		if c.TargetType == "devices" {
			serialIDs = append(serialIDs, c.ID)
		}
		for _, m := range batches[c.ID] {
			if m.ID != c.ID && m.TargetType == "devices" {
				serialIDs = append(serialIDs, m.ID)
			}
		}
	}
	targetSerials, _ := h.db.GetCommandTargetSerialsBatch(r.Context(), serialIDs)

	data := map[string]any{
		"Title":         "History — " + title,
		"Heading":       title,
		"Status":        status,
		"FilterType":    cmdType,
		"Rows":          pageRows,
		"BucketByID":    bucketByID,
		"Summaries":     summaries,
		"Batches":       batches,
		"Clusters":      clusters,
		"AppIcons":      apkURLToIcon(apps),
		"PkgIcons":      pkgToIcon(apps),
		"AppNames":      apkURLToName(apps),
		"TargetSerials": targetSerials,
		"AttnCount":     len(attn),
		"ProgCount":     len(prog),
		"DoneCount":     len(doneAll),
		"AllCount":      len(rows),
		"Page":          page,
		"Total":         total,
		"TotalPages":    totalPages,
	}
	// The #hist-live self-refresh requests partial=histlive and swaps the returned
	// fragment directly (no hx-select on a full document, which could parse to
	// nothing and blank the region). Serve just that block for the refresh.
	if r.URL.Query().Get("partial") == "histlive" {
		h.renderCachedHTML(w, r, "action-history-live", h.withRole(r, data))
		return
	}
	h.render(w, r, "command_history.html", data)
}

func (h *Handler) CommandDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid command ID", http.StatusBadRequest)
		return
	}
	// Admin/dev can delete anything. Operator can only delete a command still In progress
	// or in Needs attention — the same two buckets they can already act on elsewhere
	// (Resend, Dismiss) — not settled history under Completed. Reuses the exact
	// classification the page itself displays, so "can I delete this row" always
	// matches what bucket it's actually shown in.
	if role := h.role(r); role != "admin" {
		cmd, err := h.db.GetCommand(r.Context(), id)
		if err != nil {
			http.Error(w, "Command not found", http.StatusNotFound)
			return
		}
		summaries, _ := h.db.GetCommandDeliverySummaries(r.Context(), h.cfg.CommandExpiry(), actionsWindowDays)
		dismissed, _ := h.db.ListDismissedCommandIDs(r.Context())
		if commandBucket(*cmd, summaries[id], dismissed[id]) == "done" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
	}
	// Grab the targeted devices BEFORE the delete cascades command_targets/status
	// away, so we can tell any device mid-download to stop.
	deviceIDs, _ := h.db.GetCommandDeviceIDs(r.Context(), id)
	// Deleting cancels it on every device it reached, so a restricted user may only
	// delete a command whose every device is theirs to manage the queue of.
	if acc := h.access(r); !acc.unrestricted() {
		if _, dropped := acc.filterDevices("queue", deviceIDs); dropped > 0 {
			h.denied(r, "queue", nil, decision{Reason: "command reaches devices outside the policy"})
			http.Error(w, "This action reached devices outside your access policy, so you can't delete it.", http.StatusForbidden)
			return
		}
	}
	if err := h.db.DeleteCommand(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Push a cancel frame to each reached device. Harmless to a device that isn't
	// running this command — the client only acts on a cmd id it's actively
	// downloading. Aborts the in-flight APK download so a removed action doesn't
	// keep pulling bytes (and installing) after the operator deleted it.
	if len(deviceIDs) > 0 {
		if msg, mErr := json.Marshal(map[string]any{"type": "cancel_command", "id": id.String()}); mErr == nil {
			for _, did := range deviceIDs {
				h.hub.Push(did, msg)
			}
		}
	}
	// Fires the same command-update SSE event a status change would, so every open
	// Actions/history view (including this tab) re-fetches its capped bucket and
	// morphs in place — the next hidden row slides into view instead of the bucket
	// just shrinking by one, and other tabs/users stay in sync.
	h.hub.PublishCommandUpdate(id)
	if hxReq(r) {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/commands", http.StatusFound)
}

// resendPayload is the payload a resend repeats: the stored one, with its secret
// fields while they are still kept. A wifi_config whose password has been swept cannot
// be repeated from here — the new command would carry no password — so it says so.
func (h *Handler) resendPayload(w http.ResponseWriter, r *http.Request, cmd *db.Command) (json.RawMessage, bool) {
	payload := h.db.DeliveryPayload(r.Context(), cmd.ID, cmd.Type, cmd.Payload)
	if _, msg := product.NormalizeCommandPayload(cmd.Type, payload); msg != "" {
		http.Error(w, cmdTypeLabel(cmd.Type)+" can't be resent: the password is no longer kept. Send it again with the password.", http.StatusBadRequest)
		return nil, false
	}
	return payload, true
}

func (h *Handler) CommandResendDevice(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid command ID", http.StatusBadRequest)
		return
	}
	serial := r.PathValue("serial")
	if serial == "" {
		http.Error(w, "Serial required", http.StatusBadRequest)
		return
	}
	cmd, err := h.db.GetCommand(r.Context(), id)
	if err != nil {
		http.Error(w, "Command not found", http.StatusNotFound)
		return
	}
	if !h.commandTypeAllowed(h.role(r), cmd.Type) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	deviceIDs, err := h.db.GetDeviceIDsBySerials(r.Context(), []string{serial})
	if err != nil || len(deviceIDs) == 0 {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, policyActionForCommand(cmd.Type), deviceIDs[0]) {
		return
	}
	payload, ok := h.resendPayload(w, r, cmd)
	if !ok {
		return
	}
	newCmd, err := h.db.CreateCommandBy(r.Context(), cmd.Type, cmd.ApkURL, payload, "devices", deviceIDs, h.currentUsername(r))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.pushCommand(r.Context(), newCmd, "devices", deviceIDs)
	http.Redirect(w, r, "/commands/"+newCmd.ID.String(), http.StatusFound)
}

func (h *Handler) CommandResendAll(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid command ID", http.StatusBadRequest)
		return
	}
	cmd, err := h.db.GetCommand(r.Context(), id)
	if err != nil {
		http.Error(w, "Command not found", http.StatusNotFound)
		return
	}
	if !h.commandTypeAllowed(h.role(r), cmd.Type) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	targetIDs, err := h.db.GetCommandTargetIDs(r.Context(), id)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	targetIDs, targetType, ok := h.enforceCommandTargets(w, r, policyActionForCommand(cmd.Type), cmd.TargetType, targetIDs)
	if !ok {
		return
	}
	payload, ok := h.resendPayload(w, r, cmd)
	if !ok {
		return
	}
	newCmd, err := h.db.CreateCommandBy(r.Context(), cmd.Type, cmd.ApkURL, payload, targetType, targetIDs, h.currentUsername(r))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// targetType, not cmd.TargetType: a group or "all" narrowed by the policy is a
	// device list now, and pushing it as the original type reaches the whole group.
	h.pushCommand(r.Context(), newCmd, targetType, targetIDs)
	http.Redirect(w, r, "/commands/"+newCmd.ID.String(), http.StatusFound)
}

func (h *Handler) CommandDetail(w http.ResponseWriter, r *http.Request) {
	from := r.URL.Query().Get("from")
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid command ID", http.StatusBadRequest)
		return
	}
	cmd, err := h.db.GetCommand(r.Context(), id)
	if err != nil {
		http.Error(w, "Command not found", http.StatusNotFound)
		return
	}
	deliveries, err := h.db.GetCommandDeliveries(r.Context(), id, h.cfg.CommandExpiry())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	deliveries = h.keepVisibleDeliveries(r, deliveries)
	h.markDeliveryPresence(deliveries)
	// Operators must not see raw shell command detail (text/output) — hide its
	// existence, mirroring the shell filtering applied to every history listing.
	if cmd.Type == "shell" && hideShellForRole(h.role(r)) {
		http.Error(w, "Command not found", http.StatusNotFound)
		return
	}
	// Mirrors filterAdminCommands: operators must not reach an admin-created
	// command's detail page directly either.
	if len(h.filterHiddenCommands(r, []db.Command{*cmd})) == 0 {
		http.Error(w, "Command not found", http.StatusNotFound)
		return
	}
	// The "show admin actions" toggle only filters lists: an admin or dev may
	// always open a command by URL (they land here right after sending one).
	if hideAdminCommandsForRole(h.role(r)) && h.isAdminAction(*cmd) {
		http.Error(w, "Command not found", http.StatusNotFound)
		return
	}
	if !canSeeCommandURLs(h.role(r)) {
		cmd.ApkURL = ""
	}
	single := []db.Command{*cmd}
	h.resolveCommandActors(r.Context(), single)
	*cmd = single[0]
	// Resolve the app behind an install/uninstall so the page can show its icon
	// and name instead of a download URL or package id.
	var app *db.App
	if cmd.Type == "install_apk" || cmd.Type == "uninstall" {
		var pkg string
		if cmd.Type == "uninstall" {
			var p struct {
				Package string `json:"package"`
			}
			if json.Unmarshal(cmd.Payload, &p) == nil {
				pkg = p.Package
			}
		}
		if apps, err := h.db.ListApps(r.Context()); err == nil {
			for i := range apps {
				if (cmd.ApkURL != "" && apps[i].ApkURL == cmd.ApkURL) || (pkg != "" && apps[i].PackageName == pkg) {
					app = &apps[i]
					break
				}
			}
		}
	}
	// Sibling commands from the same multi-app send, with their apps resolved.
	type sib struct {
		ID   uuid.UUID
		Name string
		Icon string
		Self bool
	}
	var siblings []sib
	if b := batchOf(*cmd); b != "" {
		if members, err := h.db.ListCommandsByBatch(r.Context(), b); err == nil && len(members) > 1 {
			apps, _ := h.db.ListApps(r.Context())
			byURL := map[string]db.App{}
			for _, a := range apps {
				byURL[a.ApkURL] = a
			}
			for _, m := range members {
				a := byURL[m.ApkURL]
				name := a.Name
				if name == "" {
					name = cmdTypeLabel(m.Type)
				}
				siblings = append(siblings, sib{ID: m.ID, Name: name, Icon: a.Icon, Self: m.ID == cmd.ID})
			}
		}
	}
	nick := h.deliveryNicknames(r.Context(), deliveries)
	h.render(w, r, "command_detail.html", map[string]any{
		"Title":      "Action " + id.String()[:8],
		"Command":    cmd,
		"App":        app,
		"Siblings":   siblings,
		"Nicknames":  nick,
		"Deliveries": deliveries,
		// What actually happened to this command, per device, with the waits named — the
		// difference between "we sent it" and "it ran". See command_timeline.go.
		"Timelines":  h.commandTimelines(r.Context(), cmd.ID, deliveries),
		"Stats":      computeDeliveryStats(deliveries),
		"From":       from,
		"CanResend":  h.commandTypeAllowed(h.role(r), cmd.Type),
	})
}

// CommandScreenshot serves a screenshot delivery's PNG from a normal same-origin
// URL. The gallery stores each shot as a base64 data: URI, which renders fine as an
// inline <img> but cannot be opened or downloaded through a link: browsers block
// top-level navigation to data: URIs, so the "open" and "download" links landed on a
// blank tab until a manual refresh (FW-2026-000018). Decoding the stored base64 here
// gives those links a real URL that loads on first paint.
func (h *Handler) CommandScreenshot(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid command ID", http.StatusBadRequest)
		return
	}
	serial := r.PathValue("serial")
	cmd, err := h.db.GetCommand(r.Context(), id)
	if err != nil {
		http.Error(w, "Command not found", http.StatusNotFound)
		return
	}
	if cmd.Type != "screenshot" {
		http.Error(w, "Not a screenshot command", http.StatusBadRequest)
		return
	}
	deliveries, err := h.db.GetCommandDeliveries(r.Context(), id, h.cfg.CommandExpiry())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	deliveries = h.keepVisibleDeliveries(r, deliveries)
	var b64 string
	for _, d := range deliveries {
		if d.SerialNumber == serial {
			b64 = d.Output
			break
		}
	}
	if b64 == "" {
		http.Error(w, "Screenshot not available", http.StatusNotFound)
		return
	}
	png, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		http.Error(w, "Corrupt screenshot", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=300")
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "screenshot-"+serial+".png"))
	}
	w.Write(png)
}

// withBatch adds "batch": id to a command payload (JSON object; empty → {}).
func withBatch(payload json.RawMessage, id string) json.RawMessage {
	m := map[string]json.RawMessage{}
	if len(payload) > 0 {
		_ = json.Unmarshal(payload, &m)
	}
	b, _ := json.Marshal(id)
	m["batch"] = b
	out, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return out
}

// batchOf returns the payload.batch id of a command, or "".
func batchOf(c db.Command) string {
	if len(c.Payload) == 0 {
		return ""
	}
	var p struct {
		Batch string `json:"batch"`
	}
	if json.Unmarshal(c.Payload, &p) != nil {
		return ""
	}
	return p.Batch
}

// collapseBatches folds commands that share a payload.batch id into their first
// member: the returned list has one representative per batch (others removed),
// the representative's summary is the sum of the members', and batches maps the
// representative's id to every member (itself first) so the history row can show
// "Install 3 apps" with an icon cluster. Commands without a batch pass through.
func collapseBatches(cmds []db.Command, summaries map[uuid.UUID]db.CommandDeliverySummary) ([]db.Command, map[uuid.UUID]db.CommandDeliverySummary, map[uuid.UUID][]db.Command) {
	batches := map[uuid.UUID][]db.Command{}
	rep := map[string]uuid.UUID{}
	var out []db.Command
	for _, c := range cmds {
		b := batchOf(c)
		if b == "" {
			out = append(out, c)
			continue
		}
		if r, ok := rep[b]; ok {
			batches[r] = append(batches[r], c)
			continue
		}
		rep[b] = c.ID
		batches[c.ID] = []db.Command{c}
		out = append(out, c)
	}
	if len(batches) == 0 {
		return cmds, summaries, map[uuid.UUID][]db.Command{}
	}
	merged := make(map[uuid.UUID]db.CommandDeliverySummary, len(summaries))
	for k, v := range summaries {
		merged[k] = v
	}
	for r, members := range batches {
		if len(members) < 2 {
			delete(batches, r)
			continue
		}
		var sum db.CommandDeliverySummary
		sum.CommandID = r
		for _, m := range members {
			sm := summaries[m.ID]
			sum.Pending += sm.Pending
			sum.Delivered += sm.Delivered
			sum.Completed += sm.Completed
			sum.Failed += sm.Failed
			sum.Downloading += sm.Downloading
			sum.Installing += sm.Installing
		}
		merged[r] = sum
	}
	return out, merged, batches
}

// attnMaxAge is how recent a failure must be to still count as "needs attention"
// — older failures drop into Completed so the triage list stays actionable.
const attnMaxAge = 72 * time.Hour

// commandBucket classifies one command from its delivery summary into
// "attn" (recent, undismissed failure), "prog" (in flight) or "done".
func commandBucket(c db.Command, s db.CommandDeliverySummary, dismissed bool) string {
	switch {
	case s.Failed > 0 && !dismissed && c.CreatedAt.After(time.Now().Add(-attnMaxAge)):
		return "attn"
	case s.Failed == 0 && (s.Pending > 0 || s.Delivered > 0):
		return "prog"
	default:
		return "done"
	}
}

// classifyCommands buckets non-OTA commands (newest first) into the three triage
// lists. Commands in the dismissed set are kept out of Needs-attention. Shared by
// the Actions page and the standalone history page.
func classifyCommands(cmds []db.Command, summaries map[uuid.UUID]db.CommandDeliverySummary, dismissed map[uuid.UUID]bool) (attn, prog, done []db.Command) {
	for _, c := range cmds {
		if c.Type == "ota" {
			continue
		}
		switch commandBucket(c, summaries[c.ID], dismissed[c.ID]) {
		case "attn":
			attn = append(attn, c)
		case "prog":
			prog = append(prog, c)
		default:
			done = append(done, c)
		}
	}
	return
}

// AttentionClear dismisses every command currently in Needs-attention from that
// list. It records dismissals only — the commands and their delivery history are
// left intact (they remain visible under Completed / history).
func (h *Handler) AttentionClear(w http.ResponseWriter, r *http.Request) {
	cmds, err := h.db.ListCommandsSince(r.Context(), actionsWindowDays)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	summaries, _ := h.db.GetCommandDeliverySummaries(r.Context(), h.cfg.CommandExpiry(), actionsWindowDays)
	dismissed, _ := h.db.ListDismissedCommandIDs(r.Context())
	attn, _, _ := classifyCommands(cmds, summaries, dismissed)
	ids := make([]uuid.UUID, 0, len(attn))
	for _, c := range attn {
		ids = append(ids, c.ID)
	}
	if err := h.db.DismissCommands(r.Context(), ids, h.role(r)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "command.attention_clear", "", fmt.Sprintf("cleared=%d", len(ids)))
	http.Redirect(w, r, "/commands", http.StatusFound)
}

// cmdTypeLabel maps a command type to its friendly label for the dashboard.
// Package-level so both the template funcmap and handlers (recipes, timeline)
// share one source of truth.
func cmdTypeLabel(cmdType string) string {
	switch cmdType {
	case "install_apk":
		return "Install app"
	case "uninstall":
		return "Uninstall"
	case "shell":
		return "Shell"
	case "query":
		return "Query"
	case "screenshot":
		return "Screenshot"
	case "reboot":
		return "Reboot"
	case "wipe":
		return "Wipe"
	case "update_splash":
		return "Boot logo"
	case "app_reload":
		return "Reload page"
	case "app_restart":
		return "Restart app"
	case "app_clear_cache":
		return "Clear web cache"
	case "app_update_check":
		return "Check for update"
	case "app_update":
		return "Client update"
	case "logcat":
		return "Log capture"
	case "collect_logs":
		return "Collect logs"
	case "ota":
		return "OTA update"
	case "adb_tcp":
		return "Wireless adb"
	case "lost_mode":
		return "Lost mode"
	case "wifi_config":
		return "Wi-Fi network"
	case "set_kiosk", "kiosk_set":
		return "Kiosk"
	case "remote":
		return "Remote control"
	case "mic_gain_read":
		return "Mic gain read"
	case "mic_gain_set":
		return "Mic gain set"
	default:
		return cmdType
	}
}

// isSystemReboot: a reboot the update flow asked for — recognised by its payload
// reason, not by who sent it, so "Reboot to apply" pressed by a person clusters too.
func isSystemReboot(c db.Command) bool { return isUpdateReboot(c) }

// clusterSystemReboots folds runs of OTA-sent reboots (each targets one device,
// created within minutes of each other) into one representative row per run,
// mirroring collapseBatches. Returns the reduced list, merged summaries and the
// cluster members keyed by representative id.
func clusterSystemReboots(cmds []db.Command, summaries map[uuid.UUID]db.CommandDeliverySummary) ([]db.Command, map[uuid.UUID]db.CommandDeliverySummary, map[uuid.UUID][]db.Command) {
	// runKey names the run a command belongs to, or "" when it stands alone: update
	// reboots (20 min apart at most), and a person's identical wireless-adb switches
	// fanned out one per device (2 min) — the adb page's batch before it carried a
	// batch id, and the same shape from a script.
	runKey := func(c db.Command) (string, time.Duration) {
		switch {
		case isSystemReboot(c):
			return "reboot|ota", 20 * time.Minute
		case c.Type == "adb_tcp" && c.TargetType == "devices" && batchOf(c) == "":
			return "adb_tcp|" + c.CreatedBy + "|" + string(c.Payload), 2 * time.Minute
		}
		return "", 0
	}
	clusters := map[uuid.UUID][]db.Command{}
	var out []db.Command
	var rep uuid.UUID
	var last time.Time
	var lastKey string
	for _, c := range cmds {
		key, gap := runKey(c)
		if key == "" {
			out = append(out, c)
			continue
		}
		d := last.Sub(c.CreatedAt)
		if d < 0 {
			d = -d
		}
		if !last.IsZero() && key == lastKey && d <= gap {
			clusters[rep] = append(clusters[rep], c)
		} else {
			rep = c.ID
			clusters[rep] = []db.Command{c}
			out = append(out, c)
		}
		last, lastKey = c.CreatedAt, key
	}
	merged := make(map[uuid.UUID]db.CommandDeliverySummary, len(summaries))
	for k, v := range summaries {
		merged[k] = v
	}
	for r, members := range clusters {
		if len(members) < 2 {
			delete(clusters, r)
			continue
		}
		var sum db.CommandDeliverySummary
		sum.CommandID = r
		for _, m := range members {
			sm := summaries[m.ID]
			sum.Pending += sm.Pending
			sum.Delivered += sm.Delivered
			sum.Completed += sm.Completed
			sum.Failed += sm.Failed
		}
		merged[r] = sum
	}
	return out, merged, clusters
}

// ── WS push helpers ───────────────────────────────────────────────────────────

func (h *Handler) pushCommand(ctx context.Context, cmd *db.Command, targetType string, targetIDs []uuid.UUID) {
	msg, _ := json.Marshal(map[string]any{
		"type":         "command",
		"id":           cmd.ID,
		"command_type": db.DeviceCommandType(cmd.Type),
		"apk_url":      cmd.ApkURL,
		"payload":      h.db.DeliveryPayload(ctx, cmd.ID, cmd.Type, cmd.Payload),
	})

	switch targetType {
	case "all":
		h.hub.Broadcast(msg)
		h.hub.PublishCommandUpdate(cmd.ID)
		return
	case "groups":
		ids, err := h.db.GetDeviceIDsByGroupIDs(ctx, targetIDs)
		if err != nil {
			return
		}
		targetIDs = ids
	}

	var pushed []uuid.UUID
	for _, deviceID := range targetIDs {
		// Surface the new queue entry on that device's own page immediately (its Queue
		// tab listens for this), instead of only on its next 300s poll or a reload —
		// whether the command ends up pushed, held behind an earlier one, or the device
		// is offline and never gets a push at all.
		h.hub.PublishDeviceUpdate(deviceID)
		// The whole queue runs one command at a time per device: if an earlier command for
		// this device is still unfinished, hold this one (it stays queued and is flushed when
		// the current one finishes).
		if blocked, err := h.db.CommandBlocked(ctx, cmd.ID, deviceID); err == nil && blocked {
			continue
		}
		if h.hub.Push(deviceID, msg) {
			pushed = append(pushed, deviceID)
		}
	}
	// One batched status write for all online targets (was a query per device).
	// A pushed command is only 'delivered', never 'completed' here — including reboot.
	// Marking a reboot 'completed' at delivery (as this path used to) is a false
	// positive: hub.Push succeeding only means the frame was queued, not that the device
	// rebooted; a device that never reboots (low battery / declined / half-open socket)
	// then shows a false 'completed'. Reboot flips to 'completed' only when the device
	// actually comes back — CompleteDeliveredReboots on WS connect / next check-in
	// (FW-2026-000033). This matches the api-side pushCommand.
	_ = h.db.SetCommandStatusForDevices(ctx, cmd.ID, pushed, "delivered", false)
	// Surface the new delivery/ack state on the command detail page in real time
	// instead of waiting for its 30s polling fallback.
	h.hub.PublishCommandUpdate(cmd.ID)
}
