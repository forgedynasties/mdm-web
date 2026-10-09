package dashboard

import (
	"net/http"
	"time"

	"mdm/internal/db"
)

// commandChannelView is the device page's answer to the question the Online badge cannot
// answer: will anything we send to this device actually run?
//
// The badge is live WebSocket presence, so a device whose socket died shows Offline while
// its telemetry stays current — "Offline" sitting next to "Last seen: just now", with
// nothing saying that commands to it will not run. That contradiction is what made
// AT070AABU00077 take a day to diagnose instead of a minute (9 Oct 2026: 15h of check-ins,
// a shell command stuck at 'delivered', fixed only by a reboot).
type commandChannelView struct {
	// Channel is "http" (no socket, but the client polls — reachable, late) or "none" (no
	// socket and a client with no pull path — nothing sent to it will run). A device with a
	// socket produces no view at all.
	Channel string
	Since   time.Time
	For     string // "15h 12m", for the sentence
	// Reachable is Channel == "http": the difference between a note and a site visit.
	Reachable bool
	// RoundTrip is when a command last completed for this device — the only evidence it is
	// controllable, as opposed to merely reporting. Zero means never.
	RoundTrip time.Time
}

// commandChannelFor returns the banner's data, or nil when there is nothing to say: the
// device holds a socket, or it is simply offline (no check-ins either — that is ordinary
// Offline, which the badge already says and the offline alert already covers).
func (h *Handler) commandChannelFor(r *http.Request, dev *db.Device) *commandChannelView {
	if dev == nil || h.hub.IsConnected(dev.ID) {
		return nil
	}
	// Not heard from recently: this is a device that is off or off-network, not one that is
	// reporting-but-unreachable. Saying "no command channel" about a powered-down tablet
	// would be true and useless.
	if time.Since(dev.LastSeenAt) > 15*time.Minute {
		return nil
	}
	since, roundTrip, channel, err := h.db.CommandChannelState(r.Context(), dev.ID)
	if err != nil || since.IsZero() {
		return nil
	}
	// Below the alert's own grace period this is a reconnect or a blip, not a condition.
	gone := time.Since(since)
	if gone < 20*time.Minute {
		return nil
	}
	if channel == "" || channel == "ws" {
		// The stored channel disagrees with the hub (a server restart dropped every socket
		// before any check-in could update it). Trust what the client reported it can do.
		channel = "none"
	}
	return &commandChannelView{
		Channel:   channel,
		Since:     since,
		For:       roughDuration(gone),
		Reachable: channel == "http",
		RoundTrip: roundTrip,
	}
}

// roughDuration renders a spell length the way the alert text does.
func roughDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		return itoa(int(d.Hours())) + "h " + itoa(int(d.Minutes())%60) + "m"
	}
	return itoa(int(d.Hours()/24)) + " days"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
