package dashboard

import (
	"context"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"mdm/internal/apkmeta"
	"mdm/internal/db"

	"github.com/google/uuid"
)

// ── Groups ────────────────────────────────────────────────────────────────────

func (h *Handler) GroupNew(w http.ResponseWriter, r *http.Request) {
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{}, 0, 500, "", "")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices = h.access(r).keepVisible(devices) // only devices the user can see
	groups, _ := h.db.ListGroups(r.Context())
	productions, _ := h.db.ListProductions(r.Context(), h.connectedSlice())
	builds, _ := h.db.GetDistinctBuildIDs(r.Context())
	connected := h.hub.ConnectedIDsForDisplay()
	online := make(map[uuid.UUID]bool, len(connected))
	for id := range connected {
		online[id] = true
	}

	h.render(w, r, "group_form.html", map[string]any{
		"ScopesJSON":  h.pickerScopesJSON(r),
		"Title":       "New Group",
		"Devices":     devices,
		"Online":      online,
		"Groups":      groups,
		"Productions": productions,
		"Builds":      builds,
	})
}

func (h *Handler) GroupNewDevices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	filter := db.DeviceFilter{
		Search:  q,
		Online:  r.URL.Query().Get("status"),
		BuildID: r.URL.Query().Get("build"),
		Battery: r.URL.Query().Get("battery"),
	}
	var groupID uuid.UUID
	if gid := r.URL.Query().Get("group"); gid != "" {
		if parsed, err := uuid.Parse(gid); err == nil {
			groupID = parsed
		}
	}
	filter.GroupID = groupID
	var productionID uuid.UUID
	if pid := r.URL.Query().Get("production"); pid != "" {
		if parsed, err := uuid.Parse(pid); err == nil {
			productionID = parsed
		}
	}
	filter.ProductionID = productionID

	devices, err := h.db.ListDevices(r.Context(), filter, 0, 500, "", "")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices = h.access(r).keepVisible(devices) // only devices the user can see
	h.renderCachedHTML(w, r, "device-picker-rows", map[string]any{
		"Devices":  devices,
		"Query":    q,
		"Relocate": false,
	})
}

func (h *Handler) GroupList(w http.ResponseWriter, r *http.Request) {
	// Groups is its own page again, beside Venues: the venues section's "Groups" link
	// landed on the fleet page instead (user, 13 Oct).
	groups, err := h.db.ListGroups(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Per-group health scores for the card grid; missing entries render as "—".
	health := make(map[uuid.UUID]db.GroupHealth)
	if ghs, err := h.db.GetGroupHealth(r.Context(), h.connectedSlice()); err == nil {
		for _, gh := range ghs {
			health[gh.GroupID] = gh
		}
	}
	h.render(w, r, "groups.html", map[string]any{
		"Title":  "Groups",
		"Groups": groups,
		"Health": health,
	})
}

// onlineMap returns a device-id → true map of currently WS-connected devices,
// for the shared device-cards roster partial.
func (h *Handler) onlineMap() map[uuid.UUID]bool {
	connected := h.hub.ConnectedIDsForDisplay()
	m := make(map[uuid.UUID]bool, len(connected))
	for id := range connected {
		m[id] = true
	}
	return m
}

func (h *Handler) GroupDetail(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	g, err := h.db.GetGroup(r.Context(), id)
	if err != nil {
		http.Error(w, "Group not found", http.StatusNotFound)
		return
	}
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{GroupID: id}, 0, 1000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices = h.access(r).keepVisible(devices)
	data := map[string]any{
		"Title":               g.Name,
		"Group":               g,
		"Devices":             devices,
		"Online":              h.onlineMap(),
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
	}
	data["ScopesJSON"] = h.pickerScopesJSON(r)
	// Kiosk belongs on the venue: "the tablets at this restaurant run the ordering app"
	// is the sentence the business says. See venue_kiosk.go.
	data["Kiosk"] = h.venueKiosk(r, id)
	if r.URL.Query().Get("partial") == "kpis" { // the count cards, refreshed on group-updated
		_ = h.tmpl.ExecuteTemplate(w, "group-kpis", h.withRole(r, data))
		return
	}
	h.render(w, r, "group_detail.html", data)
}

// GroupDevicesModal serves just the "add/remove devices" card + members list —
// the fleet-page kebab menu's "Add/remove devices" popup loads this via htmx
// instead of navigating to the full group page.
func (h *Handler) GroupDevicesModal(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	g, err := h.db.GetGroup(r.Context(), id)
	if err != nil {
		http.Error(w, "Group not found", http.StatusNotFound)
		return
	}
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{GroupID: id}, 0, 1000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices = h.access(r).keepVisible(devices)
	h.render(w, r, "group-devices-modal", map[string]any{
		"Group":               g,
		"Devices":             devices,
		"Online":              h.onlineMap(),
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		"Lean":                true,
	})
}

// parseSerialsField splits the "serials" form field(s) into individual,
// trimmed, non-empty serial numbers. The group forms submit selected devices
// as a single newline-separated <textarea name="serials">, so the raw form
// value is one multi-line blob — it must be split, not used as-is.
//
// Separators are any whitespace, comma or semicolon — the same set picker.js
// splits a pasted list on (/[\s,;]+/). They used to differ: the server split on
// newlines and commas only, so a list pasted space- or tab-separated (out of a
// spreadsheet cell, a chat line, a shell loop) arrived as ONE token, matched no
// device, and the add silently did nothing. Duplicates are dropped, keeping the
// first spelling, so a pasted list with repeats reports an honest count.
func parseSerialsField(values []string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, v := range values {
		for _, part := range strings.FieldsFunc(v, func(r rune) bool {
			return unicode.IsSpace(r) || r == ',' || r == ';'
		}) {
			part = cleanSerialToken(part)
			if part == "" {
				continue
			}
			k := strings.ToUpper(part)
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, part)
		}
	}
	return out
}

