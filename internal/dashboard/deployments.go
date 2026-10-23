package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	_ "image/gif"
	_ "image/jpeg"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"mdm/internal/db"
	"mdm/internal/ota"
	"mdm/internal/product"

	"github.com/google/uuid"
)

// NewUpdatePage is the standalone "push an update" screen reached from the Updates
// page's "+ New Update" button. Step 1: pick a publishable release. Step 2 (once
// ?release= is set): pick the specific devices, reboot and delivery mode, then POST
// to /updates (DeployCreate). Device selection is scoped to the release's product.
func (h *Handler) NewUpdatePage(w http.ResponseWriter, r *http.Request) {
	deployable, _ := h.db.ListDeployableReleases(r.Context())
	deployable = visibleReleases(h.role(r), deployable)
	// The product is chosen first: a release only ever targets one product, and the
	// device list is scoped to it, so picking the hardware narrows both lists before
	// a version is even named. ?product= carries the choice; a chosen release always
	// wins, since its own product is the truth.
	selProduct := product.Normalize(r.URL.Query().Get("product"))
	if r.URL.Query().Get("product") == "" {
		selProduct = ""
	}

	// Arriving from the fleet page with a selection (?serials=): an OTA targets one
	// product of our own firmware, so the selection must be MDM Firmware devices of a
	// single model — that model is then preselected. Anything else goes back to the
	// fleet with the reason (covers "select all N matching", which the page can't check).
	if raw := strings.TrimSpace(r.URL.Query().Get("serials")); raw != "" && r.URL.Query().Get("product") == "" && r.URL.Query().Get("release") == "" {
		var serials []string
		for _, sn := range strings.Split(raw, ",") {
			if sn = strings.TrimSpace(sn); sn != "" {
				serials = append(serials, sn)
			}
		}
		if ids, err := h.db.GetDeviceIDsBySerials(r.Context(), serials); err == nil && len(ids) > 0 {
			if devs, err := h.db.GetDevicesByIDs(r.Context(), ids); err == nil {
				models := map[string]bool{}
				notFirmware := 0
				for _, d := range devs {
					if d.IsDPC() {
						notFirmware++
						continue
					}
					models[d.ProductKey()] = true
				}
				var why string
				switch {
				case notFirmware > 0:
					why = fmt.Sprintf("Push update is only for Firmware MDM devices — %d of the selected are Standard MDM / MDM Lite.", notFirmware)
				case len(models) > 1:
					var names []string
					for k := range models {
						names = append(names, productLabel(k))
					}
					sort.Strings(names)
					why = "Select devices of one model to push an update (selected: " + strings.Join(names, ", ") + ")."
				}
				if why != "" {
					http.Redirect(w, r, "/devices?flash="+url.QueryEscape(why)+"&flash_type=error", http.StatusFound)
					return
				}
				for k := range models {
					selProduct = k
				}
			}
		}
	}

	// Which products actually have something to push, so the picker never offers a
	// dead end. ListDeployableReleases doesn't carry the product, so resolve each.
	prodOf := map[int]string{}
	if all, err := h.db.ListReleases(r.Context()); err == nil {
		for _, rel := range all {
			prodOf[rel.ID] = product.Normalize(rel.Product)
		}
	}
	var products []product.Product
	seen := map[string]bool{}
	scoped := deployable[:0]
	for _, rel := range deployable {
		pk := prodOf[rel.ID]
		if !seen[pk] {
			seen[pk] = true
			p, _ := product.Resolve(pk)
			products = append(products, p)
		}
		if selProduct == "" || pk == selProduct {
			rel.Product = pk
			scoped = append(scoped, rel)
		}
	}
	sort.SliceStable(products, func(i, j int) bool { return products[i].Label < products[j].Label })

	data := map[string]any{
		"Title":               "New Update",
		"Releases":            scoped,
		"PushProducts":        products,
		"SelectedProduct":     selProduct,
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
	}
	if raw := r.URL.Query().Get("serials"); raw != "" {
		data["PreSerials"] = parseSerialsField([]string{raw})
	}
	// Groups and restaurants power the target selector (deploy to a whole group/venue,
	// mirroring the Actions target picker). resolveEligibleDevices resolves them server-side.
	var groupsList []db.Group
	if groups, err := h.db.ListGroups(r.Context()); err == nil {
		data["Groups"] = groups
		groupsList = groups
	}
	if rests, err := h.db.ListRestaurants(r.Context()); err == nil {
		data["Restaurants"] = rests
	}

	// Step 2 only renders once a release is chosen.
	if relRaw := strings.TrimSpace(r.URL.Query().Get("release")); relRaw != "" {
		if relID, err := strconv.Atoi(relRaw); err == nil {
			if rel, err := h.db.GetRelease(r.Context(), relID); err == nil {
				data["Release"] = rel

				// Delivery relevance: forcing full only makes sense when the release has a
				// full image; smart-vs-full only differs when an incremental also exists.
				pkgs, _ := h.db.ListPackagesByRelease(r.Context(), relID)
				hasFull, hasIncremental, fullWipes := false, false, false
				sourceBuilds := map[string]bool{}
				for _, p := range pkgs {
					if p.Status != "active" {
						continue
					}
					if p.Type == "full" {
						hasFull = true
						fullWipes = fullWipes || p.Wipe
					} else {
						hasIncremental = true
						if p.SourceBuildID != "" {
							sourceBuilds[p.SourceBuildID] = true
						}
					}
				}
				data["HasFull"] = hasFull
				data["HasIncremental"] = hasIncremental
				data["SourceBuilds"] = sourceBuilds
				data["FullWipes"] = fullWipes

				// Every device of the release's product; the template marks devices already
				// on this build as up to date and ones mid-update as updating.
				pushDevices, _ := h.db.ListDevices(r.Context(), db.DeviceFilter{Product: rel.Product}, 0, 500, "serial", "asc")
				if acc := h.access(r); !acc.unrestricted() {
					kept := pushDevices[:0:0]
					for _, d := range pushDevices {
						if acc.canDevice("ota", d.ID) {
							kept = append(kept, d)
						}
					}
					pushDevices = kept
				}

				updating, _ := h.db.SerialsUpdating(r.Context())
				data["DevicesUpdating"] = updating
				data["OTAUnsupported"] = h.otaUnsupportedForDevices(r.Context(), pushDevices)
				blockedNewer, _ := h.db.SerialsOnNewerRelease(r.Context(), relID)
				data["DevicesBlocked"] = blockedNewer

				// Eligible-to-push devices first (same "blocked" test the template applies
				// per row: up to date / updating / on a newer release / no artifact for
				// this build), so an operator scanning the list isn't scrolling past a
				// long run of greyed-out rows before reaching anything they can select.
				// Stable sort keeps the existing serial-asc order within each group.
				sort.SliceStable(pushDevices, func(i, j int) bool {
					blockedFor := func(dv db.Device) bool {
						if dv.BuildID == rel.Version {
							return true
						}
						if updating[dv.SerialNumber] != "" || blockedNewer[dv.SerialNumber] != "" {
							return true
						}
						return !hasFull && !sourceBuilds[dv.BuildID]
					}
					return !blockedFor(pushDevices[i]) && blockedFor(pushDevices[j])
				})
				data["PushDevices"] = pushDevices
				data["ScopesJSON"] = h.pickerScopesJSON(r)
				// ?serials= (the fleet selection panel's "Push update") starts the
				// picker with those devices already chosen.
				data["PreSerials"] = parseSerialsField([]string{r.URL.Query().Get("serials")})

				// Group/venue → member-serial maps (restricted to this push list) so the
				// target rail can tick a collection's devices in the list below and float
				// them to the top, client-side. Only serials present here are included, so
				// off-product/ineligible members are naturally excluded.
				idToSerial := make(map[uuid.UUID]string, len(pushDevices))
				for _, dv := range pushDevices {
					idToSerial[dv.ID] = dv.SerialNumber
				}
				groupMembers := map[string][]string{}
				groupIDs := make([]uuid.UUID, len(groupsList))
				for i, g := range groupsList {
					groupIDs[i] = g.ID
				}
				if byGroup, gerr := h.db.GetGroupDeviceIDsBatch(r.Context(), groupIDs); gerr == nil {
					for _, g := range groupsList {
						var serials []string
						for _, id := range byGroup[g.ID] {
							if s, ok := idToSerial[id]; ok {
								serials = append(serials, s)
							}
						}
						if len(serials) > 0 {
							groupMembers[g.ID.String()] = serials
						}
					}
				}
				restMembers := map[string][]string{}
				for _, dv := range pushDevices {
					if dv.RestaurantID != nil {
						rid := dv.RestaurantID.String()
						restMembers[rid] = append(restMembers[rid], dv.SerialNumber)
					}
				}
				gmJSON, _ := json.Marshal(groupMembers)
				rmJSON, _ := json.Marshal(restMembers)
				data["PushGroupMembers"] = template.JS(gmJSON)
				data["PushRestMembers"] = template.JS(rmJSON)
			}
		}
	}

	h.render(w, r, "updates_new.html", data)
}

