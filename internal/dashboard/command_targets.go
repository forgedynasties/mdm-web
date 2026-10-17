package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"mdm/internal/db"
	"mdm/internal/otagate"
	"mdm/internal/product"

	"github.com/google/uuid"
)

// CommandBrowseDevices renders the filtered device picker for the command builder's
// CommandResolveSerials backs the Actions console's paste-a-list flow: given the
// serials the operator pasted (comma/space/newline separated), it answers which ones
// name a real active device and which do not, so the console adds only the former
// and shows the rest in a popup instead of silently dropping them at send.
func (h *Handler) CommandResolveSerials(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("serials")
	var serials []string
	for _, t := range strings.FieldsFunc(raw, func(c rune) bool { return c == ',' || c == ';' || c == ' ' || c == '\n' || c == '\r' || c == '\t' }) {
		if t = cleanSerialToken(t); t != "" {
			serials = append(serials, t)
		}
		if len(serials) >= 500 {
			break
		}
	}
	found, missing, err := h.db.ResolveSerials(r.Context(), serials)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Scope to what this user may target: devices outside their access policy count
	// as unknown, matching how the send path drops them.
	if ids := h.access(r).visibleIDs(); ids != nil {
		devices, _ := h.db.GetDevicesByIDs(r.Context(), ids)
		allowed := map[string]bool{}
		for _, d := range devices {
			allowed[d.SerialNumber] = true
		}
		kept := found[:0]
		for _, sn := range found {
			if allowed[sn] {
				kept = append(kept, sn)
			} else {
				missing = append(missing, sn)
			}
		}
		found = kept
	}
	if found == nil {
		found = []string{}
	}
	if missing == nil {
		missing = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"found": found, "missing": missing})
}

