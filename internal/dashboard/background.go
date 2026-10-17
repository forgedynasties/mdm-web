package dashboard

import (
	"context"
	_ "image/gif"
	_ "image/jpeg"
	"log"
	"time"

	"mdm/internal/db"
	"mdm/internal/notify"

	"github.com/google/uuid"
)

// RunRecentAlerts evaluates the recent-tier rules (point-in-time + rate/sustained T7
// matrix rules) and dispatches any new alerts. Called every minute from main.go so
// 5-minute-offline / SoC-now / discharge-rate alerts fire promptly, not hourly.
func (h *Handler) RunRecentAlerts(ctx context.Context) {
	// The offline-family rules must not page a device that is still present, so pass
	// the present set (see offlineHitsQuery / the alerts-offline design note): live
	// sockets, and MDM-lite devices that checked in recently (they never hold one).
	connSet := h.hub.ConnectedIDsForDisplay()
	connected := make([]uuid.UUID, 0, len(connSet))
	for id := range connSet {
		connected = append(connected, id)
	}
	created, resolved, err := h.db.EvaluateRecentAlerts(ctx, connected)
	if err != nil {
		log.Printf("[recent-alerts] evaluate: %v", err)
		return
	}
	if len(created) > 0 || resolved > 0 {
		log.Printf("[recent-alerts] %d new, %d resolved", len(created), resolved)
		h.hub.PublishAlertUpdate()
	}
	// A crash/ANR alert carries the real diagnostic — the DropBox stack/ANR/tombstone
	// trace the client now sends with the crash event (see LatestCrashTrace, rendered
	// on the alert). We deliberately do NOT auto-capture logcat here: a delayed *:E grab
	// fires a minute+ after the crash and returns ambient system noise, not the crash.
	h.dispatchAlertNotifications(ctx, created)
	// A paged problem that has fully cleared gets one "resolved" message, and the
	// overnight digest goes out at opening; both are cheap until there is work.
	h.alerts.DispatchResolved(ctx)
	h.alerts.MaybeSendMorningDigest(ctx, connected)
	// Lock rules are enforced, not applied once: devices that joined or left a
	// rule's target, or installed its app, are brought in line every minute.
	h.reconcileKiosk(ctx)
	h.checkKeyResets(ctx)
	// A device locked again by any path (its dialog, bulk kiosk, a rule save) answers
	// its "taken unlocked" alert.
	if n, err := h.db.ResolveAnsweredKioskExitAlerts(ctx); err == nil && n > 0 {
		h.hub.PublishAlertUpdate()
	}
}

// dispatchAlertNotifications routes freshly-created alerts to the configured
// channels. The routing logic is shared with the device API (new-device alerts)
// via internal/alerts.
func (h *Handler) dispatchAlertNotifications(ctx context.Context, created []db.AlertNotification) {
	h.alerts.Dispatch(ctx, created)
}

// RunHousekeeping applies the configured retention policies. (Devices are no longer
// auto-hidden: a device that stops reporting stays in the fleet until it is retired.)
// Safe to call repeatedly; each step is a no-op when its setting is 0.
func (h *Handler) RunHousekeeping(ctx context.Context) {
	// Roll up daily stats first — refresh today and finalize yesterday — so checkins
	// are always aggregated before the retention prune below can delete them.
	now := time.Now().UTC() // roll by UTC day (matches the DB session tz) for a stable boundary
	for _, day := range []time.Time{now, now.AddDate(0, 0, -1)} {
		if _, err := h.db.RollupDailyStatsFor(ctx, day); err != nil {
			log.Printf("[housekeeping] rollup daily stats %s: %v", day.Format("2006-01-02"), err)
		}
	}
	// Recompute battery discharge cycles from the freshly rolled-up daily swings.
	if n, err := h.db.RecomputeDischargeCycles(ctx); err != nil {
		log.Printf("[housekeeping] recompute discharge cycles: %v", err)
	} else if n > 0 {
		log.Printf("[housekeeping] recomputed discharge cycles for %d device(s)", n)
	}
	// Evaluate daily-tier alert rules against the freshly rolled-up stats, then notify.
	if created, resolved, err := h.db.EvaluateAlerts(ctx, h.connectedSlice()); err != nil {
		log.Printf("[housekeeping] evaluate alerts: %v", err)
	} else {
		if len(created) > 0 || resolved > 0 {
			log.Printf("[housekeeping] alerts: %d new, %d resolved", len(created), resolved)
			h.hub.PublishAlertUpdate()
		}
		h.dispatchAlertNotifications(ctx, created)
	}
	h.maybeSendDigest(ctx)
	h.applyPrunes(ctx)
	h.downsampleOldCheckins(ctx)
	// The check-in backfill, repair and strip jobs are gone with the checkins table
	// (retired 24 Sep 2026): samples and state history were filled from it in one sitting
	// first, and the strip job had been deleting the last copy of readings it wrongly
	// believed were in device_samples.
}

// downsampleCheckinsDaysPerRun is how much history one housekeeping pass thins. Three
// days an hour works a long backlog off in a day or so without ever being the heaviest
// thing on the box, and once caught up there is only ever one new day to do.
const downsampleCheckinsDaysPerRun = 3

