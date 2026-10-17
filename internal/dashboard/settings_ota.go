package dashboard

import (
	"context"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/otaconfig"
	"mdm/internal/product"

	"github.com/google/uuid"
)

// otaCutoffRow is one product's "oldest build with MDM OTA support" setting row.
type otaCutoffRow struct {
	ProductKey   string
	ProductLabel string
	Releases     []db.Release // published releases of this product, newest first
	Current      int          // chosen cutoff release id, 0 = none
}

func (h *Handler) otaCutoffRows(ctx context.Context) []otaCutoffRow {
	rels, _ := h.db.ListReleases(ctx)
	cur := h.cfg.OTAMinRelease()
	byProduct := map[string][]db.Release{}
	var order []string
	for _, rel := range rels {
		if rel.Status != "published" {
			continue
		}
		pk := product.Normalize(rel.Product)
		if _, ok := byProduct[pk]; !ok {
			order = append(order, pk)
		}
		byProduct[pk] = append(byProduct[pk], rel)
	}
	sort.Strings(order)
	var rows []otaCutoffRow
	for _, pk := range order {
		list := byProduct[pk]
		sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedAt.After(list[j].CreatedAt) })
		rows = append(rows, otaCutoffRow{ProductKey: pk, ProductLabel: product.Label(pk), Releases: list, Current: cur[pk]})
	}
	return rows
}

// otaUnsupportedBuilds returns the release versions of a product that predate
// the configured OTA support cutoff (their firmware has no MDM OTA agent), or
// nil when no cutoff is set for that product.
func (h *Handler) otaUnsupportedBuilds(ctx context.Context, productKey string) map[string]bool {
	out := map[string]bool{}
	for build := range h.otaGate.Unsupported(ctx, productKey) {
		out[build] = true
	}
	return out
}

// filterOTACapable splits device ids into those whose build can take an MDM OTA and
// those that cannot. Ids in, ids out — the push path works in ids.
func (h *Handler) filterOTACapable(ctx context.Context, ids []uuid.UUID) (kept, dropped []uuid.UUID) {
	for _, id := range ids {
		d, err := h.db.GetDeviceByID(ctx, id)
		if err != nil || d == nil {
			kept = append(kept, id) // can't tell: let the existing paths decide
			continue
		}
		if h.otaGate.Device(ctx, *d).OK {
			kept = append(kept, id)
		} else {
			dropped = append(dropped, id)
		}
	}
	return kept, dropped
}

// splitOTACapable divides device ids by the transport their build supports.
func (h *Handler) splitOTACapable(ctx context.Context, ids []uuid.UUID) (mdm, legacy []uuid.UUID) {
	for _, id := range ids {
		d, err := h.db.GetDeviceByID(ctx, id)
		if err != nil || d == nil {
			mdm = append(mdm, id) // can't tell: the existing paths decide
			continue
		}
		// A DPC device has no OTA path at all: it goes in neither half.
		if v := h.otaGate.Device(ctx, *d); v.OK {
			mdm = append(mdm, id)
		} else if v.Legacy() {
			legacy = append(legacy, id)
		}
	}
	return mdm, legacy
}

// legacyOnlySerials picks the submitted serials that belong to no fleet device but do
// poll the legacy listener — otautil devices, which the unified picker now offers
// alongside the fleet.
func (h *Handler) legacyOnlySerials(r *http.Request, rel *db.Release) []string {
	serials := parseSerialsField(r.Form["serials"])
	if len(serials) == 0 {
		return nil
	}
	known, err := h.db.ListLegacyOTADevices(r.Context())
	if err != nil {
		return nil
	}
	legacy := make(map[string]bool, len(known))
	for _, d := range known {
		legacy[d.Serial] = true
	}
	var out []string
	for _, s := range serials {
		if !legacy[s] {
			continue
		}
		if dev, err := h.db.GetDevice(r.Context(), s); err == nil && dev != nil {
			continue // in the fleet: already decided by the gate
		}
		out = append(out, s)
	}
	return out
}

// otaUnsupportedForDevices is the same answer keyed by the builds these devices are
// actually on, so a build nobody tracked as a release is covered too.
func (h *Handler) otaUnsupportedForDevices(ctx context.Context, devices []db.Device) map[string]bool {
	out := map[string]bool{}
	for _, d := range devices {
		if _, seen := out[d.BuildID]; seen {
			continue
		}
		if !h.otaGate.Device(ctx, d).OK {
			out[d.BuildID] = true
		}
	}
	return out
}

// SettingsSetOTAMinRelease stores, per product, the oldest release with MDM OTA
// support (form fields ota_min_<product>, 0 or empty = no cutoff).
// SettingsSetLegacyOTA switches the legacy otautil routes between the MDM and the
// old ota-server (pass-through). Takes effect on the next device poll.
func (h *Handler) SettingsSetLegacyOTA(w http.ResponseWriter, r *http.Request) {
	mode := r.FormValue("legacy_ota_mode")
	if err := h.cfg.SetLegacyOTAMode(mode); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "settings.legacy_ota_mode", "", h.cfg.LegacyOTAMode())
	if next := r.FormValue("next"); strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") {
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	h.settingsRedirect(w, r)
}

