package alerts

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/notify"
)

// Cause is the likely fault behind the alerts open on one device: what the problem is
// called and one sentence on why the MDM thinks so.
type Cause struct {
	Key   string // offline | wifi | charging | slow_charge | battery | heat | crash | storage | memory | new | other
	Title string
	Why   string
}

// IsOffline reports whether an alert type means the device stopped reporting.
func IsOffline(t string) bool {
	return t == "offline" || t == "offline_peak" || t == "offline_long"
}

// CauseOf names the problem behind a set of alert types open on one device. The order
// is what an operator should look at first: a device that is not reporting explains
// every stale reading behind it, and charging faults explain the low battery.
func CauseOf(types []string) Cause {
	has := map[string]bool{}
	for _, t := range types {
		has[t] = true
	}
	any := func(ts ...string) bool {
		for _, t := range ts {
			if has[t] {
				return true
			}
		}
		return false
	}
	switch {
	case any("offline", "offline_peak", "offline_long") && any("wifi_weak", "wifi_unstable"):
		return Cause{"wifi", "Wi-Fi dropping", "Its Wi-Fi was weak or dropping, then it stopped checking in. Likely where it sits in the restaurant, or the router."}
	case any("offline", "offline_peak", "offline_long"):
		return Cause{"offline", "Offline", "It has stopped checking in. Check it is powered and on the restaurant's Wi-Fi."}
	case any("charger_flapping", "wlc_dead") || (has["slow_charge_night"] && has["battery_low"]):
		return Cause{"charging", "Charging hardware", "Charging keeps dropping in and out, or the battery barely rises on charge. The wireless pad or the charger is likely faulty."}
	case has["slow_charge_night"]:
		return Cause{"slow_charge", "Slow charging", "It was on charge overnight but the battery barely rose. Check the pad or charger it sits on."}
	case has["battery_low"]:
		return Cause{"battery", "Battery low in service", "The battery is low while the restaurant is open. Put it on charge."}
	case has["wlc_continuous"]:
		return Cause{"charging", "Left on the pad", "It has sat on the wireless pad for hours. Check it is being used, and that the pad is not holding it warm."}
	case has["overheating"]:
		return Cause{"heat", "Overheating", "It is running hotter than the limit. Check for direct sun, a blocked back or an app stuck working."}
	case has["device_crash"]:
		return Cause{"crash", "App crashing", "An app on it keeps crashing. The stack trace is on the alert."}
	case any("storage_low", "storage_warning", "storage_filling"):
		return Cause{"storage", "Storage filling up", "It is running out of space. Large logs or downloads are the usual cause."}
	case any("memory_low", "memory_pressure"):
		return Cause{"memory", "Low memory", "It is short of memory, which makes apps slow or restart."}
	case any("wifi_weak", "wifi_unstable"):
		return Cause{"wifi", "Weak Wi-Fi", "Its Wi-Fi signal is weak or keeps dropping."}
	case has["kiosk_exited"]:
		return Cause{"kiosk", "Taken out of kiosk on site", "Someone used the exit PIN or code on the device. It stays unlocked until someone locks it again."}
	case has["identity_conflict"]:
		return Cause{"identity", "Possible impersonation", "Something used this device's serial without its own key. If the device was just factory reset or had its data cleared, it is asking to come back: reset its key."}
	case has["key_not_registered"]:
		return Cause{"identity", "Key not registered", "Its key was reset over an hour ago and it has not registered a new one. Until it does, the shared key works for it."}
	case has["new_device"]:
		return Cause{"new", "New device", "A device checked in for the first time."}
	}
	t := ""
	if len(types) > 0 {
		t = types[0]
	}
	return Cause{"other", strings.ReplaceAll(t, "_", " "), ""}
}

// problemMsg is one message to a channel: a device's problem, or several devices at one
// restaurant that went offline together.
type problemMsg struct {
	Severity string
	Title    string
	Text     string
	When     string
	IDs      []uuid.UUID
}

func sevEmoji(sev string) string {
	switch sev {
	case "critical":
		return "🔴"
	case "warning":
		return "🟠"
	}
	return "🔵"
}