// downsampleOldCheckins thins history past the configured age to one row per device per
// bucket. Unlike retention it keeps the period — just at a coarser resolution — so a
// venue's week stays readable years later at a fraction of the rows.
func (h *Handler) downsampleOldCheckins(ctx context.Context) {
	days := h.cfg.CheckinDownsampleDays()
	if days <= 0 {
		return // an admin turned it off; keep every row at full resolution
	}
	sec := h.cfg.CheckinDownsampleSec()
	// The check-in downsampling setting, applied to the history that replaced checkins.
	srows, sdays, err := h.db.DownsampleSamples(ctx, days, sec, downsampleCheckinsDaysPerRun)
	if err != nil {
		log.Printf("[housekeeping] downsample samples: %v", err)
		return
	}
	if srows > 0 {
		log.Printf("[housekeeping] downsampled %d sample row(s) across %d day(s) older than %dd to 1 per %ds",
			srows, sdays, days, sec)
	}
}

// legacyStripPct is the legacy check-in cleanup's progress for the Settings page.
func (h *Handler) legacyStripPct(ctx context.Context) int {
	// The legacy strip ran to completion before the checkins table was retired.
	return 100
}

// applyPrunes deletes check-ins and logcat results past their retention windows
// (each a no-op when disabled). These are destructive — rows are removed.
func (h *Handler) applyPrunes(ctx context.Context) {
	if d := h.cfg.CheckinRetentionDays(); d > 0 {
		// The check-in retention setting governs the history that replaced checkins.
		if n, err := h.db.PruneShaped(ctx, d); err != nil {
			log.Printf("[retention] prune samples/events: %v", err)
		} else if n > 0 {
			log.Printf("[retention] pruned %d sample/event row(s) older than %dd", n, d)
		}
	}
	// Fast temperature readings are a day's close-up, whatever the retention setting;
	// device_samples keeps the minute-by-minute history.
	if n, err := h.db.PruneTempFast(ctx); err != nil {
		log.Printf("[retention] prune fast temperature: %v", err)
	} else if n > 0 {
		log.Printf("[retention] pruned %d fast temperature row(s) older than %dh", n, db.TempFastKeepHours)
	}
	// The battery log and charge climbs (client 1.10.1) keep 180 days: enough for a
	// health trend over seasons, without the table growing for ever.
	if n, err := h.db.PruneBatteryData(ctx, 180*24*time.Hour); err != nil {
		log.Printf("[retention] prune battery data: %v", err)
	} else if n > 0 {
		log.Printf("[retention] pruned %d battery row(s) older than 180 days", n)
	}
	if d := h.cfg.LogcatRetentionDays(); d > 0 {
		if n, err := h.db.PruneLogcat(ctx, d); err != nil {
			log.Printf("[retention] prune logcat: %v", err)
		} else if n > 0 {
			log.Printf("[retention] pruned %d logcat row(s) older than %dd", n, d)
		}
	}
	if n, err := h.db.PruneResolvedAlerts(ctx, resolvedAlertRetentionDays); err != nil {
		log.Printf("[retention] prune resolved alerts: %v", err)
	} else if n > 0 {
		log.Printf("[retention] pruned %d resolved alert(s) older than %dd", n, resolvedAlertRetentionDays)
	}
	if err := h.db.DeleteExpiredSessions(ctx); err != nil {
		log.Printf("[retention] prune sessions: %v", err)
	}
	// A wifi_config password outlives its command's delivery window by nothing.
	if n, err := h.db.PruneCommandSecrets(ctx); err != nil {
		log.Printf("[retention] prune command secrets: %v", err)
	} else if n > 0 {
		log.Printf("[retention] pruned %d command secret(s) past delivery", n)
	}
}

// resolvedAlertRetentionDays is how long resolved alerts are kept before the
// housekeeping prune deletes them (open/acknowledged alerts are never pruned).
const resolvedAlertRetentionDays = 90

// maybeSendDigest posts a once-a-day fleet digest — the explained fleet health score —
// to the alert webhook. It is a no-op unless the digest toggle and a webhook are both
// configured. It fires on the first housekeeping run at or after 08:00 local each day,
// so it reflects daytime service rather than overnight charging.
func (h *Handler) maybeSendDigest(ctx context.Context) {
	if !h.cfg.DailyDigestEnabled() {
		return
	}
	url := h.cfg.AlertWebhookURL()
	if url == "" {
		return
	}
	now := time.Now()
	today := now.Format("2006-01-02")
	if h.lastDigestDay == today || now.Hour() < 8 {
		return
	}
	groups, err := h.db.GetRestaurantHealth(ctx, h.connectedSlice(), 1) // Daily Report: one day
	if err != nil {
		log.Printf("[digest] group health: %v", err)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	fleetScore, den := 0, 0
	for _, g := range groups {
		fleetScore += g.Score * g.DeviceCount
		den += g.DeviceCount
	}
	if den > 0 {
		fleetScore /= den
	} else {
		fleetScore = 100
	}
	crashStats, _ := h.db.GetFleetCrashStats(cctx, 4)
	msg := healthExplainText(fleetScore, 0, explainFleetHealth(groups, crashStats.ByRestaurant))
	if err := notify.SendWebhook(ctx, url, "*Daily fleet digest*\n"+msg); err != nil {
		log.Printf("[digest] webhook: %v", err)
		return
	}
	h.lastDigestDay = today // only on success, so a transient failure retries next hour
	log.Printf("[digest] sent daily fleet digest")
}
