package dashboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// CommandList renders /jobs and the history table's live fragment.
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
	// Jobs: in flight, needs attention, and what has run.
	data["JobsView"] = true
	h.jobsJ1Data(r, data)
	// The count cards refresh on their own, like the list (J1, 21 Oct 2026).
	if r.URL.Query().Get("partial") == "jobscards" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		h.tmpl.ExecuteTemplate(w, "jobs-cards", h.withRole(r, data))
		return
	}
	data["St"] = h.movedSettings(r) // Action governance lives here since S1 (24 Oct 2026)
	h.render(w, r, "jobs.html", data)
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

	groupNames := make(map[uuid.UUID]string, len(groups))
	for _, g := range groups {
		groupNames[g.ID] = g.Name
	}

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

	if acc := h.access(r); acc.hidesDevices() {
		groups = h.visibleGroupList(r, groups)
	}

	return map[string]any{
		"Title":         "Jobs",
		"Commands":      cmds,
		"Attention":     attnShown,
		"InProgress":    progShown,
		"Completed":     doneShown,
		"AttnAll":       attn,
		"ProgAll":       prog,
		"DoneAll":       doneAll,
		"AttnCount":     len(attn),
		"ProgCount":     len(prog),
		"DoneCount":     len(doneAll),
		"Groups":        groups,
		"Productions":   in.productions,
		"Builds":        in.builds,
		"Apps":          apps,
		"AppNames":      apkURLToName(apps),
		"FleetPackages": in.fleetPackages,
		"Summaries":     summaries,
		"Batches":       batches,
		"Clusters":      clusters,
		"AppIcons":      apkURLToIcon(apps),
		"PkgIcons":      pkgToIcon(apps),
		"TargetSerials": targetSerials,
		"RequireReason": h.cfg.RequireReason(),
	}, nil
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
		"Timelines": h.commandTimelines(r.Context(), cmd.ID, deliveries),
		"Stats":     computeDeliveryStats(deliveries),
		"From":      from,
		"CanResend": h.commandTypeAllowed(h.role(r), cmd.Type),
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
	// Marked lost or lifted: the venue's other devices start or stop listening for it.
	if cmd.Type == "lost_mode" && targetType == "devices" {
		defer h.nudgeLostVenue(ctx, targetIDs)
	}
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