// "Specific" target — the same filters as the Devices list and the new-group browser
// (search, status, group, production, build, battery). Checking rows feeds the target
// serial chips; "Select all matching" turns the current filter into the target set.
func (h *Handler) CommandBrowseDevices(w http.ResponseWriter, r *http.Request) {
	filter := db.DeviceFilter{
		Search:              r.URL.Query().Get("q"),
		Online:              r.URL.Query().Get("status"),
		BuildID:             r.URL.Query().Get("build"),
		Battery:             r.URL.Query().Get("battery"),
		Kiosk:               r.URL.Query().Get("kiosk"),
		ActiveThresholdSecs: h.cfg.CheckinInterval() * 3,
	}
	if gid := r.URL.Query().Get("group"); gid != "" {
		if id, err := uuid.Parse(gid); err == nil {
			filter.GroupID = id
		}
	}
	if pid := r.URL.Query().Get("production"); pid != "" {
		if id, err := uuid.Parse(pid); err == nil {
			filter.ProductionID = id
		}
	}
	// ?source=legacy: rows come from the otautil devices instead of the fleet.
	// They are not in the devices table by design, so they get their own path here
	// and the same markup, which is what lets one picker serve both.
	if r.URL.Query().Get("source") == "legacy" {
		h.browseLegacyDevices(w, r)
		return
	}
	// ?product=<key>: the hardware chosen on the push screen, before a release is
	// named. A ?release= below overrides it — the release's own product is the truth.
	if pk := strings.TrimSpace(r.URL.Query().Get("product")); pk != "" {
		filter.Product = product.Normalize(pk)
	}
	// ?release=<id>: a release only ever targets its own product, so scope the list
	// the way the push path does instead of listing devices it could never reach.
	var pushRel *db.Release
	if relRaw := strings.TrimSpace(r.URL.Query().Get("release")); relRaw != "" {
		if relID, err := strconv.Atoi(relRaw); err == nil {
			if rel, err := h.db.GetRelease(r.Context(), relID); err == nil && rel != nil {
				pushRel = rel
				filter.Product = rel.Product
			}
		}
	}
	if gid := r.URL.Query().Get("exclude_group"); gid != "" {
		if id, err := uuid.Parse(gid); err == nil {
			filter.ExcludeGroupID = id
		}
	}
	// The picker sends this when it is assigning devices to a venue; without it the
	// venue's own devices were offered back to it.
	if rid := r.URL.Query().Get("exclude_restaurant"); rid != "" {
		if id, err := uuid.Parse(rid); err == nil {
			filter.ExcludeRestaurantID = id
		}
	}
	if rid := r.URL.Query().Get("restaurant"); rid != "" {
		if id, err := uuid.Parse(rid); err == nil {
			filter.RestaurantID = id
		}
	}
	h.access(r).applyFilter(&filter)
	devices, err := h.db.ListDevices(r.Context(), filter, 0, 500, r.URL.Query().Get("sort"), r.URL.Query().Get("dir"))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	connected := h.hub.ConnectedIDs()
	online := make(map[uuid.UUID]bool, len(connected))
	for id := range connected {
		online[id] = true
	}
	// Per-device agent type so the Actions console rail can tag DPC devices and
	// filter/subset by capability. ListDevices already carries latest_extra.
	dpc := make(map[uuid.UUID]bool, len(devices))
	for _, d := range devices {
		if d.IsDPC() {
			dpc[d.ID] = true
		}
	}
	// ?release=<id>: annotate each row with whether this release can be pushed to
	// that device and, if it can, which artifact it would get. The Deploy page's
	// picker needs it; every other caller leaves the maps empty and the rows render
	// exactly as before.
	blocked, artifact := map[string]string{}, map[string]string{}
	var legacyRows []legacyPickerRow
	// A scoped request ("every device in this restaurant/group/production") is
	// answered by the picker with "select all of these", so it must contain only
	// devices that really are in that scope. Legacy-only devices are not in the
	// fleet at all and so belong to no restaurant or group — including them here
	// made a restaurant of 8 devices select 10.
	scoped := filter.RestaurantID != uuid.Nil || filter.GroupID != uuid.Nil || filter.ProductionID != uuid.Nil
	if pushRel != nil {
		blocked, artifact = h.releaseEligibility(r.Context(), pushRel, devices)
		// Devices that are not in the fleet at all but do poll the legacy listener:
		// one push covers both kinds now, so they belong in the same list.
		if !scoped {
			legacyRows = h.legacyPickerRows(r, pushRel, blocked, artifact, r.URL.Query().Get("q"), r.URL.Query().Get("status"))
		}
		// What can be pushed comes first. A blocked row still belongs in the list —
		// "why can't I pick this one" is the next question — but scrolling past a
		// screenful of up-to-date devices to reach the three that need the build is
		// the picker failing at its one job.
		sort.SliceStable(devices, func(i, j int) bool {
			return blocked[devices[i].SerialNumber] == "" && blocked[devices[j].SerialNumber] != ""
		})
		sort.SliceStable(legacyRows, func(i, j int) bool {
			return blocked[legacyRows[i].Device.Serial] == "" && blocked[legacyRows[j].Device.Serial] != ""
		})
	}
	h.tmpl.ExecuteTemplate(w, "cmd-device-browser", map[string]any{
		"Devices":    devices,
		"Online":     online,
		"DPC":        dpc,
		"Blocked":    blocked,
		"Artifact":   artifact,
		"LegacyRows": legacyRows,
	})
}

