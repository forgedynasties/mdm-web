package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"mdm/internal/apkmeta"
	"mdm/internal/db"
	"mdm/internal/product"

	"github.com/google/uuid"
)

func (h *Handler) CommandCreate(w http.ResponseWriter, r *http.Request) {
	// update_splash may carry a file upload (multipart); other types are urlencoded.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(uploadSizeLimit); err != nil {
			http.Error(w, "bad upload", http.StatusBadRequest)
			return
		}
	} else {
		r.ParseForm()
	}
	cmdType := r.FormValue("type")
	if cmdType == "" {
		cmdType = "install_apk"
	}

	// set_kiosk isn't a queued command — it writes kiosk config to the target set (like
	// the old bulk-kiosk action). Gate it directly (admin/dev/operator), not via the
	// command-role machinery the real commands use.
	if cmdType == "set_kiosk" {
		if role := h.role(r); !roleCanOperate(role) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
	} else if writeCommandAuthzError(w, h.authorizeCommand(h.role(r), cmdType)) {
		return
	}
	if cmdType == "shell" && !h.cfg.ShellEnabled() {
		http.Error(w, "Shell commands are disabled by an administrator.", http.StatusForbidden)
		return
	}

	// A "query" is a read-only diagnostic whose command text comes only from the
	// admin-curated catalog (chosen by query_id), never from the client. That's why
	// admin/dev/operator may issue it without raw-shell rights: whatever shell_cmd the
	// client might send is ignored — the payload is rebuilt from the catalog here.
	// The command is stored as type "query" and only translated to "shell" at the
	// device boundary (see deviceCommandType), so the dashboard shows it as a Query.
	var queryPayload json.RawMessage
	if cmdType == "query" {
		qid, _ := strconv.Atoi(r.FormValue("query_id"))
		q, err := h.db.GetDeviceQuery(r.Context(), qid)
		if err != nil || !q.Enabled {
			http.Error(w, "Unknown query", http.StatusBadRequest)
			return
		}
		b, _ := json.Marshal(map[string]any{"cmd": q.Command, "query_id": q.ID, "query": q.Label})
		queryPayload = json.RawMessage(b)
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if isDestructiveCmd(cmdType) && h.cfg.RequireReason() && reason == "" {
		http.Error(w, "A reason is required for this command.", http.StatusBadRequest)
		return
	}

	targetType := r.FormValue("target_type")
	if targetType != "all" && targetType != "devices" && targetType != "groups" && targetType != "scope" && targetType != "mixed" {
		http.Redirect(w, r, "/commands", http.StatusFound)
		return
	}

	// "set_kiosk" writes kiosk config to the resolved target devices immediately
	// (pushed on their next check-in), then returns — no command row is created.
	if cmdType == "set_kiosk" {
		h.applyKioskForTargets(w, r, targetType)
		return
	}

	// install_apk and uninstall accept MULTIPLE selections — one command is created
	// per chosen app / package (the command + client model is one-app-per-command).
	// Every other type carries a single payload.
	installURLs := dedupeStrings(formValues(r, "apk_url"))
	uninstallPkgs := dedupeStrings(formValues(r, "package"))
	if cmdType == "install_apk" && len(installURLs) == 0 {
		http.Redirect(w, r, "/commands", http.StatusFound)
		return
	}
	if cmdType == "uninstall" && len(uninstallPkgs) == 0 {
		http.Redirect(w, r, "/commands", http.StatusFound)
		return
	}
	if cmdType == "update_splash" {
		// An uploaded BMP/PNG/JPEG is wrapped into a splash.img server-side and
		// served from /splash-img/; its URL then drives the normal flow. A pasted
		// splash_url is the fallback when no file is uploaded.
		url, err := h.generateSplashFromUpload(r)
		if err != nil {
			http.Error(w, "splash upload failed: "+err.Error(), http.StatusBadRequest)
			return
		}
		if url != "" {
			r.Form.Set("splash_url", url)
		}
		if strings.TrimSpace(r.FormValue("splash_url")) == "" {
			http.Redirect(w, r, "/commands", http.StatusFound)
			return
		}
	}

	// Resolve the target device set once — every command we create shares it.
	var targetIDs []uuid.UUID
	switch targetType {
	case "all":
		// Snapshot: resolve all current device IDs so future devices are unaffected.
		ids, err := h.db.GetAllDeviceIDs(r.Context())
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		targetType = "devices"
		targetIDs = ids
	case "devices":
		serials := db.ParseSerials(r.FormValue("target_serials"))
		ids, err := h.db.GetDeviceIDsBySerials(r.Context(), serials)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		if max := h.cfg.MaxTargets(); max > 0 && len(ids) > max {
			http.Error(w, fmt.Sprintf("Too many target devices (%d); the configured limit is %d.", len(ids), max), http.StatusBadRequest)
			return
		}
		targetIDs = ids
	case "groups":
		var gids []uuid.UUID
		for _, gid := range r.Form["target_groups"] {
			if id, err := uuid.Parse(gid); err == nil {
				gids = append(gids, id)
			}
		}
		// Snapshot group membership to concrete device IDs NOW, like all/scope — a one-shot
		// command (install/reboot/…) targets the CURRENT members, so a device that joins the
		// group later doesn't pick up a stale command on its first connect, and the install
		// dedup (device-targeted) runs for group sends too.
		ids, err := h.db.GetDeviceIDsByGroupIDs(r.Context(), gids)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		if max := h.cfg.MaxTargets(); max > 0 && len(ids) > max {
			http.Error(w, fmt.Sprintf("Too many target devices (%d); the configured limit is %d.", len(ids), max), http.StatusBadRequest)
			return
		}
		targetType = "devices"
		targetIDs = ids
	case "scope":
		// Snapshot the scope-rail selection (collection + refine) to device IDs now.
		ids, err := h.resolveScopeDeviceIDs(r)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		if max := h.cfg.MaxTargets(); max > 0 && len(ids) > max {
			http.Error(w, fmt.Sprintf("Too many target devices (%d); the configured limit is %d.", len(ids), max), http.StatusBadRequest)
			return
		}
		targetType = "devices"
		targetIDs = ids
	case "mixed":
		// Restaurants + groups + serials in one send, de-duplicated by device.
		ids, err := h.resolveMixedDeviceIDs(r)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		if max := h.cfg.MaxTargets(); max > 0 && len(ids) > max {
			http.Error(w, fmt.Sprintf("Too many target devices (%d); the configured limit is %d.", len(ids), max), http.StatusBadRequest)
			return
		}
		targetType = "devices"
		targetIDs = ids
	}
	// Per-user access policy: drop the devices this operator may not send this
	// command to. Nothing left → refuse; some dropped → proceed and say so.
	if acc := h.access(r); !acc.unrestricted() {
		kept, dropped := acc.filterDevices(policyActionForCommand(cmdType), targetIDs)
		if len(kept) == 0 && len(targetIDs) > 0 {
			http.Error(w, "Your access policy does not allow this command on the selected devices.", http.StatusForbidden)
			return
		}
		if dropped > 0 {
			log.Printf("[access] %s: %s skipped %d device(s) outside their policy", h.currentUsername(r), cmdType, dropped)
		}
		targetIDs = kept
	}

	// Capability gating: a device whose agent cannot run this command is dropped
	// here (and named in the result) instead of receiving a command it would only
	// fail. Mirrors the impact preview so what was promised is what fires.
	unsupportedSkipped := 0
	unsupSkipped := map[string]bool{}
	if product.CommandNeeds(cmdType) != "" && len(targetIDs) > 0 {
		if devs, err := h.db.GetDevicesByIDs(r.Context(), targetIDs); err == nil {
			can := make(map[uuid.UUID]bool, len(devs))
			for _, d := range devs {
				if d.Supports(cmdType) {
					can[d.ID] = true
				} else {
					unsupSkipped[d.SerialNumber] = true
					unsupportedSkipped++
				}
			}
			kept := targetIDs[:0:0]
			for _, id := range targetIDs {
				if can[id] {
					kept = append(kept, id)
				}
			}
			if len(kept) == 0 {
				http.Error(w, fmt.Sprintf("None of the selected devices can run %s on their agent.", cmdTypeLabel(cmdType)), http.StatusBadRequest)
				return
			}
			if unsupportedSkipped > 0 {
				h.audit(r, "command.send.unsupported", cmdType, fmt.Sprintf("skipped %d device(s) whose agent cannot run it", unsupportedSkipped))
			}
			targetIDs = kept
		}
	}

	// Reject a command that resolves to no devices (e.g. "all" on an empty fleet, or
	// serials that match nothing) rather than inserting an orphan command with no
	// targets that no device will ever pick up.
	if len(targetIDs) == 0 {
		http.Error(w, "No matching target devices.", http.StatusBadRequest)
		return
	}

	// Build the per-command work items (apkURL + payload).
	type cmdItem struct {
		apkURL  string
		payload json.RawMessage
	}
	var items []cmdItem
	switch cmdType {
	case "install_apk":
		for _, u := range installURLs {
			items = append(items, cmdItem{apkURL: u})
		}
	case "uninstall":
		for _, p := range uninstallPkgs {
			b, _ := json.Marshal(map[string]string{"package": p})
			items = append(items, cmdItem{payload: json.RawMessage(b)})
		}
	case "query":
		items = []cmdItem{{payload: queryPayload}}
	case "app_update":
		// Fleet-wide agent update: every target gets the build this server hosts.
		// MDM-lite hosts pick their own APK from the app library, so a URL supplied
		// by the form still wins.
		if u := strings.TrimSpace(r.FormValue("apk_url")); u != "" {
			items = []cmdItem{{apkURL: u, payload: buildPayload(cmdType, r)}}
			break
		}
		// Targets can span agent kinds and signing variants, and each may only install
		// its own build — so this becomes one command per slot actually present in the
		// target set, rather than one URL for everybody.
		targets, err := h.db.GetDevicesByIDs(r.Context(), targetIDs)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		slots := map[string]bool{}
		for i := range targets {
			if targets[i].Supports("app_update") {
				slots[agentSlotFor(&targets[i])] = true
			}
		}
		for slot := range slots {
			url, pkg, version, _, sha, ok := h.agentUpdateTarget(r, slot)
			if !ok {
				continue
			}
			items = append(items, cmdItem{apkURL: url, payload: agentUpdatePayload(pkg, version, sha)})
		}
		if len(items) == 0 {
			http.Error(w, "No agent build is hosted for the targeted devices' agents and signing keys — publish one first.", http.StatusBadRequest)
			return
		}
	default:
		items = []cmdItem{{apkURL: strings.TrimSpace(r.FormValue("apk_url")), payload: buildPayload(cmdType, r)}}
	}

	// Create one command per item. For install_apk, drop target devices that already
	// have this exact APK in flight or already report its package (unless "Reinstall
	// anyway"); an app whose whole target set is skipped simply creates no command.
	reinstall := r.FormValue("reinstall") != ""
	skippedSet := unsupSkipped // starts with the devices whose agent cannot run the command
	var created []*db.Command

	// Pre-fetch APK size/ETag for every install URL CONCURRENTLY with a short bound, so a
	// multi-app install doesn't serialize N slow HEAD requests (that hung the install
	// popup for many seconds). Augment already degrades gracefully to no size/etag on a
	// slow/failed HEAD, so a laggy file host can't block command creation.
	augmentedPayload := map[string]json.RawMessage{}
	if cmdType == "install_apk" && len(items) > 0 {
		actx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, it := range items {
			u := it.apkURL
			wg.Add(1)
			go func(base json.RawMessage) {
				defer wg.Done()
				p := apkmeta.Augment(actx, u, base)
				mu.Lock()
				augmentedPayload[u] = p
				mu.Unlock()
			}(it.payload)
		}
		wg.Wait()
		cancel()
	}

	// Several apps in one send form one batch: every command carries the same
	// payload.batch id, and the history shows them as one "Install N apps" row.
	batchID := ""
	if len(items) > 1 {
		batchID = uuid.New().String()
	}
	for _, it := range items {
		ids := targetIDs
		if cmdType == "install_apk" && targetType == "devices" && len(ids) > 0 && !reinstall {
			skip := make(map[uuid.UUID]bool)
			inflight, err := h.db.DevicesWithPendingInstall(r.Context(), it.apkURL, ids, h.cfg.CommandExpiry())
			if err != nil {
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}
			for id := range inflight {
				skip[id] = true
			}
			installed, err := h.db.DevicesWithPackageInstalled(r.Context(), it.apkURL, ids)
			if err != nil {
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}
			for id := range installed {
				skip[id] = true
			}
			if len(skip) > 0 {
				fresh := make([]uuid.UUID, 0, len(ids))
				var skippedIDs []uuid.UUID
				for _, id := range ids {
					if skip[id] {
						skippedIDs = append(skippedIDs, id)
					} else {
						fresh = append(fresh, id)
					}
				}
				if devs, e := h.db.GetDevicesByIDs(r.Context(), skippedIDs); e == nil {
					for _, d := range devs {
						skippedSet[d.SerialNumber] = true
					}
				}
				ids = fresh
			}
		}
		if len(ids) == 0 {
			continue // every target already has / is installing this app
		}
		payload := it.payload
		if cmdType == "install_apk" {
			// Use the size/ETag fetched concurrently above (falls back to the base payload).
			if p, ok := augmentedPayload[it.apkURL]; ok {
				payload = p
			}
		}
		if batchID != "" {
			payload = withBatch(payload, batchID)
		}
		cmd, err := h.db.CreateCommandBy(r.Context(), cmdType, it.apkURL, payload, targetType, ids, h.currentUsername(r))
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		h.pushCommand(r.Context(), cmd, targetType, ids)
		created = append(created, cmd)
	}

	if len(created) == 0 {
		h.audit(r, "command.send.skip", cmdType, fmt.Sprintf("all %d target(s) already have or are installing the selected app(s)", len(targetIDs)))
		h.hxRedirect(w, r, "/commands?flash="+url.QueryEscape("All selected device(s) already have or are installing the selected app(s) — nothing queued.")+"&flash_type=info")
		return
	}

	detail := fmt.Sprintf("target=%s, devices=%d, commands=%d", targetType, len(targetIDs), len(created))
	if reason != "" {
		detail += ", reason=" + reason
	}
	if len(created) == 1 {
		detail += ", cmd=" + created[0].ID.String()
	}
	h.audit(r, "command.send", cmdType, detail)

	// Boot logo is applied from its own config page — return there (with status),
	// not to the command-detail view, and never into the command history flow.
	if cmdType == "update_splash" {
		h.hxRedirect(w, r, "/boot-logo?flash="+url.QueryEscape(fmt.Sprintf("Boot logo applied to %d device(s).", len(targetIDs)))+"&flash_type=success")
		return
	}

	skipped := make([]string, 0, len(skippedSet))
	for s := range skippedSet {
		skipped = append(skipped, s)
	}
	sort.Strings(skipped)

	// Device-page install (progress_view=1): don't navigate anywhere. Fire device-updated
	// so the device page's Applications drawer refreshes and shows the queued apps inline
	// as "Installing…" — the popup already closed client-side. No separate progress/history
	// page; the drawer IS the combined live view.
	if cmdType == "install_apk" && r.FormValue("progress_view") == "1" {
		w.Header().Set("HX-Trigger", "device-updated")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// One command → its detail view (with any skip note). Multiple → back to Actions
	// with a summary, since there's no single command to open.
	if len(created) == 1 {
		dest := "/commands/" + created[0].ID.String()
		if n := len(skipped); n > 0 {
			msg := fmt.Sprintf("Sent to %d device(s). Skipped %d that already have or are installing this app: %s", len(targetIDs), n, strings.Join(skipped, ", "))
			dest += "?flash=" + url.QueryEscape(msg) + "&flash_type=info"
		}
		h.hxRedirect(w, r, dest)
		return
	}
	msg := fmt.Sprintf("Queued %d commands to %d device(s).", len(created), len(targetIDs))
	if n := len(skipped); n > 0 {
		msg += fmt.Sprintf(" Skipped %d device(s) that already had one of the apps.", n)
	}
	h.hxRedirect(w, r, "/commands?flash="+url.QueryEscape(msg)+"&flash_type=success")
}

// pkgNameSet is the set of package names a device currently reports installed, so the
// install picker can hide apps that are already on the device.
func pkgNameSet(pkgs []db.DevicePackage) map[string]bool {
	m := make(map[string]bool, len(pkgs))
	for _, p := range pkgs {
		if p.PackageName != "" {
			m[p.PackageName] = true
		}
	}
	return m
}

// apkURLToName maps each library app's apk_url to its display name, so the command
// history can label an install with the app name instead of the raw APK URL.
func apkURLToName(apps []db.App) map[string]string {
	m := make(map[string]string, len(apps))
	for _, a := range apps {
		if a.ApkURL != "" && a.Name != "" {
			m[a.ApkURL] = a.Name
		}
	}
	return m
}

// formValues returns the trimmed, non-empty values of a repeated form field.
func formValues(r *http.Request, key string) []string {
	var out []string
	for _, v := range r.Form[key] {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// dedupeStrings preserves order while dropping duplicate values.
func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// isDestructiveCmd marks command types that change device state in a way that
// warrants a reason when the RequireReason setting is on.
func isDestructiveCmd(t string) bool {
	return t == "reboot" || t == "ota" || t == "update_splash" || t == "wipe"
}

func buildPayload(cmdType string, r *http.Request) json.RawMessage {
	switch cmdType {
	case "collect_logs":
		return collectLogsPayload() // fixed; nothing from the form

	case "shell":
		cmd := strings.TrimSpace(r.FormValue("shell_cmd"))
		b, _ := json.Marshal(map[string]string{"cmd": cmd})
		return json.RawMessage(b)
	case "uninstall":
		b, _ := json.Marshal(map[string]string{"package": strings.TrimSpace(r.FormValue("package"))})
		return json.RawMessage(b)
	case "adb_tcp":
		// port 0 turns wireless adb off; hours is how long it stays on (0 = until told).
		m := map[string]int{"port": 5555, "hours": 24}
		if p, err := strconv.Atoi(strings.TrimSpace(r.FormValue("port"))); err == nil {
			m["port"] = p
		}
		if h, err := strconv.Atoi(strings.TrimSpace(r.FormValue("hours"))); err == nil {
			m["hours"] = h
		}
		b, _ := json.Marshal(m)
		return json.RawMessage(b)
	case "mic_gain_set":
		// value = TX_DEC Volume to enforce (e.g. 102); empty clears enforcement.
		b, _ := json.Marshal(map[string]string{"value": strings.TrimSpace(r.FormValue("value"))})
		return json.RawMessage(b)
	case "update_splash":
		// Client downloads url (a splash.img: 0x4000 zero filler + BMP), validates
		// the BMP signature at 0x4000 and (if given) partition_size, then stages +
		// triggers the init broker.
		m := map[string]any{"url": strings.TrimSpace(r.FormValue("splash_url"))}
		if ps, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("partition_size")), 10, 64); err == nil && ps > 0 {
			m["partition_size"] = ps
		}
		b, _ := json.Marshal(m)
		return json.RawMessage(b)
	default:
		return json.RawMessage("{}")
	}
}
