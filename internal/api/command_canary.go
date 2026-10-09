package api

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/google/uuid"
	"mdm/internal/db"
)

// Proving control, rather than assuming it.
//
// Every signal this server had was liveness: a socket, a check-in, a fresh temperature.
// None of them means a command would run. AT070AABU00077 had all three for fifteen hours
// while being completely uncommandable (9 Oct 2026), and nothing on any page disagreed.
//
// The probe is a no-op shell line, enqueued at most once a day per device, in its own lane
// so it can never delay an operator's command (see GetPendingCommandsForDevice). Its
// terminal ack stamps devices.last_round_trip_ok_at — which turns "we think the fleet is
// manageable" into a column, and makes control_stale answerable.
const (
	// canaryStaleAfter is how long we tolerate having no proof for a device before asking
	// it for some. Just under a day, so a device probed yesterday is probed again today
	// rather than drifting an hour later each time.
	canaryStaleAfter = 20 * time.Hour
	// canarySeenWithin keeps probes off devices that are simply off: a powered-down tablet
	// is ordinary Offline, which the badge and the offline alert already cover.
	canarySeenWithin = 2 * time.Hour
	// canaryBatch caps a single tick. The job runs every minute, so a fleet works through
	// itself over a few minutes instead of enqueueing hundreds of commands at once.
	canaryBatch = 25
	// controlStaleAfter is when a device that is reporting with no proven round trip stops
	// being a gap in our knowledge and becomes a fleet problem.
	controlStaleAfter = 48 * time.Hour
)

var canaryPayload = json.RawMessage(`{"cmd":"echo mdm-canary"}`)

// RunCommandCanary enqueues control probes for devices with no recent proof. Opt-in: one
// queued command per device per day is cheap but not free, and that is a decision rather
// than a default (config command_canary).
func (h *Handler) RunCommandCanary(ctx context.Context) {
	if !h.cfg.CommandCanary() {
		return
	}
	due, err := h.db.DevicesNeedingCanary(ctx, canaryStaleAfter, canarySeenWithin, canaryBatch)
	if err != nil {
		log.Printf("[canary] DevicesNeedingCanary: %v", err)
		return
	}
	for _, c := range due {
		cmd, err := h.db.CreateCommandBy(ctx, "canary", "", canaryPayload, "devices",
			[]uuid.UUID{c.DeviceID}, "control probe")
		if err != nil {
			log.Printf("[canary] %s: %v", c.Serial, err)
			continue
		}
		// Push it if we can, so a healthy device answers in seconds; a device with no
		// socket collects it on its next poll, which is exactly the case being measured.
		h.pushCommand(ctx, cmd, "devices", []uuid.UUID{c.DeviceID})
	}
	if len(due) > 0 {
		log.Printf("[canary] probed %d device(s) with no recent proof of control", len(due))
	}
}

// RunControlStaleAlerts raises control_stale for devices that are reporting while nothing
// has completed on them for controlStaleAfter. With the canary on this means the probe
// itself never came back, which is as strong a statement as this system can make: the
// device is alive and we cannot make it do anything.
func (h *Handler) RunControlStaleAlerts(ctx context.Context) {
	// Evidence ends the condition: close anything that has since proven a round trip,
	// before raising new ones. An alert that cannot clear itself trains people to ignore
	// the list, which costs more than the alert was worth.
	if n, err := h.db.ResolveProvenControlAlerts(ctx, controlStaleAfter); err != nil {
		log.Printf("[control-stale] ResolveProvenControlAlerts: %v", err)
	} else if n > 0 {
		log.Printf("[control-stale] %d device(s) proved control again — alerts resolved", n)
		h.hub.PublishAlertUpdate()
	}

	stale, err := h.db.StaleControlDevices(ctx, controlStaleAfter, canarySeenWithin)
	if err != nil {
		log.Printf("[control-stale] StaleControlDevices: %v", err)
		return
	}
	for _, s := range stale {
		// "Never" and "not lately" are different statements, and deriving a duration from
		// the device row's creation time for the first case produced alerts like "reporting
		// for 18m with nothing completing" against a 48-hour threshold — which reads as a
		// bug in the alert rather than a fact about the device.
		summary := "Reporting, and no command has ever completed on it. It is alive and we have no evidence it can be commanded at all."
		detail := map[string]any{"last_round_trip": nil}
		if !s.LastOK.IsZero() {
			summary = "Reporting, but nothing we sent has completed in " + roughDuration(time.Since(s.LastOK)) +
				". It is alive and we have no recent evidence it can be commanded."
			detail["last_round_trip"] = s.LastOK.UTC()
		}
		inserted, escalated, err := h.db.CreateOrEscalateAlert(ctx, nil, "control_stale", s.DeviceID,
			"warning", summary, detail)
		if err != nil {
			log.Printf("[control-stale] alert for %s: %v", s.Serial, err)
			continue
		}
		if inserted || escalated {
			eventAt := s.LastOK
			if eventAt.IsZero() {
				eventAt = time.Now()
			}
			h.alerts.Dispatch(ctx, []db.AlertNotification{{
				Type: "control_stale", Severity: "warning", Summary: summary,
				Serial: s.Serial, DeviceID: s.DeviceID, EventAt: eventAt.UTC(), Escalated: escalated,
			}})
			h.hub.PublishAlertUpdate()
		}
	}
}