// releaseEligibility answers, per serial, why a release cannot be pushed to that
// device ("" when it can) and which artifact it would receive. Same rules the
// push path enforces, so the picker can't offer a device the deploy would drop:
// already on the build, mid-update, on a newer release, no applicable package,
// or firmware older than the product's MDM OTA cutoff.
func (h *Handler) releaseEligibility(ctx context.Context, rel *db.Release, devices []db.Device) (map[string]string, map[string]string) {
	blocked, artifact := map[string]string{}, map[string]string{}
	relID := rel.ID
	hasFull := false
	sourceBuilds := map[string]bool{}
	pkgs, _ := h.db.ListPackagesByRelease(ctx, relID)
	for _, p := range pkgs {
		if p.Status != "active" {
			continue
		}
		if p.Type == "full" {
			hasFull = true
		} else if p.SourceBuildID != "" {
			sourceBuilds[p.SourceBuildID] = true
		}
	}
	updating, _ := h.db.SerialsUpdating(ctx)
	newer, _ := h.db.SerialsOnNewerRelease(ctx, relID)
	for _, d := range devices {
		s := d.SerialNumber
		// A build with no MDM OTA is not blocked any more — it is served the same
		// release over the legacy path, in the same rollout. Only a full image can
		// reach one, since the legacy client has no incremental story.
		verdict := h.otaGate.Device(ctx, d)
		legacy := verdict.Legacy()
		switch {
		case verdict.Source == otagate.SourceDPC:
			blocked[s] = "DPC device — OTA is firmware-only"
		case d.BuildID == rel.Version:
			blocked[s] = "up to date"
		case newer[s] != "":
			blocked[s] = "newer installed (" + newer[s] + ")"
		case updating[s] != "":
			blocked[s] = "already updating"
		case legacy && !hasFull:
			blocked[s] = "legacy OTA needs a full image"
		case legacy:
			artifact[s] = "legacy"
		case !hasFull && !sourceBuilds[d.BuildID]:
			blocked[s] = "no update for this build"
		case sourceBuilds[d.BuildID]:
			artifact[s] = "incremental"
		default:
			artifact[s] = "full"
		}
	}
	return blocked, artifact
}