// buildProblemMessages folds notifications into one message per device, and folds the
// devices of one restaurant that went offline in the same pass into one restaurant
// message: five tablets dropping together is the restaurant's network, not five faults.
func buildProblemMessages(ns []db.AlertNotification, info map[uuid.UUID]db.DeviceNotifyInfo) []problemMsg {
	byDev := map[uuid.UUID][]db.AlertNotification{}
	var order []uuid.UUID
	for _, n := range ns {
		if _, ok := byDev[n.DeviceID]; !ok {
			order = append(order, n.DeviceID)
		}
		byDev[n.DeviceID] = append(byDev[n.DeviceID], n)
	}
	// Restaurant-wide outages first.
	offlineAt := map[string][]uuid.UUID{}
	for _, id := range order {
		r := info[id].Restaurant
		if r == "" {
			continue
		}
		types := typesOf(byDev[id])
		if CauseOf(types).Key == "offline" {
			offlineAt[r] = append(offlineAt[r], id)
		}
	}
	var out []problemMsg
	folded := map[uuid.UUID]bool{}
	var rests []string
	for r, ids := range offlineAt {
		if len(ids) >= 2 {
			rests = append(rests, r)
		}
	}
	sort.Strings(rests)
	for _, r := range rests {
		ids := offlineAt[r]
		m := problemMsg{Severity: "warning"}
		var serials []string
		for _, id := range ids {
			folded[id] = true
			for _, n := range byDev[id] {
				m.IDs = append(m.IDs, n.AlertID)
				if db.SeverityRank(n.Severity) > db.SeverityRank(m.Severity) {
					m.Severity = n.Severity
				}
				if m.When == "" {
					m.When = formatWhen(n.EventAt, n.Timezone)
				}
			}
			serials = append(serials, byDev[id][0].Serial)
		}
		m.Title = fmt.Sprintf("%s %s — %s: %d devices offline", sevEmoji(m.Severity), strings.ToUpper(m.Severity), r, len(ids))
		m.Text = fmt.Sprintf("%d devices stopped checking in together. Most likely the restaurant's Wi-Fi or router, not the devices.\n\n%s",
			len(ids), strings.Join(serials, ", "))
		out = append(out, m)
	}
	for _, id := range order {
		if folded[id] {
			continue
		}
		group := byDev[id]
		m := problemMsg{Severity: "info"}
		var lines []string
		escalated := false
		for _, n := range group {
			m.IDs = append(m.IDs, n.AlertID)
			if db.SeverityRank(n.Severity) > db.SeverityRank(m.Severity) {
				m.Severity = n.Severity
			}
			if m.When == "" {
				m.When = formatWhen(n.EventAt, n.Timezone)
			}
			escalated = escalated || n.Escalated
			lines = append(lines, "• "+n.Summary)
		}
		c := CauseOf(typesOf(group))
		subject := group[0].Serial
		if r := info[id].Restaurant; r != "" {
			subject += " · " + r
		}
		m.Title = fmt.Sprintf("%s %s — %s: %s", sevEmoji(m.Severity), strings.ToUpper(m.Severity), subject, c.Title)
		text := c.Why
		if escalated {
			text = "Still happening now the restaurant is open. " + text
		}
		m.Text = strings.TrimSpace(text + "\n\n" + strings.Join(lines, "\n"))
		out = append(out, m)
	}
	return out
}

func typesOf(ns []db.AlertNotification) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Type)
	}
	return out
}

// send delivers one message in the channel's shape.
func send(ctx context.Context, c db.AlertChannel, severity, title, text, when string) error {
	if c.Kind == "teams" {
		return notify.SendTeams(ctx, c.URL, title, text, severity, when)
	}
	line := title + "\n" + text
	if when != "" {
		line += "\n🕒 " + when
	}
	return notify.SendWebhook(ctx, c.URL, line)
}

// DispatchResolved sends one "resolved" message for each problem a channel was told
// about that has now fully cleared, to the channels that want resolve messages.
func (dp *Dispatcher) DispatchResolved(ctx context.Context) {
	closed, err := dp.db.TakeClosedPagedProblems(ctx)
	if err != nil {
		log.Printf("[alert] closed problems: %v", err)
		return
	}
	if len(closed) == 0 {
		return
	}
	channels, err := dp.db.ListAlertChannels(ctx, true)
	if err != nil {
		log.Printf("[alert] list channels: %v", err)
		return
	}
	for _, p := range closed {
		subject := p.Serial
		if p.Restaurant != "" {
			subject += " · " + p.Restaurant
		}
		title := fmt.Sprintf("✅ RESOLVED — %s: %s", subject, CauseOf(p.Types).Title)
		text := "Open for " + roughDuration(p.ClosedAt.Sub(p.OpenedAt)) + "."
		if p.Reason != "" {
			text += " Resolved by hand: " + strings.ReplaceAll(p.Reason, "_", " ") + "."
		} else {
			text += " It cleared by itself."
		}
		for _, c := range channels {
			if c.URL == "" || c.Mode != "realtime" || !c.NotifyResolve {
				continue
			}
			if err := send(ctx, c, "info", title, text, ""); err != nil {
				log.Printf("[alert] channel %q resolve: %v", c.Name, err)
			}
		}
	}
}

func roughDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