// SettingsSetOTAConfig saves the legacy OTA discovery-config settings and publishes the
// file immediately, so an admin sees the result rather than waiting for the hourly job.
func (h *Handler) SettingsSetOTAConfig(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	pollMS, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("poll_interval_ms")))
	leadHours, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("lead_hours")))
	if err := h.cfg.SetOTAConfig(
		r.FormValue("managed") == "on",
		r.FormValue("api_base_url"),
		pollMS,
		r.FormValue("timezone"),
		leadHours,
	); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "settings.ota_config", "", "")
	// Publishing on save keeps "what the fleet reads" and "what this page shows" in step.
	h.PublishOTAConfig(r.Context())
	h.hxDoneToast(w, r, h.settingsDest(r), "OTA discovery config saved", "success")
}

func (h *Handler) SettingsSetOTAMinRelease(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	m := map[string]int{}
	for k, v := range r.Form {
		if !strings.HasPrefix(k, "ota_min_") || len(v) == 0 {
			continue
		}
		if id, err := strconv.Atoi(strings.TrimSpace(v[0])); err == nil && id > 0 {
			m[strings.TrimPrefix(k, "ota_min_")] = id
		}
	}
	if err := h.cfg.SetOTAMinRelease(m); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.otaGate.Invalidate()
	h.audit(r, "settings.ota_min_release", "", fmt.Sprint(m))
	h.settingsRedirect(w, r)
}

// SettingsSetOTASupport records an explicit MDM OTA answer for one build, or clears
// it so the build falls back to the product cutoff.
func (h *Handler) SettingsSetOTASupport(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	build := strings.TrimSpace(r.FormValue("build_id"))
	pk := product.Normalize(r.FormValue("product"))
	if build == "" {
		h.settingsRedirect(w, r)
		return
	}
	switch r.FormValue("value") {
	case "yes":
		_ = h.db.SetOTASupport(r.Context(), build, pk, true, "manual", "")
	case "no":
		_ = h.db.SetOTASupport(r.Context(), build, pk, false, "manual", "no MDM OTA on this build — legacy OTA only")
	default:
		_ = h.db.DeleteOTASupport(r.Context(), build, pk)
	}
	h.otaGate.Invalidate()
	h.audit(r, "settings.ota_support", build, r.FormValue("value"))
	h.settingsRedirect(w, r)
}

// otaBuildRows lists every build the fleet is actually running with the gate's
// verdict for it, so the cutoff can be checked against reality rather than trusted.
type otaBuildRow struct {
	BuildID      string
	ProductKey   string
	ProductLabel string
	Devices      int
	OK           bool
	Reason       string
	Source       string
	Tracked      bool
}

func (h *Handler) otaBuildRows(ctx context.Context) []otaBuildRow {
	fleet, _ := h.db.GetFleetVersions(ctx)
	releases, _ := h.db.ListReleases(ctx)
	tracked := map[string]bool{}
	for _, rel := range releases {
		tracked[product.Normalize(rel.Product)+"|"+rel.Version] = true
	}
	rows := make([]otaBuildRow, 0, len(fleet))
	for _, fv := range fleet {
		pk := product.Normalize(fv.Product)
		v := h.otaGate.Build(ctx, fv.Version, pk)
		rows = append(rows, otaBuildRow{
			BuildID: fv.Version, ProductKey: pk, ProductLabel: product.Label(pk),
			Devices: fv.DeviceCount, OK: v.OK, Reason: v.Reason, Source: v.Source,
			Tracked: tracked[pk+"|"+fv.Version],
		})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Devices > rows[j].Devices })
	return rows
}

// SettingsSetKioskAllowlist replaces the app locked-app allowlist from a free-form
// field (patterns separated by newlines, commas, or spaces). Empty = allow all.
func (h *Handler) SettingsSetKioskAllowlist(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	seen := map[string]bool{}
	var pats []string
	for _, tok := range strings.FieldsFunc(r.FormValue("allowlist"), func(c rune) bool {
		return c == '\n' || c == '\r' || c == ',' || c == ' ' || c == '\t' || c == ';'
	}) {
		tok = strings.TrimSpace(tok)
		if tok == "" || seen[tok] {
			continue
		}
		seen[tok] = true
		pats = append(pats, tok)
	}
	if err := h.cfg.SetKioskAllowlist(pats); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.settingsRedirect(w, r)
}

// otaConfigLastView / otaConfigNextView back the discovery-config card: what the fleet is
// reading right now, and the window the next publish would carry.
func otaConfigLastView(cfg *config.Config) map[string]string {
	body, at := cfg.OTAConfigLast()
	return map[string]string{"Body": body, "At": at}
}

func otaConfigNextView(cfg *config.Config) map[string]any {
	o := cfg.OTAConfigOptions()
	start, end := otaconfig.Window(o, time.Now())
	return map[string]any{"Start": start, "End": end, "TZ": o.Timezone}
}