// CommandImpact renders the Actions builder's live "blast radius" panel: given
// the current builder state (action type + target selection), it resolves the
// concrete device set and summarises online/offline split and action-specific
// risks (low battery for reboot, missing-package skips for uninstall). It reuses
// the same resolution the send path uses, so the preview matches what will fire.
func (h *Handler) CommandImpact(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	cmdType := r.FormValue("type")
	if cmdType == "" {
		cmdType = "install_apk"
	}
	targetType := r.FormValue("target_type")

	// Resolve the target device set the same way CommandCreate does (all/devices/
	// groups/scope) so the impact count is exactly what a send would hit.
	ids, _ := h.resolveTargetDeviceIDs(r, targetType)
	// The preview counts only what this user may see; the send path narrows the same way.
	ids, _ = h.access(r).filterDevices("view", ids)
	devices, _ := h.db.GetDevicesByIDs(r.Context(), ids)
	connected := h.hub.ConnectedIDs()

	// DPC split: how many of the resolved targets run the Device-Owner agent.
	// The Actions console uses this (via data-dpc on the fragment root) to dim
	// system-app-only actions and to scope the DPC-only wipe. Additive — older
	// consumers of this fragment just ignore the extra attribute.
	dpcCount, _ := h.db.CountDPCDevices(r.Context(), ids)

	// Uninstall is a no-op on devices without the package — count and subtract.
	// Only among devices that can uninstall at all: the rest are already counted
	// as unsupported below, and must not be subtracted twice.
	skipped := 0
	if cmdType == "uninstall" {
		if pkg := strings.TrimSpace(r.FormValue("package")); pkg != "" {
			var capable []uuid.UUID
			for _, d := range devices {
				if d.Supports("uninstall") {
					capable = append(capable, d.ID)
				}
			}
			have := 0
			if len(capable) > 0 {
				have, _ = h.db.CountDevicesWithPackage(r.Context(), capable, pkg)
			}
			skipped = len(capable) - have
			if skipped < 0 {
				skipped = 0
			}
		}
	}
	// Capability gating: devices whose agent cannot run the action are skipped at
	// send (see CommandCreate), so the preview subtracts them here and names them.
	// unsupportedBy carries the same count for every console action so the grid
	// can caveat each card without another round-trip.
	unsupported := 0
	var unsupSerials []string
	unsupportedBy := map[string]int{}
	// Which agent each target runs, and which of them can run each console action.
	// The three kinds are counted apart because "DPC" alone is a lie on this page:
	// MDM Lite is stored as a DPC kind, so a fleet with one of each looked like two
	// Device Owners. Per-kind support drives the badges on every action card, and the
	// per-kind split of what cannot run it is what the skip list reads out.
	liteIDs, _ := h.db.MDMLiteIDs(r.Context(), ids)
	kindOf := func(d db.Device) string {
		switch {
		case liteIDs[d.ID]:
			return "lite"
		case d.IsDPC():
			return "dpc"
		}
		return "fw"
	}
	kindCount := map[string]int{"fw": 0, "dpc": 0, "lite": 0}
	kindSup := map[string]map[string]int{}
	type skipRow struct {
		Serial string `json:"s"`
		Kind   string `json:"k"`
	}
	var unsupRows []skipRow
	for _, d := range devices {
		k := kindOf(d)
		kindCount[k]++
		if !d.Supports(cmdType) {
			unsupported++
			if len(unsupSerials) < 500 {
				unsupSerials = append(unsupSerials, d.SerialNumber)
			}
			if len(unsupRows) < 200 {
				unsupRows = append(unsupRows, skipRow{Serial: d.SerialNumber, Kind: k})
			}
		}
		for _, t := range []string{"install_apk", "uninstall", "reboot", "screenshot", "query", "collect_logs", "shell", "set_kiosk", "update_splash", "wipe"} {
			need := t
			if t == "set_kiosk" {
				need = "kiosk_set"
			}
			if kindSup[t] == nil {
				kindSup[t] = map[string]int{"fw": 0, "dpc": 0, "lite": 0}
			}
			if d.Supports(need) {
				kindSup[t][k]++
			} else {
				unsupportedBy[t]++
			}
		}
	}
	kindJSON, _ := json.Marshal(kindCount)
	kindSupJSON, _ := json.Marshal(kindSup)
	skipJSON, _ := json.Marshal(unsupRows)
	skipped += unsupported
	unsupJSON, _ := json.Marshal(unsupportedBy)

	type rollRow struct {
		Serial  string
		Online  bool
		Battery int
	}
	online, lowBatOnline := 0, 0
	roll := make([]rollRow, 0, len(devices))
	for _, d := range devices {
		_, up := connected[d.ID]
		if up {
			online++
			if d.BatteryPct > 0 && d.BatteryPct < 20 {
				lowBatOnline++
			}
		}
		if len(roll) < 8 {
			roll = append(roll, rollRow{Serial: d.SerialNumber, Online: up, Battery: d.BatteryPct})
		}
	}
	total := len(devices)
	effective := total - skipped // devices that will actually receive the action
	if effective < 0 {
		effective = 0
	}
	onlineEff := online
	if onlineEff > effective {
		onlineEff = effective
	}
	offlineEff := effective - onlineEff

	var warn string
	switch cmdType {
	case "reboot":
		if lowBatOnline > 0 {
			s := ""
			if lowBatOnline != 1 {
				s = "s"
			}
			verb := "is"
			if lowBatOnline != 1 {
				verb = "are"
			}
			warn = fmt.Sprintf("%d online device%s %s under 20%% battery — rebooting risks a unit that can't power back up.", lowBatOnline, s, verb)
		}
	case "uninstall":
		if skipped-unsupported > 0 {
			n := skipped - unsupported
			s, verb := "", "doesn't"
			if n != 1 {
				s, verb = "s", "don't"
			}
			warn = fmt.Sprintf("%d targeted device%s %s report this package — they'll be skipped automatically.", n, s, verb)
		}
	}
	if unsupported > 0 {
		s := ""
		if unsupported != 1 {
			s = "s"
		}
		msg := fmt.Sprintf("%d device%s can't run %s on their agent — skipped automatically.", unsupported, s, cmdTypeLabel(cmdType))
		if warn != "" {
			warn = msg + " " + warn
		} else {
			warn = msg
		}
	}

	h.tmpl.ExecuteTemplate(w, "cmd-impact", map[string]any{
		"Type":         cmdType,
		"Total":        total,
		"Effective":    effective,
		"Online":       onlineEff,
		"Offline":      offlineEff,
		"DPC":          dpcCount,
		"Skipped":      skipped,
		"Unsupported":  unsupported,
		"UnsupJSON":    string(unsupJSON),
		"UnsupSerials": strings.Join(unsupSerials, ","),
		"KindJSON":     string(kindJSON),
		"KindSupJSON":  string(kindSupJSON),
		"SkipJSON":     string(skipJSON),
		"LowBattery":   lowBatOnline,
		"Screenshot":   cmdType == "screenshot",
		"Warn":         warn,
		"Roll":         roll,
		"More":         total - len(roll),
	})
}