// cleanSerialToken strips list decoration people paste along with a serial —
// "-AT070AABU00333", "• AT070…", "1. AT070…", a trailing comma, quotes — from the
// ends only. Never from inside the token: this also parses package names (com.a.b).
func cleanSerialToken(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, " \t-–—*•·>#\"'`([{")
	// Numbered lists ("1. AT070…", "2) AT070…"). Only when a letter follows, so a
	// dotted value like "1.2.3" is left alone.
	if i := strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }); i > 0 && i < len(s) {
		if rest := strings.TrimSpace(s[i+1:]); (s[i] == '.' || s[i] == ')') && rest != "" && unicode.IsLetter(rune(rest[0])) {
			s = rest
		}
	}
	return strings.TrimRight(strings.TrimSpace(s), " \t,;:.\"'`)]}")
}

func (h *Handler) GroupCreate(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Redirect(w, r, "/groups", http.StatusFound)
		return
	}
	group, err := h.db.CreateGroup(r.Context(), name)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	serials := h.keepVisibleSerials(r, parseSerialsField(r.Form["serials"]))
	if len(serials) > 0 {
		if _, err := h.db.AddDevicesToGroup(r.Context(), serials, group.ID); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/groups/"+group.ID.String(), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/groups", http.StatusFound)
}

func (h *Handler) GroupDelete(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteGroup(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// A deleted group can't scope the roster any more, so fall back to all devices.
	localRedirect(w, r, "/devices")
}

// localRedirect sends the user to the form's "redirect" target when it is a safe
// same-site path (a single leading "/"), else to fallback. Lets the fleet toolbar's
// inline edits return to the exact roster view they were launched from, while still
// serving the old callers (group page) that post no redirect.
func localRedirect(w http.ResponseWriter, r *http.Request, fallback string) {
	if dest := r.FormValue("redirect"); strings.HasPrefix(dest, "/") && !strings.HasPrefix(dest, "//") {
		http.Redirect(w, r, dest, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fallback, http.StatusSeeOther)
}

// GroupUpdate renames a group. A group has only a name, so this doubles as its full "edit".
func (h *Handler) GroupUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "Name required", http.StatusBadRequest)
		return
	}
	if err := h.db.RenameGroup(r.Context(), id, name); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "group.rename", id.String(), name)
	localRedirect(w, r, "/devices?group="+id.String())
}

