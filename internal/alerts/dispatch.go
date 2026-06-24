// Package alerts routes freshly-created alert instances to the configured
// outbound notification channels. It is shared by the dashboard (which fires
// alerts from rule evaluation) and the device API (which fires the
// "new_device" onboarding alert on a device's first checkin), so both surfaces
// honour the same channel routing, severity filtering, and active-window rules.
package alerts

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/notify"
)

// Dispatcher delivers alert notifications using the alert-channel configuration.
type Dispatcher struct {
	db  *db.DB
	cfg *config.Config
}

// NewDispatcher builds a Dispatcher from the shared DB and live config.
func NewDispatcher(d *db.DB, cfg *config.Config) *Dispatcher {
	return &Dispatcher{db: d, cfg: cfg}
}

// Dispatch routes freshly-created alerts to every matching enabled channel:
// severity ≥ the channel's minimum, realtime mode, and the channel's active
// window currently open (fleet-default window). Falls back to the legacy single
// AlertWebhookURL when no channels are configured, so upgrades keep working.
func (dp *Dispatcher) Dispatch(ctx context.Context, created []db.AlertNotification) {
	if len(created) == 0 {
		return
	}
	channels, err := dp.db.ListAlertChannels(ctx, true)
	if err != nil {
		log.Printf("[alert] list channels: %v", err)
		return
	}
	// Back-compat: no channels yet → use the legacy single webhook for everything.
	if len(channels) == 0 {
		if url := dp.cfg.AlertWebhookURL(); url != "" {
			for _, n := range created {
				if err := notify.SendWebhook(ctx, url, FormatAlert(n)); err != nil {
					log.Printf("[alert] webhook failed: %v", err)
				}
			}
		}
		return
	}
	for _, c := range channels {
		if c.URL == "" || c.Mode != "realtime" {
			continue // digest channels are handled by the daily digest
		}
		if !dp.db.FleetWindowActive(ctx, c.ActiveWindow) {
			continue
		}
		min := db.SeverityRank(c.MinSeverity)
		for _, n := range created {
			if db.SeverityRank(n.Severity) < min {
				continue
			}
			if !c.AllowsType(n.Type) {
				continue // this channel opted out of this alert type
			}
			if err := SendToChannel(ctx, c, n); err != nil {
				log.Printf("[alert] channel %q (%s) failed: %v", c.Name, c.Kind, err)
			}
		}
	}
}

// SendToChannel delivers one notification to a channel using the right payload
// shape for its kind: Microsoft Teams gets an Adaptive Card; everything else
// (Slack / Discord / Mattermost) gets the {"text":...} webhook.
func SendToChannel(ctx context.Context, c db.AlertChannel, n db.AlertNotification) error {
	if c.Kind == "teams" {
		emoji := "🔵"
		switch n.Severity {
		case "critical":
			emoji = "🔴"
		case "warning":
			emoji = "🟠"
		}
		title := fmt.Sprintf("%s %s — %s", emoji, strings.ToUpper(n.Severity), n.Serial)
		return notify.SendTeams(ctx, c.URL, title, n.Summary, n.Severity, formatWhen(n.EventAt, n.Timezone))
	}
	return notify.SendWebhook(ctx, c.URL, FormatAlert(n))
}

// FormatAlert renders a notification line with a severity emoji/prefix.
func FormatAlert(n db.AlertNotification) string {
	emoji := "🔵"
	switch n.Severity {
	case "critical":
		emoji = "🔴"
	case "warning":
		emoji = "🟠"
	}
	line := fmt.Sprintf("%s [%s] %s — %s", emoji, strings.ToUpper(n.Severity), n.Serial, n.Summary)
	if w := formatWhen(n.EventAt, n.Timezone); w != "" {
		line += " · 🕒 " + w
	}
	return line
}

// formatWhen renders an event time in the device's local timezone (falling back to
// UTC when the zone is empty or unknown). Returns "" for a zero time.
func formatWhen(at time.Time, tz string) string {
	if at.IsZero() {
		return ""
	}
	loc := time.UTC
	if tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	return at.In(loc).Format("Mon 2 Jan 3:04 PM MST")
}