// CommandTargetPackages renders the uninstall package picker scoped to the current
// target selection, so the Actions uninstall list shows only apps actually installed
// on the devices that would receive the command. htmx-refreshed when the target
// changes (see the #uninstall-list container in commands.html).
func (h *Handler) CommandTargetPackages(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	targetType := r.FormValue("target_type")
	if targetType != "all" && targetType != "devices" && targetType != "groups" && targetType != "scope" && targetType != "mixed" {
		targetType = "all"
	}
	ids, _ := h.resolveTargetDeviceIDs(r, targetType)
	// The preview counts only what this user may see; the send path narrows the same way.
	ids, _ = h.access(r).filterDevices("view", ids)
	pkgs, _ := h.db.PackagesForDevices(r.Context(), ids)
	h.tmpl.ExecuteTemplate(w, "uninstall-pkg-list", map[string]any{
		"FleetPackages": pkgs,
		"TargetCount":   len(ids),
	})
}

// markDeliveryPresence flags each delivery online when the device currently
// holds a live WebSocket connection in the hub — so the UI can distinguish a
// device that's about to ack from one that's offline and won't receive the
// command until it reconnects.
func (h *Handler) markDeliveryPresence(deliveries []db.CommandDelivery) {
	connected := h.hub.ConnectedIDs()
	for i := range deliveries {
		_, deliveries[i].Online = connected[deliveries[i].DeviceID]
	}
}

// deliveryStats is the Mission-Control rollup for a command detail page: the
// progress-ring counts derived from the per-device delivery statuses.
type deliveryStats struct {
	Total   int
	Done    int // installed / completed
	Failed  int // failed / expired (terminal, needs attention)
	Pending int // pending / delivered (in flight)
	Offline int // in-flight and currently offline (will receive on reconnect)
	Pct     int // Done / Total, 0-100
}

func computeDeliveryStats(deliveries []db.CommandDelivery) deliveryStats {
	var s deliveryStats
	s.Total = len(deliveries)
	for _, d := range deliveries {
		switch d.Status {
		case "installed", "completed":
			s.Done++
		case "failed", "expired":
			s.Failed++
		default: // pending, delivered
			s.Pending++
			if !d.Online {
				s.Offline++
			}
		}
	}
	if s.Total > 0 {
		s.Pct = s.Done * 100 / s.Total
	}
	return s
}

// resolveTargetDeviceIDs turns a command target spec (all/devices/groups/scope) into
// the concrete device-ID set — expanding groups/scope to their devices — for actions
// like kiosk/logcat that operate on device IDs directly rather than via CreateCommand.
func (h *Handler) resolveTargetDeviceIDs(r *http.Request, targetType string) ([]uuid.UUID, error) {
	switch targetType {
	case "groups":
		var gids []uuid.UUID
		for _, g := range r.Form["target_groups"] {
			if id, err := uuid.Parse(g); err == nil {
				gids = append(gids, id)
			}
		}
		return h.db.GetDeviceIDsInGroups(r.Context(), gids)
	case "devices":
		return h.db.GetDeviceIDsBySerials(r.Context(), db.ParseSerials(r.FormValue("target_serials")))
	case "scope":
		return h.resolveScopeDeviceIDs(r)
	case "mixed":
		return h.resolveMixedDeviceIDs(r)
	case "all":
		return h.db.GetAllDeviceIDs(r.Context())
	default:
		// No target chosen yet (the builder's default, empty state) — the impact
		// preview should show 0, not silently preview the whole fleet.
		return nil, nil
	}
}

