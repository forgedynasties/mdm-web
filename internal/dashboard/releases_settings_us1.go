package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"

	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/product"
)

// Update settings, US1 (picked 18 Nov 2026): a list of the settings that decide how
// devices find and take updates, each with its current value as a hint, and the picked
// one explained on the right with what it would do tonight. The settings keep their own
// homes (window on Lineup, client policy on Clients, venue gates on the venue page, one
// home each); only the MDM OTA cutoff is edited here.

type usRailRow struct {
	Key, Title, Hint, Tone string
}

type UsPolicy struct {
	Label  string
	On     bool
	Paused bool
	Window string
	By     string
	At     string
}

type usSlotView struct {
	UsPolicy
	Slot    string
	Hosted  string // hosted build's version, "" = none hosted
	Behind  int    // present and behind the hosted build
	Tonight int
	Href    string
}

type usModelN struct {
	Label string
	N     int
}

type usVenueView struct {
	ID     uuid.UUID
	Name   string
	Detail string
	Paused bool
}

type usView struct {
	Sel      string
	Rail     []usRailRow
	Tonight  int
	ClientN  int
	Firmware UsPolicy
	Apps     UsPolicy
	FwN      int
	FwModels []usModelN
	FwNote   string // why the count is not a number
	Slots    []usSlotView
	Venues   []usVenueView
	Cutoffs  []otaCutoffRow
	Builds   []otaBuildRow
	Legacy   int // builds the rule sends to legacy OTA
}

func usPolicyOf(label string, p config.ClientAutoUpdate) UsPolicy {
	v := UsPolicy{Label: label, On: p.On, Paused: p.Paused, Window: hhmmRange(p.WindowStart, p.WindowEnd), By: p.By}
	if !p.At.IsZero() {
		v.At = p.At.Format("2 Jan 2006")
	}
	return v
}

func usState(p UsPolicy) string {
	switch {
	case !p.On:
		return "Off"
	case p.Paused:
		return "Paused"
	}
	return "On"
}

func usPlural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// buildUS1 gathers the page. The "tonight" counts use the selection the update jobs use
// (RunRoleFirmware, RunClientAutoUpdate) without their window test: a device that is
// online now and behind its target would be handed it inside its window. Devices that are
// offline now, or already mid-install, are not counted.
func (h *Handler) buildUS1(ctx context.Context, sel string) usView {
	now := time.Now()
	v := usView{Sel: sel}
	v.Firmware = usPolicyOf("Prod firmware", h.cfg.RoleRollout(config.RolloutFirmware))
	v.Apps = usPolicyOf("Role apps", h.cfg.RoleRollout(config.RolloutApps))
	v.Cutoffs = h.otaCutoffRows(ctx)
	v.Builds = h.otaBuildRows(ctx)
	for _, b := range v.Builds {
		if !b.OK {
			v.Legacy++
		}
	}

	devices, err := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 100000, "serial", "asc")
	if err != nil {
		v.FwNote = "The fleet list could not be read, so nothing is counted."
	}
	gate := h.loadVenueGate(ctx)
	pausedVenue := func(d *db.Device) bool {
		if d.RestaurantID == nil {
			return false
		}
		p, ok := gate.pols[*d.RestaurantID]
		return ok && p.PausedAt(now)
	}
	seenWithin := time.Duration(h.cfg.CheckinInterval()*autoUpdateSeenFactor) * time.Second

	// Client slots.
	for _, slot := range clientSlotOrder {
		pol := h.cfg.ClientAutoUpdate(slot)
		sv := usSlotView{UsPolicy: usPolicyOf(clientSlotLabels[slot], pol), Slot: slot, Href: "/clients?client=" + slot}
		// firmware-gms is folded into Firmware MDM: same policy, measured against its own build.
		for _, s := range append([]string{slot}, map[bool][]string{true: {"firmware-gms"}}[slot == "firmware-qcom"]...) {
			build, hosted := h.cfg.AgentAPKSlot(s)
			if !hosted || build.Package == "" || build.VersionCode == 0 {
				continue
			}
			if s == slot {
				sv.Hosted = build.Version
			}
			for i := range devices {
				d := &devices[i]
				if agentSlotFor(d) != s || time.Since(d.LastSeenAt) > seenWithin {
					continue
				}
				_, code := clientVersionOf(d)
				if code == 0 || !versionComparable(d, build.Package) || code >= build.VersionCode {
					continue
				}
				sv.Behind++
				if pol.Active() && !pausedVenue(d) {
					sv.Tonight++
				}
			}
		}
		v.Tonight += sv.Tonight
		v.ClientN += sv.Tonight
		v.Slots = append(v.Slots, sv)
	}

	// Prod firmware.
	if pol := h.cfg.RoleRollout(config.RolloutFirmware); pol.Active() && err == nil {
		v.FwModels = h.firmwareTonight(ctx, devices, seenWithin, pausedVenue)
		for _, m := range v.FwModels {
			v.FwN += m.N
		}
		v.Tonight += v.FwN
	}

	// Venue gates: the venues that do not simply follow the fleet.
	pols, _ := h.db.VenueUpdatePolicies(ctx)
	if len(pols) > 0 {
		names := map[uuid.UUID]string{}
		if rs, e := h.db.ListRestaurants(ctx); e == nil {
			for _, r := range rs {
				names[r.ID] = r.Name
			}
		}
		for id, p := range pols {
			vv := usVenueView{ID: id, Name: names[id], Paused: p.PausedAt(now)}
			if vv.Name == "" {
				vv.Name = id.String()[:8]
			}
			switch {
			case vv.Paused && p.PausedUntil != nil:
				vv.Detail = "Paused until " + p.PausedUntil.Format("Mon 2 Jan, 15:04")
			case vv.Paused:
				vv.Detail = "Paused until someone resumes"
			case p.Mode == db.VenueUpdateClosed:
				vv.Detail = "Only while the venue is closed"
			case p.Mode == db.VenueUpdateOwn:
				vv.Detail = "Own window " + hhmmRange(p.Start, p.End)
			default:
				continue
			}
			v.Venues = append(v.Venues, vv)
		}
		sort.Slice(v.Venues, func(i, j int) bool {
			if v.Venues[i].Paused != v.Venues[j].Paused {
				return v.Venues[i].Paused
			}
			return v.Venues[i].Name < v.Venues[j].Name
		})
	}

	v.Rail = usRail(v)
	return v
}