// digestWindow is how long after opening the morning digest may still go out; a server
// that was down all morning does not send "overnight" at 3 PM.
const digestWindow = 2 * time.Hour

// MaybeSendMorningDigest sends the overnight digest once a day, at the fleet's opening
// time: what was raised while restaurants were closed (those alerts did not page), and
// whether every placed device is up for opening. Called every minute; cheap until due.
func (dp *Dispatcher) MaybeSendMorningDigest(ctx context.Context, connected []uuid.UUID) {
	w, err := dp.db.GetFleetServiceWindow(ctx)
	if err != nil {
		return
	}
	loc := time.UTC
	if w.TZ != "" {
		if l, err := time.LoadLocation(w.TZ); err == nil {
			loc = l
		}
	}
	now := time.Now().In(loc)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	open := day.Add(time.Duration(w.OpenMin) * time.Minute)
	if now.Before(open) || now.After(open.Add(digestWindow)) {
		return
	}
	// The night ran from last close to this open.
	nightMin := (w.OpenMin - w.CloseMin + 1440) % 1440
	if nightMin == 0 {
		nightMin = 8 * 60
	}
	since := open.Add(-time.Duration(nightMin) * time.Minute)
	night, err := dp.db.AlertsFiredSince(ctx, since)
	if err != nil {
		log.Printf("[digest] overnight alerts: %v", err)
		return
	}
	offline, err := dp.db.OfflineAtOpen(ctx, connected)
	if err != nil {
		log.Printf("[digest] offline at open: %v", err)
		return
	}
	title, text, lines := morningDigest(day, night, offline)
	if ok, err := dp.db.ClaimDigest(ctx, day, lines); err != nil || !ok {
		return
	}
	if lines == 0 {
		log.Printf("[digest] quiet night, nothing sent")
		return
	}
	channels, err := dp.db.ListAlertChannels(ctx, true)
	if err != nil {
		dp.db.ReleaseDigest(ctx, day)
		return
	}
	sent := 0
	for _, c := range channels {
		if c.URL == "" {
			continue
		}
		if err := send(ctx, c, "info", title, text, ""); err != nil {
			log.Printf("[digest] channel %q: %v", c.Name, err)
			continue
		}
		sent++
	}
	if sent == 0 && len(channels) > 0 {
		dp.db.ReleaseDigest(ctx, day) // retry next minute
		return
	}
	log.Printf("[digest] sent morning digest (%d lines) to %d channel(s)", lines, sent)
}

// morningDigestMax is how many problems the digest lists before "+N more".
const morningDigestMax = 12

// morningDigest writes the digest: one line per device problem raised overnight, with
// whether it is still open, then the state of the fleet at opening. lines is 0 on a
// quiet night with everything up, when nothing is sent.
func morningDigest(day time.Time, night []db.NightAlert, offlineAt map[string]int) (title, text string, lines int) {
	type devProb struct {
		serial, restaurant string
		types              []string
		open               bool
	}
	byDev := map[uuid.UUID]*devProb{}
	var order []uuid.UUID
	for _, n := range night {
		p, ok := byDev[n.DeviceID]
		if !ok {
			p = &devProb{serial: n.Serial, restaurant: n.Restaurant}
			byDev[n.DeviceID] = p
			order = append(order, n.DeviceID)
		}
		p.types = append(p.types, n.Type)
		p.open = p.open || n.Open
	}
	var b strings.Builder
	for i, id := range order {
		if i == morningDigestMax {
			fmt.Fprintf(&b, "• and %d more on the Alerts page\n", len(order)-morningDigestMax)
			break
		}
		p := byDev[id]
		state := "cleared"
		if p.open {
			state = "still open"
		}
		where := ""
		if p.restaurant != "" {
			where = " (" + p.restaurant + ")"
		}
		fmt.Fprintf(&b, "• %s%s: %s — %s\n", p.serial, where, CauseOf(p.types).Title, state)
		lines++
	}
	if len(offlineAt) == 0 {
		b.WriteString("\nAt opening: every placed device is reporting.")
	} else {
		var rs []string
		total := 0
		for r, n := range offlineAt {
			rs = append(rs, fmt.Sprintf("%s %d", r, n))
			total += n
		}
		sort.Strings(rs)
		fmt.Fprintf(&b, "\nAt opening: %d placed device(s) not reporting — %s.", total, strings.Join(rs, ", "))
		lines++
	}
	title = "🌅 Overnight, " + day.Format("Mon 2 Jan")
	if len(order) == 0 {
		title += " — quiet night"
	} else {
		title += fmt.Sprintf(" — %d device problem(s)", len(order))
	}
	return title, strings.TrimSpace(b.String()), lines
}