// resolveMixedDeviceIDs is the console's "pick anything" target: any number of
// restaurants (target_restaurants), groups (target_groups) and serials
// (target_serials), unioned and de-duplicated by device, so a device that sits in
// two chosen scopes counts once. target_all=1 short-circuits to the whole fleet.
func (h *Handler) resolveMixedDeviceIDs(r *http.Request) ([]uuid.UUID, error) {
	if r.FormValue("target_all") == "1" {
		return h.db.GetAllDeviceIDs(r.Context())
	}
	seen := map[uuid.UUID]bool{}
	var out []uuid.UUID
	add := func(ids []uuid.UUID) {
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	parse := func(vals []string) []uuid.UUID {
		var ids []uuid.UUID
		for _, v := range vals {
			if id, err := uuid.Parse(v); err == nil {
				ids = append(ids, id)
			}
		}
		return ids
	}
	if ids, err := h.db.GetDeviceIDsInRestaurants(r.Context(), parse(r.Form["target_restaurants"])); err != nil {
		return nil, err
	} else {
		add(ids)
	}
	if ids, err := h.db.GetDeviceIDsByGroupIDs(r.Context(), parse(r.Form["target_groups"])); err != nil {
		return nil, err
	} else {
		add(ids)
	}
	if serials := db.ParseSerials(r.FormValue("target_serials")); len(serials) > 0 {
		ids, err := h.db.GetDeviceIDsBySerials(r.Context(), serials)
		if err != nil {
			return nil, err
		}
		add(ids)
	}
	return out, nil
}

// buildScopeFilter reads the scope-rail form fields (a primary collection —
// all/restaurant/group/build — plus optional refine filters) into a DeviceFilter.
func (h *Handler) buildScopeFilter(r *http.Request) db.DeviceFilter {
	f := db.DeviceFilter{ActiveThresholdSecs: h.cfg.CheckinInterval() * 3}
	switch r.FormValue("scope_mode") {
	case "restaurant":
		if id, err := uuid.Parse(r.FormValue("scope_id")); err == nil {
			f.RestaurantID = id
		}
	case "group":
		if id, err := uuid.Parse(r.FormValue("scope_id")); err == nil {
			f.GroupID = id
		}
	case "build":
		f.BuildID = r.FormValue("scope_id")
	}
	// Refine pills compose on top of the collection.
	f.Online = r.FormValue("scope_status")
	f.Battery = r.FormValue("scope_battery")
	f.Kiosk = r.FormValue("scope_kiosk")
	return f
}

// resolveScopeDeviceIDs snapshots the scope filter to the matching device IDs (like the
// "all" path — future devices that later match are unaffected). scope_mode is empty
// until the user actually picks something in the Target rail (the builder's default
// targets no devices, not the whole fleet) — buildScopeFilter's switch has no case for
// that, so an empty/unrecognized scope_mode would otherwise fall through to an
// unfiltered DeviceFilter{} and silently match every device. Refuse to resolve until
// scope_mode is a real, explicit choice.
func (h *Handler) resolveScopeDeviceIDs(r *http.Request) ([]uuid.UUID, error) {
	switch r.FormValue("scope_mode") {
	case "restaurant", "group", "build", "all":
	default:
		return nil, nil
	}
	devs, err := h.db.ListDevices(r.Context(), h.buildScopeFilter(r), 0, 20000, "serial", "asc")
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(devs))
	for i := range devs {
		ids[i] = devs[i].ID
	}
	return ids, nil
}

