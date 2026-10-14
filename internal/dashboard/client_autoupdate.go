package dashboard

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/config"
	"mdm/internal/db"
)

// Keeping the client up to date is a standing instruction, not a button press.
//
// "Update all behind" queued an install for whoever was online at that second. On a
// restaurant fleet that is most of the fleet missed: units are off overnight, a venue is
// between shifts, a box sits unplugged for a week. The count never reached zero and
// somebody had to remember to come back.
//
// The hosted build already says what the target is. This job acts on it: every tick, the
// devices that are present, behind, and inside their own quiet window are handed the
// build. A device that was away collects it the first time it appears, by itself.
//
// It queues only for devices that have checked in recently, on purpose. Queuing for the
// whole fleet would pile up commands nobody can deliver and make the job's own output
// (what is waiting, what is stuck) meaningless; the job runs every minute, so a device
// coming back is served within a minute of being seen.
const (
	// autoUpdateBatch caps one tick per slot, so a fleet rolls over a few minutes
	// rather than enqueueing hundreds of installs in one second.
	autoUpdateBatch = 20
	// autoUpdateSeenFactor × the check-in interval is "present enough to be worth
	// queuing for" — the same multiple the fleet list calls offline at.
	autoUpdateSeenFactor = 3
)

// RunClientAutoUpdate hands the hosted build to devices that are behind it, for every
// slot whose policy is on and not paused. Safe to call every minute.
func (h *Handler) RunClientAutoUpdate(ctx context.Context) {
	var active []string
	for _, slot := range clientSlotOrder {
		if h.cfg.ClientAutoUpdate(slot).Active() {
			active = append(active, slot)
		}
	}
	if len(active) == 0 {
		return
	}
	devices, err := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 10000, "serial", "asc")
	if err != nil {
		log.Printf("[client-auto-update] ListDevices: %v", err)
		return
	}
	seenWithin := time.Duration(h.cfg.CheckinInterval()*autoUpdateSeenFactor) * time.Second

	for _, slot := range active {
		pol := h.cfg.ClientAutoUpdate(slot)
		build, hosted := h.cfg.AgentAPKSlot(slot)
		if !hosted || build.Package == "" || build.VersionCode == 0 {
			continue
		}
		name, known := AgentAPKSlots[slot]
		if !known {
			continue
		}
		apkURL := h.publicBase() + "/agent/" + name

		// Candidates first, then one query for what is already in flight: a device
		// mid-install is not behind in any sense that matters here.
		var cand []*db.Device
		var ids []uuid.UUID
		for i := range devices {
			d := &devices[i]
			if agentSlotFor(d) != slot {
				continue // its image trusts another key; it is never offered this build
			}
			if time.Since(d.LastSeenAt) > seenWithin {
				continue // away — it gets the build the tick after it reappears
			}
			_, code := clientVersionOf(d)
			if code == 0 || !versionComparable(d, build.Package) || code >= build.VersionCode {
				continue
			}
			if !pol.Allows(deviceNow(d)) {
				continue
			}
			cand = append(cand, d)
			ids = append(ids, d.ID)
		}
		if len(cand) == 0 {
			continue
		}
		inFlight, err := h.db.InFlightAgentUpdates(ctx, ids)
		if err != nil {
			log.Printf("[client-auto-update] %s: in-flight: %v", slot, err)
			continue
		}
		payload := agentUpdatePayload(build.Package, build.Version, build.SHA256Hex)
		sent := 0
		for _, d := range cand {
			if sent >= autoUpdateBatch {
				break
			}
			if _, busy := inFlight[d.ID]; busy {
				continue
			}
			cmd, err := h.db.CreateCommandBy(ctx, "app_update", apkURL, payload, "devices",
				[]uuid.UUID{d.ID}, "auto-update")
			if err != nil {
				log.Printf("[client-auto-update] %s %s: %v", slot, d.SerialNumber, err)
				continue
			}
			_ = cmd
			sent++
		}
		if sent > 0 {
			log.Printf("[client-auto-update] %s: queued %s for %d device(s)", slot, build.Version, sent)
		}
	}
}

// deviceNow is the wall clock where the device stands, so a 02:00–05:00 window means
// 02:00 in that restaurant and not on the server. An unknown or bad zone falls back to
// the server's, which is better than skipping the device forever.
func deviceNow(d *db.Device) time.Time {
	tz := extraString(d.LatestExtra, "timezone")
	if tz == "" {
		return time.Now()
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Now()
	}
	return time.Now().In(loc)
}


// ClientAutoUpdateSet records the standing instruction for one slot. It changes what the
// server will do unprompted, so it is admin-only and written to the audit log.
func (h *Handler) ClientAutoUpdateSet(w http.ResponseWriter, r *http.Request) {
	slot := strings.TrimSpace(r.FormValue("slot"))
	if _, ok := AgentAPKSlots[slot]; !ok {
		http.Error(w, "Unknown client slot", http.StatusBadRequest)
		return
	}
	pol := config.ClientAutoUpdate{
		On:          r.FormValue("on") == "1",
		Paused:      r.FormValue("paused") == "1",
		WindowStart: strings.TrimSpace(r.FormValue("window_start")),
		WindowEnd:   strings.TrimSpace(r.FormValue("window_end")),
		By:          h.currentUsername(r),
		At:          time.Now(),
	}
	if err := h.cfg.SetClientAutoUpdate(slot, pol); err != nil {
		log.Printf("[client-auto-update] save %s: %v", slot, err)
		http.Error(w, "Could not save", http.StatusInternalServerError)
		return
	}
	what := "off"
	if pol.Active() {
		what = "on"
		if pol.WindowStart != "" && pol.WindowEnd != "" {
			what += " (" + pol.WindowStart + "–" + pol.WindowEnd + ")"
		}
	} else if pol.On {
		what = "paused"
	}
	h.audit(r, "settings.client_auto_update", slot, what)
	http.Redirect(w, r, "/lineup?client="+slot+"&flash=rollout", http.StatusFound)
}