func (h *Handler) GroupAddDevice(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	// Accept single serial_number (legacy) or multi-line serials textarea
	var serials []string
	if s := strings.TrimSpace(r.FormValue("serial_number")); s != "" {
		serials = append(serials, s)
	}
	serials = append(serials, parseSerialsField(r.Form["serials"])...)
	asked := len(serials)
	// Only devices the user can see: moving a device changes whose rules cover it.
	serials = h.keepVisibleSerials(r, serials)
	if len(serials) == 0 {
		h.groupMembershipDone(w, r, id, "Nothing to add", "warn")
		return
	}
	added, err := h.db.AddDevicesToGroup(r.Context(), serials, id)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if ids, err := h.db.GetDeviceIDsBySerials(r.Context(), serials); err == nil {
		for _, did := range ids {
			h.hub.PublishDeviceUpdate(did)
		}
	}
	// Say what happened. An add can legitimately move nothing — serials that are not
	// in the fleet, or devices already in this group — and the members list refreshing
	// unchanged used to be the only hint, which reads as a broken button.
	msg, typ := h.groupChangeMessage(r.Context(), "Added", "to this group", serials, asked, added)
	// HX request (the group page's add form): 204 + toast + group-updated so the
	// members list refreshes in place. Plain POST (no JS) redirects back with a flash.
	h.groupMembershipDone(w, r, id, msg, typ)
}

// groupChangeMessage builds the operator-facing summary of a group add/remove:
// how many rows actually changed, and — when fewer than asked — which of the pasted
// serials are not devices on this server, named in the message so the operator can
// fix the list instead of guessing. verb is "Added"/"Removed", where reads as
// "… to this group"/"… from this group".
func (h *Handler) groupChangeMessage(ctx context.Context, verb, where string, serials []string, asked, changed int) (string, string) {
	var unknown []string
	if changed < len(serials) {
		if known, err := h.db.SerialIDs(ctx, serials); err == nil {
			upper := make(map[string]struct{}, len(known))
			for s := range known {
				upper[strings.ToUpper(s)] = struct{}{}
			}
			for _, s := range serials {
				if _, ok := upper[strings.ToUpper(s)]; !ok {
					unknown = append(unknown, s)
				}
			}
		}
	}
	typ := "success"
	if changed == 0 {
		typ = "warn"
	}
	msg := fmt.Sprintf("%s %d device%s %s", verb, changed, plural(changed), where)
	switch {
	case len(unknown) > 0 && len(unknown) <= 6:
		msg += fmt.Sprintf(" · not on this server: %s", strings.Join(unknown, ", "))
	case len(unknown) > 6:
		msg += fmt.Sprintf(" · %d serial%s not on this server (%s, …)", len(unknown), plural(len(unknown)), strings.Join(unknown[:3], ", "))
	case changed == 0 && asked > 0:
		msg += " · already up to date"
	}
	if hidden := asked - len(serials); hidden > 0 {
		msg += fmt.Sprintf(" · %d outside your visibility", hidden)
	}
	return msg, typ
}

// groupMembershipDone answers a group add/remove: a toast plus the "group-updated"
// event that refreshes the members card and the KPIs in place.
func (h *Handler) groupMembershipDone(w http.ResponseWriter, r *http.Request, id uuid.UUID, msg, typ string) {
	h.hxDoneToastEvents(w, r, "/groups/"+id.String()+"?flash="+url.QueryEscape(msg)+"&flash_type="+typ, msg, typ, "group-updated")
}

func (h *Handler) GroupDeviceSearch(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	filter := db.DeviceFilter{
		Search:              query,
		ExcludeGroupID:      id,
		Online:              r.URL.Query().Get("status"),
		Battery:             r.URL.Query().Get("battery"),
		ActiveThresholdSecs: h.cfg.CheckinInterval() * 3,
	}
	devices, err := h.db.ListDevices(r.Context(), filter, 0, 60, "", "")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices = h.access(r).keepVisible(devices) // only devices the user can see
	h.tmpl.ExecuteTemplate(w, "device-picker-rows", map[string]any{
		"Query":    query,
		"Devices":  devices,
		"Relocate": false,
	})
}