// applyKioskForTargets sets (or clears) locked mode on the resolved target devices and
// pushes the config so they pick it up on the next check-in. Mirrors BulkKioskUpdate,
// but resolves its target from the Actions composer's target section.
func (h *Handler) applyKioskForTargets(w http.ResponseWriter, r *http.Request, targetType string) {
	enabled := r.FormValue("kiosk_enabled") == "1"
	pkg := strings.TrimSpace(r.FormValue("kiosk_package"))
	if enabled && pkg == "" {
		http.Error(w, "Pick an app to lock to when enabling locked mode.", http.StatusBadRequest)
		return
	}
	if !enabled {
		pkg = ""
	}
	ids, err := h.resolveTargetDeviceIDs(r, targetType)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// The access policy decides which of them this user may kiosk-lock.
	ids, _ = h.access(r).filterDevices("kiosk", ids)
	// Kiosk only reaches agents that can lock the device (not MDM-lite).
	if devs, err := h.db.GetDevicesByIDs(r.Context(), ids); err == nil {
		kept := ids[:0:0]
		for _, d := range devs {
			if d.Supports("kiosk_set") {
				kept = append(kept, d.ID)
			}
		}
		ids = kept
	}
	if len(ids) == 0 {
		h.hxRedirect(w, r, "/kiosk?flash="+url.QueryEscape("No devices matched the target.")+"&flash_type=info")
		return
	}
	if max := h.cfg.MaxTargets(); max > 0 && len(ids) > max {
		http.Error(w, fmt.Sprintf("Too many target devices (%d); the configured limit is %d.", len(ids), max), http.StatusBadRequest)
		return
	}
	if err := h.db.SetKioskConfigForDevices(r.Context(), ids, enabled, pkg, 0); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.markKioskManual(r.Context(), ids)
	h.pushKioskConfigToDevices(r.Context(), ids)
	verb := "Enabled"
	if !enabled {
		verb = "Disabled"
	}
	h.audit(r, "device.kiosk_bulk", verb, fmt.Sprintf("devices=%d, package=%s", len(ids), pkg))
	h.hxRedirect(w, r, "/kiosk?flash="+url.QueryEscape(fmt.Sprintf("%s kiosk on %d device(s).", verb, len(ids)))+"&flash_type=success")
}

// resolvePolicyTargetIDs resolves a stored lock rule's target to the matching
// device IDs AT THIS MOMENT — a snapshot, not a live binding (same convention as
// resolveTargetDeviceIDs uses for the "all" target elsewhere: a device added to a
// target group later doesn't retroactively pick up the policy without a re-save).
func (h *Handler) resolvePolicyTargetIDs(ctx context.Context, targetType string, targetID *uuid.UUID, targetSerial string) ([]uuid.UUID, error) {
	switch targetType {
	case "all":
		return h.db.GetAllDeviceIDs(ctx)
	case "restaurant":
		if targetID == nil {
			return nil, nil
		}
		return h.db.GetDeviceIDsByRestaurantIDs(ctx, []uuid.UUID{*targetID})
	case "group":
		if targetID == nil {
			return nil, nil
		}
		return h.db.GetDeviceIDsInGroups(ctx, []uuid.UUID{*targetID})
	case "device":
		if targetSerial == "" {
			return nil, nil
		}
		return h.db.GetDeviceIDsBySerials(ctx, []string{targetSerial})
	default:
		return nil, nil
	}
}

// manageTargetLabel renders a policy's target as the short tag shown on its card
// ("Drive-Thru Kiosks (group)", "Riverside Ave (restaurant)", a bare serial, or
// "All devices").
func manageTargetLabel(p db.KioskPolicy, restaurants []db.Restaurant, groups []db.Group) string {
	switch p.TargetType {
	case "all":
		return "All devices"
	case "restaurant":
		for _, r := range restaurants {
			if p.TargetID != nil && r.ID == *p.TargetID {
				return r.Name + " (restaurant)"
			}
		}
		return "Restaurant"
	case "group":
		for _, g := range groups {
			if p.TargetID != nil && g.ID == *p.TargetID {
				return g.Name + " (group)"
			}
		}
		return "Group"
	case "device":
		return p.TargetSerial
	default:
		return "—"
	}
}