// firmwareTonight mirrors RunRoleFirmware's candidate selection (no window, no
// in-flight check) and counts per model.
func (h *Handler) firmwareTonight(ctx context.Context, devices []db.Device, seenWithin time.Duration, pausedVenue func(*db.Device) bool) []usModelN {
	chans, err := h.db.ListFirmwareChannels(ctx)
	if err != nil || len(chans) == 0 {
		return nil
	}
	modes, err := h.db.FirmwareModes(ctx)
	if err != nil {
		return nil
	}
	labs, err := h.db.LabSessions(ctx)
	if err != nil {
		return nil
	}
	var out []usModelN
	for _, ch := range chans {
		if ch.ReleaseID == nil || ch.Version == "" || db.FirmwareModeOf(modes, ch.Model) != db.FirmwareProd {
			continue
		}
		rel, err := h.db.GetRelease(ctx, *ch.ReleaseID)
		if err != nil || rel.Status != "published" {
			continue
		}
		var cand []uuid.UUID
		for i := range devices {
			d := &devices[i]
			if d.Hidden || d.Retired() || d.IsDPC() || d.RestaurantID == nil || modelKey(d) != ch.Model {
				continue
			}
			if _, inLab := labs[d.ID]; inLab {
				continue
			}
			if time.Since(d.LastSeenAt) > seenWithin || !firmwareBehind(d.BuildID, rel.Version) || pausedVenue(d) {
				continue
			}
			cand = append(cand, d.ID)
		}
		if len(cand) == 0 {
			continue
		}
		if cand, err = h.db.RemoveDowngradeTargets(ctx, rel.ID, cand); err != nil {
			continue
		}
		if cand, err = h.db.RemoveInapplicableTargets(ctx, rel.ID, cand, false); err != nil {
			continue
		}
		cand, _ = h.filterOTACapable(ctx, cand)
		if len(cand) > 0 {
			out = append(out, usModelN{Label: product.Label(ch.Model) + " to " + ch.Version, N: len(cand)})
		}
	}
	return out
}

func usRail(v usView) []usRailRow {
	fwTone := "g"
	if !v.Firmware.On || v.Firmware.Paused {
		fwTone = "o"
	}
	clientTone, clientHint := "g", ""
	for _, s := range v.Slots {
		if clientHint != "" {
			clientHint += " · "
		}
		clientHint += s.Label + " " + usState(s.UsPolicy)
		if !s.On || s.Paused {
			clientTone = "o"
		}
	}
	cutHint, cutTone := "No cutoff set", "c"
	for _, c := range v.Cutoffs {
		if c.Current > 0 {
			cutHint, cutTone = "Cutoff set", "g"
			break
		}
	}
	if v.Legacy > 0 {
		cutHint, cutTone = usPlural(v.Legacy, "build", "builds")+" legacy only", "o"
	}
	gateHint, gateTone := "Every venue follows the fleet", "g"
	paused := 0
	for _, g := range v.Venues {
		if g.Paused {
			paused++
		}
	}
	if len(v.Venues) > 0 {
		gateHint = usPlural(len(v.Venues), "venue", "venues") + " set apart"
		if paused > 0 {
			gateHint = usPlural(paused, "venue", "venues") + " paused · " + gateHint
			gateTone = "o"
		}
	}
	return []usRailRow{
		{"window", "When devices move", usState(v.Firmware) + " · " + v.Firmware.Window + " · apps " + usState(v.Apps), fwTone},
		{"auto", "Client auto-update", clientHint, clientTone},
		{"cutoff", "MDM OTA support cutoff", cutHint, cutTone},
		{"gates", "Venue gates", gateHint, gateTone},
	}
}

// ReleasesSettingsPage is GET /releases/settings (super admin).
func (h *Handler) ReleasesSettingsPage(w http.ResponseWriter, r *http.Request) {
	sel := r.URL.Query().Get("s")
	switch sel {
	case "window", "auto", "cutoff", "gates":
	default:
		sel = "window"
	}
	h.render(w, r, "releases_settings.html", map[string]any{"Title": "Update settings", "ActivePage": "updates", "US": h.buildUS1(r.Context(), sel)})
}
