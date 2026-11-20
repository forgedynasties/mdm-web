package api

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// The watcher's client gate (19 Nov 2026; user: "it waits for it to come online then makes
// sure mdm client is updated then move to prod").
//
// A pending move goes out only when the device is online (and, for firmware at a venue,
// the venue is closed: moveWait) AND its MDM client is the build hosted for its slot. A
// client that is behind gets ONE app_update from here (the command RunClientAutoUpdate
// sends, with the same in-flight checks); the move waits and the note on the move says why.
// The venue's update window or pause does not apply: the move is the point.
//
// Decision table (state = the dashboard's standing of the device against its slot's build):
//
//	current                                  send the move
//	behind, client can self-update           queue app_update (not if one is in flight or this
//	                                         watcher sent one < 20 min ago), keep waiting
//	behind, update given up (3 failed in 24h,
//	  or a signing-key refusal) or no
//	  self-update                            send if the client can move itself, else wait
//	nobuild (no hosted build) / unknown      send if the client can move itself, else wait
//
// "Can move itself": firmware always (setprop + reboot, no client change needed); Standard
// from 0.13.4 (move_server). Without the hook (tests, stage) the gate is open.

const (
	moveUpdateEvery   = 20 * time.Minute // one client update per device per this long
	moveUpdateGiveUps = 3                // failed updates in 24 h before the watcher stops asking
	// moveMinStandard is the first Standard client that takes move_server.
	moveMinStandard = "0.13.4"
)

// SetClientStanding wires the dashboard's view of the hosted client builds (main.go).
func (h *Handler) SetClientStanding(f func(ctx context.Context, d *db.Device) db.ClientStanding) {
	h.clientStanding = f
}

// versionAtLeast compares dotted versions ("0.15.1" >= "0.13.4"); a missing or unparsable
// version is below everything.
func versionAtLeast(have, min string) bool {
	parse := func(s string) ([]int, bool) {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, false
		}
		var out []int
		for _, p := range strings.Split(s, ".") {
			end := 0
			for end < len(p) && p[end] >= '0' && p[end] <= '9' {
				end++
			}
			n, err := strconv.Atoi(p[:end])
			if err != nil {
				return nil, false
			}
			out = append(out, n)
		}
		return out, true
	}
	a, ok := parse(have)
	if !ok {
		return false
	}
	b, _ := parse(min)
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x > y
		}
	}
	return true
}

// canMoveItself: whether the device's client takes the move as it is.
func canMoveItself(kind, have string) bool {
	if kind == "firmware" {
		return true
	}
	return versionAtLeast(have, moveMinStandard)
}

// clientWait says why the move can't go because of the device's client, or "". It queues
// the client update when one is due.
func (h *Handler) clientWait(ctx context.Context, m db.DeviceMove) string {
	if h.clientStanding == nil {
		return ""
	}
	dev, err := h.db.GetDevice(ctx, m.Serial)
	if err != nil || dev == nil {
		return "waiting: could not read the device"
	}
	st := h.clientStanding(ctx, dev)
	have := st.Have
	if have == "" {
		have = m.ClientVersion
	}
	switch st.State {
	case "current":
		return ""
	case "behind":
		if st.CanUpdate && len(st.Payload) > 0 {
			why, ok := h.queueClientUpdate(ctx, m, dev, st)
			if ok {
				return why
			}
			// Can't (or won't any more) update it from here: fall to the minimum.
			if why != "" {
				return why
			}
		}
	}
	if canMoveItself(m.Kind, have) {
		return ""
	}
	if have == "" {
		have = "no version reported"
	}
	return fmt.Sprintf("client too old and cannot update (%s, needs %s)", have, moveMinStandard)
}

// queueClientUpdate sends the hosted client to a device behind it, once per moveUpdateEvery.
// It returns the note to wait with and true while an update is under way or just sent;
// "", false when the watcher gave up (the caller then applies the minimum rule), or a note
// and false for a reason to hold that is not about the update.
func (h *Handler) queueClientUpdate(ctx context.Context, m db.DeviceMove, dev *db.Device, st db.ClientStanding) (string, bool) {
	wait := fmt.Sprintf("waiting for MDM client %s → %s", st.Have, st.Want)
	if st.Have == "" {
		wait = "waiting for MDM client → " + st.Want
	}
	ids := []uuid.UUID{dev.ID}
	last, failed, err := h.db.MoveUpdateHistory(ctx, dev.ID)
	if err != nil {
		return "waiting: could not read the client update history", false
	}
	if failed >= moveUpdateGiveUps {
		return "", false
	}
	if busy, err := h.db.InFlightAgentUpdates(ctx, ids); err == nil && len(busy) > 0 {
		return wait + " (installing)", true
	}
	// An OTA or an APK install on its way: the agent restarting would lose it.
	if ota, err := h.db.InFlightOTAs(ctx, ids); err == nil && ota[dev.ID] {
		return wait + " (a system update is running)", true
	}
	if inst, err := h.db.InFlightInstalls(ctx, ids); err == nil && inst[dev.ID] {
		return wait + " (an app install is running)", true
	}
	if !last.IsZero() && time.Since(last) < moveUpdateEvery {
		return wait, true
	}
	cmd, err := h.db.CreateCommandBy(ctx, "app_update", st.URL, st.Payload, "devices", ids, db.MoveUpdateBy)
	if err != nil {
		log.Printf("[move] %s: client update: %v", m.Serial, err)
		return "waiting: could not queue the client update, trying again", true
	}
	h.pushCommand(ctx, cmd, "devices", ids)
	log.Printf("[move] %s: MDM client %s → %s queued before the move", m.Serial, st.Have, st.Want)
	return wait, true
}

// moveMaxCheckin is the largest check-in body (the device's last report, in bytes) a device may
// have for a move to go. Prod's load balancer firewall refuses request bodies over 8 KB with a
// 403, so a device whose check-in carries stack traces or long lists moves, is refused there for
// ten minutes, and falls back to this server (20 Nov 2026: DP02256HJ0147, a 53 KB report, was on
// prod for 12 minutes and came back; 19 of the other pending devices carry crash events too).
// Normal reports are 2-4 KB. MOVE_MAX_CHECKIN_BYTES overrides it, 0 turns the gate off (set it
// once the firewall rule is raised).
func moveMaxCheckin() int {
	if v := strings.TrimSpace(os.Getenv("MOVE_MAX_CHECKIN_BYTES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 7500
}

// moveCheckinTooBig reports whether a report of n bytes stays out of the move (max 0 = no limit).
func moveCheckinTooBig(n, max int) bool { return max > 0 && n > max }

// sizeWait says why the move can't go because the device's report is too big for prod, or "".
func (h *Handler) sizeWait(ctx context.Context, m db.DeviceMove) string {
	max := moveMaxCheckin()
	if max == 0 {
		return ""
	}
	dev, err := h.db.GetDevice(ctx, m.Serial)
	if err != nil || dev == nil {
		return ""
	}
	if n := len(dev.LatestExtra); moveCheckinTooBig(n, max) {
		return fmt.Sprintf("waiting: its check-in is %d KB and prod's firewall refuses over 8 KB (a crash report or a long list in it); it moves once the report shrinks", (n+1023)/1024)
	}
	return ""
}
