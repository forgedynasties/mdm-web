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

	"github.com/google/uuid"

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
//
// Channels get problems, not alerts: the alerts of one device become one message
// naming the likely cause, devices at one restaurant that went offline together
// become one restaurant message, and a device whose problem a channel was already
// told about is not told again for each new symptom. A crash whose signature is on
// several devices is a release issue, listed on the Alerts page, not paged per device.
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
	devs := make([]uuid.UUID, 0, len(created))
	ids := make([]uuid.UUID, 0, len(created))
	for _, n := range created {
		devs = append(devs, n.DeviceID)
		ids = append(ids, n.AlertID)
	}
	paged, err := dp.db.PagedDevices(ctx, devs, ids)
	if err != nil {
		log.Printf("[alert] paged devices: %v", err)
	}
	info, err := dp.db.DeviceNotifyInfos(ctx, devs)
	if err != nil {
		log.Printf("[alert] device info: %v", err)
	}
	var fresh []db.AlertNotification
	for _, n := range created {
		if paged[n.DeviceID] {
			continue
		}
		if n.Type == "device_crash" {
			if sum, _ := n.Detail["summary"].(string); dp.db.IsReleaseCrash(ctx, sum) {
				continue
			}
		}
		fresh = append(fresh, n)
	}
	var sent []uuid.UUID
	for _, c := range channels {
		if c.URL == "" || c.Mode != "realtime" {
			continue // digest channels get the morning digest
		}
		if !dp.db.FleetWindowActive(ctx, c.ActiveWindow) {
			continue
		}
		min := db.SeverityRank(c.MinSeverity)
		var keep []db.AlertNotification
		for _, n := range fresh {
			if db.SeverityRank(n.Severity) < min {
				continue
			}
			if !c.AllowsType(n.Type) {
				continue // this channel opted out of this alert type
			}
			keep = append(keep, n)
		}
		for _, m := range buildProblemMessages(keep, info) {
			if err := send(ctx, c, m.Severity, m.Title, m.Text, m.When); err != nil {
				log.Printf("[alert] channel %q (%s) failed: %v", c.Name, c.Kind, err)
				continue
			}
			sent = append(sent, m.IDs...)
		}
	}
	if err := dp.db.MarkAlertsNotified(ctx, sent); err != nil {
		log.Printf("[alert] mark notified: %v", err)
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