func (h *Handler) GroupRemoveDevice(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	serial := r.PathValue("serial")
	if len(h.keepVisibleSerials(r, []string{serial})) == 0 {
		http.NotFound(w, r)
		return
	}
	if err := h.db.RemoveDeviceFromGroup(r.Context(), serial, id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if device, err := h.db.GetDevice(r.Context(), serial); err == nil {
		h.hub.PublishDeviceUpdate(device.ID)
	}
	h.hxDone(w, r, "/groups/"+id.String(), "group-updated")
}

// GroupBulkRemoveDevice removes several devices from a group in one request, so the
// members list can drop a multi-selection at once instead of one device at a time
// (FW-2026-000024). Serials arrive in the multi-valued "serials" field.
func (h *Handler) GroupBulkRemoveDevice(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	parsed := parseSerialsField(r.Form["serials"])
	asked := len(parsed)
	serials := h.keepVisibleSerials(r, parsed)
	if len(serials) == 0 {
		h.groupMembershipDone(w, r, id, "Nothing to remove", "warn")
		return
	}
	removed, err := h.db.RemoveDevicesFromGroup(r.Context(), serials, id)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if ids, err := h.db.GetDeviceIDsBySerials(r.Context(), serials); err == nil {
		for _, did := range ids {
			h.hub.PublishDeviceUpdate(did)
		}
	}
	msg, typ := h.groupChangeMessage(r.Context(), "Removed", "from this group", serials, asked, removed)
	h.groupMembershipDone(w, r, id, msg, typ)
}

// GroupMembers renders just the members card of a group for an in-place htmx
// refresh (fired by "group-updated" after a device is removed) — no page reload.
func (h *Handler) GroupMembers(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	g, err := h.db.GetGroup(r.Context(), id)
	if err != nil {
		http.Error(w, "Group not found", http.StatusNotFound)
		return
	}
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{GroupID: id}, 0, 1000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices = h.access(r).keepVisible(devices)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.tmpl.ExecuteTemplate(w, "group-members", h.withRole(r, map[string]any{
		"Group":               g,
		"Devices":             devices,
		"Online":              h.onlineMap(),
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
		// Set by the fleet-page devices popup's own hx-get URL (?lean=1) so this
		// live in-place refresh (fired on "group-updated") keeps the same lean
		// rendering the popup opened with, instead of reverting to the full
		// fleet-card list the standalone group page uses.
		"Lean": r.URL.Query().Get("lean") == "1",
	}))
}

func (h *Handler) GroupCommandCreate(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	cmdType := r.FormValue("type")
	if cmdType == "" {
		cmdType = "install_apk"
	}
	if writeCommandAuthzError(w, h.authorizeCommand(h.role(r), cmdType)) {
		return
	}
	if cmdType == "shell" && !h.cfg.ShellEnabled() {
		http.Error(w, "Shell commands are disabled by an administrator.", http.StatusForbidden)
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if isDestructiveCmd(cmdType) && h.cfg.RequireReason() && reason == "" {
		http.Error(w, "A reason is required for this command.", http.StatusBadRequest)
		return
	}

	apkURL := strings.TrimSpace(r.FormValue("apk_url"))
	if cmdType == "install_apk" && apkURL == "" {
		http.Redirect(w, r, "/groups/"+id.String(), http.StatusFound)
		return
	}

	payload := buildPayload(cmdType, r)
	if cmdType == "install_apk" {
		// Capture APK size + ETag so the device can verify/resume the download.
		payload = apkmeta.Augment(r.Context(), apkURL, payload)
	}

	gTargets, gType, ok := h.enforceCommandTargets(w, r, policyActionForCommand(cmdType), "groups", []uuid.UUID{id})
	if !ok {
		return
	}
	cmd, err := h.db.CreateCommandBy(r.Context(), cmdType, apkURL, payload, gType, gTargets, h.currentUsername(r))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.pushCommand(r.Context(), cmd, gType, gTargets)
	detail := "target=groups, group=" + id.String()
	if reason != "" {
		detail += ", reason=" + reason
	}
	detail += ", cmd=" + cmd.ID.String()
	h.audit(r, "command.send", cmdType, detail)
	http.Redirect(w, r, "/commands/"+cmd.ID.String(), http.StatusFound)
}