// ReleaseDeploy deploys a whole release; the per-device artifact (full vs
// incremental) is chosen at resolve time. Only published releases can deploy.
func (h *Handler) ReleaseDeploy(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	h.deployRelease(w, r, relID)
}

// DeployCreate is the global Updates-hub deploy entry point: identical to
// ReleaseDeploy except the release is chosen in the composer (a form field) rather
// than taken from the URL path.
func (h *Handler) DeployCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	relID, err := strconv.Atoi(strings.TrimSpace(r.FormValue("release_id")))
	if err != nil {
		http.Error(w, "Choose a release to deploy.", http.StatusBadRequest)
		return
	}
	h.deployRelease(w, r, relID)
}

// deployRelease validates the release is publishable (published + an active package)
// and fans the update out to the resolved target devices, then redirects to the new
// deployment's detail page. Shared by ReleaseDeploy (path) and DeployCreate (form).
func (h *Handler) deployRelease(w http.ResponseWriter, r *http.Request, relID int) {
	rel, err := h.db.GetRelease(r.Context(), relID)
	if err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	// A dev release cannot be pushed by someone who cannot even see it.
	if rel.IsDev && !roleSeesDev(h.role(r)) {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	if rel.Status != "published" {
		http.Error(w, "Release must be published before it can be deployed.", http.StatusBadRequest)
		return
	}
	// A push needs at least one active package. A full image covers any device;
	// an incremental-only release is still pushable — the per-device resolver
	// (ResolveUpdateForDevice) hands each incremental only to devices whose
	// current build matches its source_build_id, and skips the rest.
	pkgs, _ := h.db.ListPackagesByRelease(r.Context(), relID)
	hasActive, hasFull, fullWipes := false, false, false
	for _, p := range pkgs {
		if p.Status == "active" {
			hasActive = true
			if p.Type == "full" {
				hasFull = true
				fullWipes = fullWipes || p.Wipe
			}
		}
	}
	if !hasActive {
		http.Error(w, "Add an OTA package to this release before pushing.", http.StatusBadRequest)
		return
	}
	r.ParseForm()

	rebootBehavior := r.FormValue("reboot_behavior")
	if rebootBehavior == "" {
		rebootBehavior = "immediate"
	}
	scheduledTime := parseScheduledUTC(r.FormValue("scheduled_time"), rebootBehavior)

	// Delivery: "smart" (default) lets the resolver pick an incremental per device where
	// its current build matches, falling back to full. "full" pins every target to the
	// full image. Only meaningful when the release actually has a full package.
	forceFull := r.FormValue("delivery") == "full"
	if forceFull && !hasFull {
		http.Error(w, "This release has no full image — add one before forcing full delivery.", http.StatusBadRequest)
		return
	}
	// A full image built with --wipe factory-resets every device it lands on. It is only
	// handed out — as the fallback for devices with no incremental, or forced — when the
	// deployment says so explicitly.
	allowWipe := fullWipes && r.FormValue("allow_wipe") == "1"
	if forceFull && fullWipes && !allowWipe {
		http.Error(w, "This release's full image wipes the device (factory reset). Tick \"Allow factory reset\" to force it.", http.StatusBadRequest)
		return
	}

	// Resolve eligibility BEFORE creating the deployment row: a push that resolves to
	// zero eligible devices (a double-submitted push where a second request finds
	// everything already targeted/updated by the first, or simply selecting devices
	// that turn out ineligible) must fail with a clear reason instead of silently
	// creating and redirecting to an empty deployment record.
	eligible, err := h.resolveEligibleDevices(r, rel.Product)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Drop devices already on a same-or-newer release — OTAing an older release onto them
	// is not allowed (they'd sit pending forever; the resolver would refuse to serve it).
	eligible, err = h.db.RemoveDowngradeTargets(r.Context(), relID, eligible)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Drop devices with no applicable artifact (incremental-only release + device not on a
	// source build) — the resolver could never serve them, so they'd strand as pending.
	eligible, err = h.db.RemoveInapplicableTargets(r.Context(), relID, eligible, allowWipe)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Split by transport rather than dropping: a build that cannot apply an MDM OTA
	// still updates, over the legacy otautil path. Both halves belong to one rollout.
	eligible, legacyIDs := h.splitOTACapable(r.Context(), eligible)
	legacySerials, err := h.db.SerialsForDeviceIDs(r.Context(), legacyIDs)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Serials the picker offered that are not in the fleet at all: otautil devices,
	// which only ever had the legacy path. They come straight from the form.
	legacySerials = append(legacySerials, h.legacyOnlySerials(r, rel)...)
	legacySerials = dedupeStrings(legacySerials)

	if len(eligible) == 0 && len(legacySerials) == 0 {
		http.Error(w, "No eligible devices — everything selected is already on this build or newer, has no applicable package, or is mid-update on another deployment.", http.StatusBadRequest)
		return
	}

	deployment, err := h.db.CreateReleaseUpdate(r.Context(), relID, rebootBehavior, scheduledTime, h.currentUsername(r))
	if err != nil {
		http.Error(w, "Internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if allowWipe {
		if err := h.db.SetUpdateAllowWipe(r.Context(), deployment.ID, true); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		h.audit(r, "deployment.allow_wipe", strconv.Itoa(deployment.ID), rel.Version)
	}
	if len(eligible) > 0 {
		if err := h.db.SendUpdateToDevices(r.Context(), deployment.ID, eligible, forceFull); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
	}
	// The legacy half: same release, same rollout, offered on each device's next
	// otautil poll instead of pushed as a command.
	if len(legacySerials) > 0 {
		legacyID, err := h.db.CreateLegacyDeployment(r.Context(), relID, legacySerials, h.currentUsername(r))
		if err != nil {
			log.Printf("[ota] legacy half of deployment %d: %v", deployment.ID, err)
		} else if err := h.db.LinkLegacyDeployment(r.Context(), legacyID, deployment.ID); err != nil {
			log.Printf("[ota] linking legacy rollout %d to deployment %d: %v", legacyID, deployment.ID, err)
		}
	}
	// Deliver to connected targets right now, the same way every other command
	// type (reboot, screenshot, ...) already does — don't wait for the device's
	// next periodic check-in to notice the pending update_devices row.
	h.pushOTAToConnected(r.Context(), eligible)
	// Belt-and-suspenders fallback: nudge online targets to check in NOW too, in
	// case a device's connection state raced between here and pushOTAToConnected
	// (CreateOTACommandIfNone is idempotent, so this can never double-send).
	// Offline devices pick the update up on their next check-in either way.
	h.nudgeCheckin(eligible)

	h.audit(r, "release.deploy", rel.Version, strconv.Itoa(len(eligible)))
	http.Redirect(w, r, fmt.Sprintf("/releases/%d/deployments/%d", relID, deployment.ID), http.StatusSeeOther)
}

// learnPackageSizes asks the package host how big each package is and remembers it. A
// HEAD costs nothing and only happens while a size is still unknown.
func (h *Handler) learnPackageSizes(pkgs []db.OTAPackage) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, p := range pkgs {
		if p.PayloadSize > 0 || p.UpdateURL == "" {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, p.UpdateURL, nil)
		if err != nil {
			continue
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.ContentLength > 0 {
			if err := h.db.SetPackageSize(ctx, p.ID, resp.ContentLength); err != nil {
				log.Printf("[ota] remember package %d size: %v", p.ID, err)
			}
		}
	}
}

// otaRowView is one device's live OTA state on the deployment page. Two clocks, because
// they answer different questions: SeenAt is the last time the device said anything
// (is it still there?), AdvancedAt the last time the percent moved (is it getting
// anywhere?). Conflating them is what called a slow download dead.
// UpdateTargetLike is the little a stall explanation needs from a row.
type UpdateTargetLike interface{ GetLastSeenAt() time.Time }

type otaRowView struct {
	Phase      string
	Percent    int
	BytesDone  int64
	BytesTotal int64
	SeenAt     time.Time
	AdvancedAt time.Time
}

// Size renders what the page should say about how far a download has got: real figures
// when the client counts bytes, and an estimate from the package's own size when it only
// sends a percent — because one percent of a two-gigabyte image is twenty megabytes, and
// "1%" for twenty minutes reads as a dead transfer.
func (v otaRowView) Size() string {
	done, total := v.BytesDone, v.BytesTotal
	approx := ""
	if done == 0 && total > 0 && v.Percent > 0 {
		done, approx = total*int64(v.Percent)/100, "about "
	}
	switch {
	case done > 0 && total > 0:
		return approx + humanBytes(done) + " of " + humanBytes(total)
	case total > 0:
		return humanBytes(total) + " to fetch"
	}
	return ""
}

// humanBytes is deliberately coarse: MB up to a gigabyte, then one decimal of GB.
func humanBytes(n int64) string {
	const mb = 1 << 20
	if n < 1<<30 {
		return strconv.FormatInt((n+mb/2)/mb, 10) + " MB"
	}
	return strconv.FormatFloat(float64(n)/float64(int64(1)<<30), 'f', 1, 64) + " GB"
}

// UpdatedAt keeps the template's older field name pointing at the advance clock.
func (v otaRowView) UpdatedAt() time.Time { return v.AdvancedAt }

func (h *Handler) DeploymentDetail(w http.ResponseWriter, r *http.Request) {
	_ = h.db.CompleteSettledDeployments(r.Context())
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	upd, err := h.db.GetUpdate(r.Context(), did)
	if err != nil || upd.ReleaseID != relID {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	if rel, err := h.db.GetRelease(r.Context(), relID); err == nil && rel.IsDev && !roleSeesDev(h.role(r)) {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	targets, _ := h.db.GetUpdateTargets(r.Context(), did)
	// Only the targets this user can see.
	if acc := h.access(r); acc.hidesDevices() {
		kept := targets[:0:0]
		for _, t := range targets {
			if acc.visible(t.DeviceID) {
				kept = append(kept, t)
			}
		}
		targets = kept
	}
	upd.Targets = targets

	otaProgress := make(map[string]any)
	progressByID := map[uuid.UUID]otaRowView{}
	// What the devices have actually reported, from command_status — written on every
	// progress frame. In-memory progress (shell.Manager) is a cache: it is empty after a
	// restart and holds nothing for a device reporting over HTTP to another replica, and
	// relying on it alone is what made a whole deployment read "stalled" while every
	// device in it was downloading.
	dbProgress, _ := h.db.OTAProgressForUpdate(r.Context(), did)
	// Older clients send only a percent. The size of the package the device was offered
	// turns that into something legible; it is an estimate and the page says so. That is
	// the incremental from the build it was on when one exists, else the full image: the
	// largest package (always the full one) made an 18 MB incremental read "of 1.8 GB".
	var fullSize, anySize int64
	incSize := map[string]int64{}
	incremental, hasFullPkg := map[string]bool{}, false
	if pkgs, err := h.db.ListPackagesByRelease(r.Context(), relID); err == nil {
		for _, p := range pkgs {
			if p.Status == "active" {
				if p.Type == "full" {
					hasFullPkg = true
				} else if p.SourceBuildID != "" {
					incremental[p.SourceBuildID] = true
				}
			}
			if p.Status == "active" {
				if p.Type == "full" && p.PayloadSize > fullSize {
					fullSize = p.PayloadSize
				} else if p.Type != "full" && p.SourceBuildID != "" && p.PayloadSize > incSize[p.SourceBuildID] {
					incSize[p.SourceBuildID] = p.PayloadSize
				}
			}
			if p.PayloadSize > anySize {
				anySize = p.PayloadSize
			}
		}
		if anySize == 0 {
			// Nothing recorded the size when these packages were made. Ask the host
			// once, in the background, so the next render has a denominator.
			go h.learnPackageSizes(pkgs)
		}
	}
	counts := make(map[string]int)
	done := 0
	durSum, durCount := 0, 0
	for i := range targets {
		t := targets[i]
		// Once a target is past the work, never show its live progress bar —
		// shell.Manager's in-memory cache has no TTL and nothing clears it when a
		// status is corrected out-of-band (e.g. a manual DB fix after a lost ack), so
		// a stale in-memory percent could otherwise sit next to "Installed" forever.
		// reboot_sent and canceled count too: a row reading "reboot sent · installing
		// 0%" looks like the reboot was pushed mid-install, when in fact the percent
		// is left over from a later, unrelated attempt.
		switch t.Status {
		case "installed", "failed", "canceled", "reboot_sent", "awaiting_reboot":
		default:
			v := otaRowView{}
			// When this attempt began. Anything the caches remember from before it
			// describes a download that no longer exists: the in-memory progress has no
			// TTL, so a fresh attempt inherited an hours-old "last advanced" and was
			// called stuck within a minute of starting.
			started := t.StartedAt
			if p, ok := dbProgress[t.DeviceID]; ok && !p.CmdAt.IsZero() {
				started = &p.CmdAt
			}
			if p := h.shell.GetOTAProgress(t.DeviceID); p != nil && (started == nil || !p.UpdatedAt.Before(*started)) {
				// Memory knows when the percent last *advanced* — it deliberately keeps
				// the old timestamp while a device repeats the same frame.
				v.Phase, v.Percent, v.AdvancedAt, v.SeenAt = p.Phase, p.Percent, p.UpdatedAt, p.UpdatedAt
			}
			if p, ok := dbProgress[t.DeviceID]; ok {
				if p.ReportAt.After(v.SeenAt) {
					v.SeenAt = p.ReportAt // last contact of any kind
				}
				v.BytesDone, v.BytesTotal = p.BytesDone, p.BytesTotal
				if v.Phase == "" || p.Percent > v.Percent {
					v.Phase, v.Percent = p.Status, p.Percent
					if v.AdvancedAt.IsZero() {
						v.AdvancedAt = p.ReportAt
					}
				}
			}
			if v.BytesTotal == 0 {
				v.BytesTotal = offeredPackageSize(t, incSize, fullSize)
			}
			if !v.SeenAt.IsZero() || v.Percent > 0 {
				if v.AdvancedAt.IsZero() {
					v.AdvancedAt = v.SeenAt
				}
				// A download cannot have been stuck for longer than it has existed.
				if started != nil && v.AdvancedAt.Before(*started) {
					v.AdvancedAt = *started
				}
				otaProgress[t.DeviceID.String()] = v
				progressByID[t.DeviceID] = v
				// The row's updated_at only moves at status checkpoints, so on its own it
				// says nothing about a download in flight. Last contact is the honest
				// clock for "is this device still working on it".
				if v.SeenAt.After(targets[i].UpdatedAt) {
					targets[i].UpdatedAt = v.SeenAt
				}
			}
		}
		counts[t.Status]++
		switch t.Status {
		case "installed", "awaiting_reboot", "reboot_sent":
			done++ // applied to the inactive slot or beyond
		}
		if d := t.DurationSeconds(); d >= 0 {
			durSum += d
			durCount++
		}
	}
	avgDuration := -1
	if durCount > 0 {
		avgDuration = durSum / durCount
	}
	pct := 0
	if len(targets) > 0 {
		pct = done * 100 / len(targets)
	}
	// Score-ring stroke-dashoffset (cc-ring: r=44, circumference 276.5) for the
	// deployment-detail hero — precomputed here since templates have no multiply func.
	ringOffset := 276.5 * float64(100-pct) / 100

	// The legacy half of this rollout, if it has one: devices whose build cannot
	// apply an MDM OTA were served the same release over the otautil path, and this
	// page is where the whole push is accounted for.
	legacyDep, _ := h.db.LegacyDeploymentForUpdate(r.Context(), did)
	legacyRows := []db.LegacyDeploymentDevice{}
	legacyDone := 0
	if legacyDep != nil {
		for i := range legacyDep.Devices {
			dv := &legacyDep.Devices[i]
			if st, ok := ota.Legacy.Get(dv.Serial); ok && (dv.Status == "downloading" || dv.Status == "installing" || dv.Status == "offered") {
				dv.Percent = st.Percent
				if st.Phase != "" {
					dv.Status = st.Phase
				}
			}
			switch {
			case dv.Status == "installed", dv.Status == "awaiting_reboot",
				dv.Status == "installing" && dv.Percent >= 100:
				legacyDone++
			}
		}
		legacyRows = legacyDep.Devices
		if acc := h.access(r); acc.hidesDevices() {
			serials := make([]string, len(legacyRows))
			for j, dv := range legacyRows {
				serials[j] = dv.Serial
			}
			ok := map[string]bool{}
			for _, sn := range h.keepVisibleSerials(r, serials) {
				ok[sn] = true
			}
			kept := legacyRows[:0:0]
			for _, dv := range legacyRows {
				if ok[dv.Serial] {
					kept = append(kept, dv)
				}
			}
			legacyRows = kept
		}
	}
	totalTargets := len(targets) + len(legacyRows)
	if totalTargets > 0 {
		pct = (done + legacyDone) * 100 / totalTargets
		ringOffset = 276.5 * float64(100-pct) / 100
	}

	// The legacy devices count towards the same status chips. Without this the hero read
	// "18 / 19 installed" next to a lone "7 installed" chip, because the chips only knew
	// about the agent-managed half — and nothing on the page said which device was still
	// going, or why the ring was short of 100%.
	for _, dv := range legacyRows {
		switch dv.Status {
		case "offered":
			counts["pending"]++
		case "downloading", "installing", "verifying", "finalizing":
			counts["installing"]++
		case "installed", "updated":
			counts["installed"]++
		// Its own bucket, not "installed". Folding it into installed is what made
		// "Reboot all installed (N)" quietly exclude the legacy half: the button reads
		// the awaiting_reboot count, so every otautil device waiting on a reboot was
		// invisible to it.
		case "awaiting_reboot":
			counts["awaiting_reboot"]++
		case "failed":
			counts["failed"]++
		default:
			counts[dv.Status]++
		}
	}
	// What "Reboot all" can actually reach. The chips count every device awaiting a
	// reboot, which is the truth about the rollout; the button must instead count only
	// the ones a reboot can be delivered to. otautil has no reboot of its own, so a
	// legacy device is only rebootable remotely if the same serial also runs the agent
	// — and a button offering to reboot devices it cannot reach is worse than one that
	// admits the number is smaller.
	rebootable := counts["awaiting_reboot"]
	for _, dv := range legacyRows {
		if dv.Status == "awaiting_reboot" && !dv.InFleet {
			rebootable--
		}
	}
	if rebootable < 0 {
		rebootable = 0
	}

	// Ordered, non-zero status buckets for the rollup line (map iteration order
	// is unstable, so build a fixed-order slice for the template).
	var summary []map[string]any
	for _, s := range []string{"pending", "downloading", "installing", "installed", "awaiting_reboot", "reboot_sent", "failed"} {
		if counts[s] > 0 {
			summary = append(summary, map[string]any{"Status": s, "Count": counts[s]})
		}
	}

	data := map[string]any{
		"Title":        fmt.Sprintf("Deployment #%d", did),
		"Deployment":   upd,
		"Release":      upd.Release,
		"OTAProgress":  otaProgress,
		"Summary":      summary,
		"Rebootable":   rebootable,
		"SummaryDone":  done + legacyDone,
		"SummaryTotal": totalTargets,
		"SummaryPct":   pct,
		"RingOffset":   ringOffset,
		"AvgDuration":  avgDuration, // seconds, or -1 if no device finished yet
		"DoneCount":    durCount,    // devices with a measured duration
		"LegacyDep":    legacyDep,
		"LegacyRows":   legacyRows,
	}
	version := ""
	if upd.Release != nil {
		version = upd.Release.Version
	}
	data["D1"] = buildDeployD1(dp1Inputs{
		Dep: upd, Version: version, Targets: targets, Progress: progressByID, Legacy: legacyRows,
		Incremental: incremental, HasFull: hasFullPkg, Rebootable: rebootable,
		CanOperate: roleCanOperate(h.role(r)), Base: fmt.Sprintf("/releases/%d/deployments/%d", relID, did),
		SeenSecs: h.cfg.CheckinInterval() * 3, Now: time.Now(),
	})

	// HTMX polling target: just the device-status table. Use the ETag/304 helper so an
	// unchanged poll short-circuits and htmx skips the swap — otherwise the every-3s
	// poll re-renders the table on every tick and the section visibly flickers.
	if r.URL.Query().Get("partial") == "targets" {
		h.renderCachedHTML(w, r, "deployment-targets", h.withRole(r, data))
		return
	}

	// The "add targets" picker only renders for operators on a non-canceled
	// deployment (see template) — skip the expensive full-fleet load otherwise so
	// viewers and canceled/finished deployments don't pay for a list they can't use.
	role := h.role(r)
	canOp := roleCanOperate(role)
	if !canOp || upd.Status == "canceled" {
		data["Devices"] = nil
		data["Online"] = map[uuid.UUID]bool{}
		data["Groups"] = nil
		h.render(w, r, "deployment_detail.html", data)
		return
	}

	// Only the full page needs the device/group lists for the "add targets" picker.
	devices, _ := h.db.ListDevices(r.Context(), db.DeviceFilter{}, 0, 10000, "", "")
	devices = h.access(r).keepVisible(devices)
	groups, _ := h.db.ListGroups(r.Context())

	// Mirror the resolver's eligibility (see ReleaseDetail): an incremental-only
	// release only reaches devices whose current build matches an active
	// incremental's source_build_id, so limit the "add targets" device list to
	// those. A full image can flash any device, so leave the list unfiltered.
	packages, _ := h.db.ListPackagesByRelease(r.Context(), relID)
	hasFull := false
	sourceBuilds := map[string]bool{}
	for _, p := range packages {
		if p.Status != "active" {
			continue
		}
		if p.Type == "full" {
			hasFull = true
		} else if p.SourceBuildID != "" {
			sourceBuilds[p.SourceBuildID] = true
		}
	}
	// Devices already in this deployment shouldn't appear in the "add more"
	// picker — they're already receiving (or have received) this update.
	existing := make(map[uuid.UUID]bool, len(targets))
	for _, t := range targets {
		existing[t.DeviceID] = true
	}
	// A deployment is restricted to its release's product — the resolver only ever
	// hands the update to matching devices, so the picker must only offer those.
	wantProduct, _ := product.Resolve(upd.Release.Product)
	eligible := devices[:0]
	for _, d := range devices {
		if existing[d.ID] {
			continue
		}
		if got, _ := product.Resolve(d.Product); got.Key != wantProduct.Key {
			continue
		}
		if !hasFull && !sourceBuilds[d.BuildID] {
			continue
		}
		if !h.otaGate.Device(r.Context(), d).OK { // legacy-OTA-only build
			continue
		}
		eligible = append(eligible, d)
	}
	devices = eligible

	connected := h.hub.ConnectedIDs()
	online := make(map[uuid.UUID]bool, len(connected))
	for cid := range connected {
		online[cid] = true
	}
	data["Devices"] = devices
	data["HasFull"] = hasFull
	data["SourceBuilds"] = sourceBuilds
	data["Online"] = online
	data["Groups"] = groups

	h.render(w, r, "deployment_detail.html", data)
}

func (h *Handler) DeploymentUpdateSettings(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	upd, err := h.db.GetUpdate(r.Context(), did)
	if err != nil || upd.ReleaseID != relID {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	// Reboot timing applies to every device in the rollout.
	if !h.holdsWholeRollout(w, r, did, "ota") {
		return
	}

	rebootBehavior := strings.TrimSpace(r.FormValue("reboot_behavior"))
	switch rebootBehavior {
	case "immediate", "scheduled", "manual":
	default:
		rebootBehavior = "immediate"
	}
	scheduledTime := parseScheduledUTC(r.FormValue("scheduled_time"), rebootBehavior)

	if err := h.db.UpdateDeploymentRebootSettings(r.Context(), did, rebootBehavior, scheduledTime); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hub.PublishDeploymentUpdate()
	h.hxRedirect(w, r, fmt.Sprintf("/releases/%d/deployments/%d", relID, did))
}

func (h *Handler) DeploymentDelete(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	if upd, err := h.db.GetUpdate(r.Context(), did); err != nil || upd.ReleaseID != relID {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	if !h.holdsWholeRollout(w, r, did, "ota") {
		return
	}
	if err := h.db.DeleteUpdate(r.Context(), did); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hub.PublishDeploymentUpdate()
	h.hxRedirect(w, r, fmt.Sprintf("/releases/%d", relID))
}

// DeploymentCancel stops an active deployment from reaching devices that
// haven't started yet (pending → canceled) and flips the update off 'active'.
func (h *Handler) DeploymentCancel(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "deploy") {
		return
	}
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	upd, err := h.db.GetUpdate(r.Context(), did)
	if err != nil || upd.ReleaseID != relID {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	if err := h.db.CancelDeployment(r.Context(), did); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// One rollout, both halves: cancelling the deployment cancels the legacy targets
	// it was pushed with, or they would keep being offered on every poll.
	if legacy, err := h.db.LegacyDeploymentForUpdate(r.Context(), did); err == nil && legacy != nil {
		_ = h.db.CancelLegacyDeployment(r.Context(), legacy.ID)
	}
	h.audit(r, "deployment.cancel", strconv.Itoa(did), "")
	h.hub.PublishDeploymentUpdate()
	h.hxRedirect(w, r, fmt.Sprintf("/releases/%d/deployments/%d", relID, did))
}

// DeploymentCancelDeviceOTA aborts one device's in-flight OTA — only while it's
// still downloading, never mid-install (aborting update_engine partway through
// writing the inactive slot risks a corrupt/unbootable slot; a download can be
// safely thrown away and resumed from scratch). Pushes cancel_command over WS;
// the client checks its current phase and only actually cancels if it's still
// "downloading" (see MdmService's cancel_command handling), then reports a
// terminal "error"/CANCELLED through the same durable ack path every other OTA
// terminal status uses, so the usual afterOtaTerminal bookkeeping applies.
func (h *Handler) DeploymentCancelDeviceOTA(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	upd, err := h.db.GetUpdate(r.Context(), did)
	if err != nil || upd.ReleaseID != relID {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, "ota", device.ID) {
		return
	}
	cmdID, ok, err := h.db.GetActiveOTACommandID(r.Context(), device.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if ok {
		if msg, e := json.Marshal(map[string]any{"type": "cancel_command", "id": cmdID.String()}); e == nil {
			h.hub.Push(device.ID, msg)
		}
	}
	// The device may never answer (rebooted mid-download, offline): settle the
	// row ourselves so it reads "failed · cancelled" and offers Retry, and clear
	// the OTA guard so a later retry can re-issue the update.
	_ = h.db.ClearPendingOTACommands(r.Context(), device.ID)
	_ = h.db.SetUpdateDeviceFailed(r.Context(), did, device.ID, "CANCELLED")
	h.hub.PublishDeviceUpdate(device.ID)
	h.hub.PublishDeploymentUpdate()
	h.auditDev(r, "deployment.cancel_ota", device.ID, r.PathValue("serial"), strconv.Itoa(did))
	h.hxRedirect(w, r, fmt.Sprintf("/releases/%d/deployments/%d", relID, did))
}

// DeploymentRetryDevice re-arms one failed device on a deployment: it clears the
// OTA command guard and resets the device's row to pending, so the next check-in
// re-issues the update (same mechanism as DeviceClearOTA).
func (h *Handler) DeploymentRetryDevice(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	upd, err := h.db.GetUpdate(r.Context(), did)
	if err != nil || upd.ReleaseID != relID {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, "ota", device.ID) {
		return
	}
	// A stuck download is still running on the device: stop it first, so the re-offer
	// starts a fresh one instead of piling on top of it.
	if cmdID, ok, err := h.db.GetActiveOTACommandID(r.Context(), device.ID); err == nil && ok {
		if msg, e := json.Marshal(map[string]any{"type": "cancel_command", "id": cmdID.String()}); e == nil {
			h.hub.Push(device.ID, msg)
		}
	}
	if err := h.db.ClearPendingOTACommands(r.Context(), device.ID); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Two retry modes: "full" pins the device to the full image (a failed device has
	// almost always tripped on an incremental that can't apply to its source build,
	// and the full package is guaranteed-applicable — also recovers devices that
	// failed before the auto-fallback existed). Plain retry leaves force_full alone
	// and just re-pends, so ResolveUpdateForDevice picks the same delivery it would
	// have chosen originally (e.g. a transient DOWNLOAD_ERROR unrelated to the
	// package itself, where the incremental is still the right, smaller download).
	if r.FormValue("delivery") == "full" {
		// A wipe image is only handed out by a deployment that allowed it; pinning the
		// device to one otherwise would leave it pending with nothing it may be sent.
		if wipes, _ := h.db.ReleaseHasWipeFull(r.Context(), relID); wipes {
			if allow, _ := h.db.UpdateAllowsWipe(r.Context(), did); !allow {
				http.Error(w, "This release's full image wipes the device (factory reset), and this deployment was not created to allow that. Create a deployment with \"Allow factory reset\" ticked instead.", http.StatusBadRequest)
				return
			}
		}
		_ = h.db.SetUpdateDeviceForceFull(r.Context(), did, device.ID)
	} else {
		_ = h.db.ClearUpdateDeviceForceFull(r.Context(), did, device.ID)
	}
	_ = h.db.SetUpdateDeviceStatus(r.Context(), did, device.ID, "pending")
	// If the deployment was already marked complete, re-pending one device would
	// otherwise strand it (ResolveUpdateForDevice only serves status='active').
	_ = h.db.ReactivateUpdate(r.Context(), did)
	h.hub.PublishDeviceUpdate(device.ID)
	h.hub.PublishDeploymentUpdate()
	h.auditDev(r, "deployment.retry", device.ID, r.PathValue("serial"), strconv.Itoa(did))
	h.hxRedirect(w, r, fmt.Sprintf("/releases/%d/deployments/%d", relID, did))
}

// DeploymentRebootDevice issues the reboot that applies an OTA which has finished
// installing to the inactive slot and is waiting (status 'awaiting_reboot' — the case
// for a manual, or a not-yet-due scheduled, reboot behavior). It pushes a reboot command
// and flips the row to 'reboot_sent', the same as the automatic/scheduled path — so a
// manual deployment can be applied on demand from the deployment page instead of only
// via a separate device reboot.
func (h *Handler) DeploymentRebootDevice(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	upd, err := h.db.GetUpdate(r.Context(), did)
	if err != nil || upd.ReleaseID != relID {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, "reboot", device.ID) {
		return
	}
	cmd, err := h.db.CreateCommandBy(r.Context(), "reboot", "", nil, "devices", []uuid.UUID{device.ID}, h.currentUsername(r))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	_ = h.db.SetUpdateDeviceStatus(r.Context(), did, device.ID, "reboot_sent")
	// Optimistically complete rather than waiting for a confirming check-in, which
	// in a multi-instance deployment may never land on this instance at all — see
	// db.OptimisticallyCompleteReboot.
	if err := h.db.OptimisticallyCompleteReboot(r.Context(), did, device.ID); err != nil {
		log.Printf("[deployment-reboot] OptimisticallyCompleteReboot error: %v", err)
	} else {
		_ = h.db.CheckAndCompleteUpdate(r.Context(), did)
	}
	h.pushCommand(r.Context(), cmd, "devices", []uuid.UUID{device.ID})
	h.hub.PublishDeviceUpdate(device.ID)
	h.hub.PublishDeploymentUpdate()
	h.auditDev(r, "deployment.reboot", device.ID, r.PathValue("serial"), strconv.Itoa(did))
	h.hxRedirect(w, r, fmt.Sprintf("/releases/%d/deployments/%d", relID, did))
}

// DeploymentRebootAll reboots every device in a deployment that has installed and is
// waiting (status 'awaiting_reboot') — the bulk form of DeploymentRebootDevice, for
// applying a manual deployment to the whole fleet at once.
func (h *Handler) DeploymentRebootAll(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	upd, err := h.db.GetUpdate(r.Context(), did)
	if err != nil || upd.ReleaseID != relID {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	ids, err := h.db.ListAwaitingRebootForUpdate(r.Context(), did)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// "All" is all this user may reboot.
	acc := h.access(r)
	ids, _ = acc.filterDevices("reboot", ids)
	for _, deviceID := range ids {
		cmd, err := h.db.CreateCommandBy(r.Context(), "reboot", "", nil, "devices", []uuid.UUID{deviceID}, h.currentUsername(r))
		if err != nil {
			log.Printf("[deployment-reboot-all] create reboot error device=%s: %v", deviceID, err)
			continue
		}
		_ = h.db.SetUpdateDeviceStatus(r.Context(), did, deviceID, "reboot_sent")
		if err := h.db.OptimisticallyCompleteReboot(r.Context(), did, deviceID); err != nil {
			log.Printf("[deployment-reboot-all] OptimisticallyCompleteReboot error device=%s: %v", deviceID, err)
		} else {
			_ = h.db.CheckAndCompleteUpdate(r.Context(), did)
		}
		h.pushCommand(r.Context(), cmd, "devices", []uuid.UUID{deviceID})
		h.hub.PublishDeviceUpdate(deviceID)
	}
	// One rollout, both halves — the same rule cancelling already follows. The legacy
	// devices installed the same build over otautil and are waiting on the same reboot;
	// leaving them out meant "reboot all" rebooted some of them and said nothing.
	legacyN := 0
	legacyAwaiting, lerr := h.db.ListLegacyAwaitingRebootForUpdate(r.Context(), did)
	if lerr != nil {
		log.Printf("[deployment-reboot-all] list legacy awaiting: %v", lerr)
	}
	for _, la := range legacyAwaiting {
		if !acc.canDevice("reboot", la.DeviceID) {
			continue
		}
		// Same guard the per-device legacy reboot uses: never reboot a device that is
		// still taking an OTA.
		if blocked, why, err := h.db.RebootBlockedFor(r.Context(), la.DeviceID); err == nil && blocked {
			log.Printf("[deployment-reboot-all] skipped legacy %s: %s", la.Serial, why)
			continue
		}
		cmd, err := h.db.CreateCommandBy(r.Context(), "reboot", "", nil, "devices", []uuid.UUID{la.DeviceID}, h.currentUsername(r))
		if err != nil {
			log.Printf("[deployment-reboot-all] create reboot error legacy=%s: %v", la.Serial, err)
			continue
		}
		h.pushCommand(r.Context(), cmd, "devices", []uuid.UUID{la.DeviceID})
		_ = h.db.SetLegacyDeploymentReboot(r.Context(), la.DeploymentID, la.Serial, cmd.ID)
		h.hub.PublishDeviceUpdate(la.DeviceID)
		legacyN++
	}
	h.hub.PublishDeploymentUpdate()
	h.audit(r, "deployment.reboot_all", strconv.Itoa(did),
		fmt.Sprintf("%d fleet, %d legacy", len(ids), legacyN))
	h.hxRedirect(w, r, fmt.Sprintf("/releases/%d/deployments/%d", relID, did))
}

// holdsWholeRollout refuses a whole-rollout change (reboot timing, deleting it) unless
// the user holds action on every device the rollout targets.
func (h *Handler) holdsWholeRollout(w http.ResponseWriter, r *http.Request, did int, action string) bool {
	acc := h.access(r)
	if acc.unrestricted() {
		return true
	}
	ids, err := h.db.UpdateDeviceIDs(r.Context(), did)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return false
	}
	if _, dropped := acc.filterDevices(action, ids); dropped > 0 {
		h.denied(r, action, nil, decision{Reason: "rollout reaches devices outside the policy"})
		http.Error(w, fmt.Sprintf("This rollout includes %d device(s) outside your access policy.", dropped), http.StatusForbidden)
		return false
	}
	return true
}

// DeploymentRemoveDevice drops a still-pending device from a deployment so it will
// never receive the update. Only 'pending' rows can be removed (nothing has been
// sent yet); once a device has started downloading the request is a no-op.
func (h *Handler) DeploymentRemoveDevice(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	upd, err := h.db.GetUpdate(r.Context(), did)
	if err != nil || upd.ReleaseID != relID {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, "reboot", device.ID) {
		return
	}
	if !h.requireDeviceAction(w, r, "ota", device.ID) {
		return
	}
	removed, err := h.db.RemoveDeviceFromUpdate(r.Context(), did, device.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if removed {
		// Clear any guard so a queued OTA can't still reach the device, and refresh
		// the dashboard — mirrors the retry/clear-OTA path.
		_ = h.db.ClearPendingOTACommands(r.Context(), device.ID)
		h.hub.PublishDeviceUpdate(device.ID)
		h.hub.PublishDeploymentUpdate()
		h.audit(r, "deployment.remove_device", r.PathValue("serial"), strconv.Itoa(did))
	}
	h.hxRedirect(w, r, fmt.Sprintf("/releases/%d/deployments/%d", relID, did))
}

// DeploymentAddTargets widens an existing deployment by adding more devices or
// groups to it, instead of creating a separate deployment. Eligible devices
// (not already on an active update) are appended to the same update.
func (h *Handler) DeploymentAddTargets(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	did, err := strconv.Atoi(r.PathValue("did"))
	if err != nil {
		http.Error(w, "Invalid deployment ID", http.StatusBadRequest)
		return
	}
	upd, err := h.db.GetUpdate(r.Context(), did)
	if err != nil || upd.ReleaseID != relID {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	rel, err := h.db.GetRelease(r.Context(), relID)
	if err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	r.ParseForm()
	// Delivery choice mirrors the initial push: "full" pins the newly added devices
	// to the full image; default "smart" lets the resolver pick an incremental.
	forceFull := r.FormValue("delivery") == "full"
	if forceFull {
		pkgs, _ := h.db.ListPackagesByRelease(r.Context(), relID)
		hasFull, fullWipes := false, false
		for _, p := range pkgs {
			if p.Status == "active" && p.Type == "full" {
				hasFull, fullWipes = true, p.Wipe
				break
			}
		}
		if !hasFull {
			http.Error(w, "This release has no full image — add one before forcing full delivery.", http.StatusBadRequest)
			return
		}
		if allow, _ := h.db.UpdateAllowsWipe(r.Context(), did); fullWipes && !allow {
			http.Error(w, "This release's full image wipes the device (factory reset), and this deployment was not created to allow that.", http.StatusBadRequest)
			return
		}
	}
	eligible, err := h.resolveEligibleDevices(r, rel.Product)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Same monotonicity guard as the initial push — never add a device that's already on a
	// same-or-newer release.
	eligible, err = h.db.RemoveDowngradeTargets(r.Context(), relID, eligible)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// And drop devices with no applicable artifact for this release.
	// Added targets follow the rule the deployment was created with.
	allowWipe, _ := h.db.UpdateAllowsWipe(r.Context(), did)
	eligible, err = h.db.RemoveInapplicableTargets(r.Context(), relID, eligible, allowWipe)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if len(eligible) > 0 {
		if err := h.db.SendUpdateToDevices(r.Context(), did, eligible, forceFull); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		// Nudge online targets to check in now so the OTA resolves immediately.
		h.nudgeCheckin(eligible)
	}
	h.audit(r, "deployment.add_targets", strconv.Itoa(did), strconv.Itoa(len(eligible)))
	h.hub.PublishDeploymentUpdate()
	h.hxRedirect(w, r, fmt.Sprintf("/releases/%d/deployments/%d", relID, did))
}

func parseScheduledUTC(raw, rebootBehavior string) *time.Time {
	if rebootBehavior != "scheduled" {
		return nil
	}
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	t, err := time.ParseInLocation("2006-01-02T15:04", s, time.UTC)
	if err != nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// resolveEligibleDevices builds the target set from the form's serials/group_ids,
// dropping devices that already have an active update. When product != "" it also drops
// devices of a different product, so a release is never deployed cross-product (the
// resolver enforces this too, but filtering here keeps mismatched devices out of the
// deployment entirely).
func (h *Handler) resolveEligibleDevices(r *http.Request, product string) ([]uuid.UUID, error) {
	ids, err := h.resolveEligibleDevicesUnscoped(r, product)
	if err != nil {
		return nil, err
	}
	// A dev's OTA rules decide which of the selected devices they may target.
	kept, dropped := h.access(r).filterDevices("ota", ids)
	if dropped > 0 {
		log.Printf("[access] %s: OTA push narrowed to %d of %d device(s)", h.currentUsername(r), len(kept), len(ids))
	}
	return kept, nil
}

func (h *Handler) resolveEligibleDevicesUnscoped(r *http.Request, product string) ([]uuid.UUID, error) {
	var deviceIDs []uuid.UUID

	// "All devices" scope (Actions-style target rail on the push page): every device of
	// the release's product. The per-product filter below still applies; the union with
	// any explicitly ticked serials is harmless (dedup handles overlap).
	if r.FormValue("scope_all") == "1" {
		devs, err := h.db.ListDevices(r.Context(), db.DeviceFilter{Product: product}, 0, 100000, "serial", "asc")
		if err != nil {
			return nil, err
		}
		for _, d := range devs {
			deviceIDs = append(deviceIDs, d.ID)
		}
	}

	// serials come from the per-device checkboxes and/or a pasted bulk list — the
	// same parseSerialsField used by the Actions target picker splits commas/newlines.
	serials := parseSerialsField(r.Form["serials"])
	if len(serials) > 0 {
		ids, err := h.db.GetDeviceIDsBySerials(r.Context(), serials)
		if err != nil {
			return nil, err
		}
		deviceIDs = append(deviceIDs, ids...)
	}

	for _, s := range r.Form["group_ids"] {
		if gid, err := uuid.Parse(s); err == nil {
			ids, err := h.db.GetDeviceIDsByGroupIDs(r.Context(), []uuid.UUID{gid})
			if err != nil {
				return nil, err
			}
			deviceIDs = append(deviceIDs, ids...)
		}
	}

	for _, s := range r.Form["restaurant_ids"] {
		if rid, err := uuid.Parse(s); err == nil {
			ids, err := h.db.GetDeviceIDsByRestaurantIDs(r.Context(), []uuid.UUID{rid})
			if err != nil {
				return nil, err
			}
			deviceIDs = append(deviceIDs, ids...)
		}
	}

	seen := make(map[uuid.UUID]bool)
	var unique []uuid.UUID
	for _, did := range deviceIDs {
		if !seen[did] {
			seen[did] = true
			unique = append(unique, did)
		}
	}

	// OTA is for our firmware devices only: DPC-managed devices are never targets,
	// whatever scope, group or pasted serial list selected them.
	if len(unique) > 0 {
		var err error
		unique, err = h.db.FilterFirmwareDeviceIDs(r.Context(), unique)
		if err != nil {
			return nil, err
		}
	}

	// Keep only devices matching the release's product (wrong-product devices can't be
	// targeted at all). Skipped when no product is supplied.
	if product != "" && len(unique) > 0 {
		var err error
		unique, err = h.db.FilterDeviceIDsByProduct(r.Context(), unique, product)
		if err != nil {
			return nil, err
		}
	}

	var eligible []uuid.UUID
	for _, did := range unique {
		has, err := h.db.DeviceHasActiveUpdate(r.Context(), did)
		if err != nil {
			return nil, err
		}
		if !has {
			eligible = append(eligible, did)
		}
	}

	if limit, err := strconv.Atoi(strings.TrimSpace(r.FormValue("limit"))); err == nil && limit > 0 && len(eligible) > limit {
		eligible = eligible[:limit]
	}

	return eligible, nil
}

// ── Commands ──────────────────────────────────────────────────────────────────

// actionsWindowDays bounds the main Actions page to recent commands: its triage
// buckets only need them (attention = failures < 3 days, in-progress is
// expiry-bounded, completed is a capped preview). Full history lives at
// /commands/history, which is unbounded + paginated.
const actionsWindowDays = 30

// offeredPackageSize is the size of the package a deployment hands this device: the
// incremental from the build it was on when offered (else its current build), unless it is
// pinned to the full image or no incremental starts there.
func offeredPackageSize(t db.UpdateTarget, incSize map[string]int64, fullSize int64) int64 {
	from := t.SourceBuildID
	if from == "" {
		from = t.BuildID
	}
	if n := incSize[from]; n > 0 && !t.ForceFull {
		return n
	}
	return fullSize
}
