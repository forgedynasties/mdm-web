package dashboard

import (
	"fmt"
	"strings"

	"mdm/internal/db"
)

// humanizeAudit turns an audit row into the predicate of a sentence whose subject is
// the person: "disabled wireless charging", so the activity line reads
//
//	Ali  disabled wireless charging
//
// The log stores action, target and detail as three loosely-related strings whose
// meaning differs per action — detail is "enabled" for a toggle, a deployment id for a
// rollout, a free-text note length for a notes edit. Rendering those three columns raw,
// as the activity page did, asks the reader to know that mapping. This encodes it once.
//
// Anything unrecognised falls back to the action with its dots turned into spaces,
// which reads acceptably for the many actions that are already verb-like
// ("release.publish" → "release publish") and never hides an entry just because it has
// no bespoke phrasing.
func humanizeAudit(e db.AuditEntry) string {
	d := strings.TrimSpace(e.Detail)
	switch e.Action {

	// ── device toggles ────────────────────────────────────────────────────────
	case "device.wlc_charging":
		return onOff(d, "wireless charging")
	case "device.kiosk":
		// detail is "enabled (single_app)" or "disabled".
		if strings.HasPrefix(d, "enabled") {
			if mode := inParens(d); mode != "" {
				return "enabled kiosk mode (" + mode + ")"
			}
			return "enabled kiosk mode"
		}
		return "disabled kiosk mode"

	// ── device identity and placement ─────────────────────────────────────────
	case "device.nickname":
		if d == "" {
			return "cleared the device name"
		}
		return fmt.Sprintf("named the device %q", d)
	case "device.notes":
		return "edited the notes"
	case "device.hide":
		return "hid the device"
	case "device.unhide":
		return "un-hid the device"
	case "device.restaurant":
		if d == "" {
			return "removed the device from its restaurant"
		}
		return "assigned the device to a restaurant"
	case "device.class":
		return "changed the device class" + trailing(d)
	case "device.move_server":
		return "moved the device to another server" + trailing(d)
	case "device.offline_code_rotate":
		return "rotated the offline exit code"

	// ── rollouts ──────────────────────────────────────────────────────────────
	case "deployment.retry":
		return "retried the update" + deployRef(d)
	case "deployment.reboot":
		return "rebooted the device" + deployRef(d)
	case "deployment.cancel_ota":
		return "cancelled the update" + deployRef(d)
	case "deployment.remove_device":
		return "removed the device from a rollout" + deployRef(d)

	// ── commands ──────────────────────────────────────────────────────────────
	case "command.send":
		if e.Target == "" {
			return "sent a command"
		}
		return "sent a " + strings.ReplaceAll(e.Target, "_", " ") + " command"
	case "command.resend":
		return "re-sent a command" + trailing(e.Target)
	case "command.cancel", "device.queue_remove":
		return "cancelled a queued command"
	case "device.queue_clear":
		return "cleared the command queue"

	default:
		return strings.ReplaceAll(e.Action, ".", " ")
	}
}

// onOff renders a toggle whose detail is the word "enabled" or "disabled".
func onOff(detail, what string) string {
	switch detail {
	case "enabled":
		return "enabled " + what
	case "disabled":
		return "disabled " + what
	default:
		// An older row, written before the toggles recorded a word. Say what changed
		// without asserting a direction we do not actually know.
		return "changed " + what
	}
}

// deployRef appends the rollout an action belonged to, when the detail carries one.
func deployRef(detail string) string {
	if detail == "" {
		return ""
	}
	return " (rollout " + detail + ")"
}

func trailing(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

// inParens returns the text inside the first (...) of s, or "".
func inParens(s string) string {
	i := strings.IndexByte(s, '(')
	j := strings.LastIndexByte(s, ')')
	if i < 0 || j <= i+1 {
		return ""
	}
	return s[i+1 : j]
}
