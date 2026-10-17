package dashboard

import (
	"context"
	"encoding/json"
	"time"

	"mdm/internal/db"
)

// ── Lost mode (L1, picked 17 Oct) ─────────────────────────────────────────────
//
// A lost device's screen is locked to a message and a phone number until someone lifts
// it here; it keeps checking in, and every check-in raises the critical "Lost device
// seen" alert with the last place the server has for it. On the dashboard it is a ⋯ menu
// entry ("Mark as lost…"), a red band at the top of the device page while it lasts, and a
// red "Lost" row in the fleet (the Lock filter's "Lost" value). Operators and up: it is
// reversible and protects the device.
//
// The phone number is one AIO support number for every lost device (Settings), not a
// venue's own — venues have no phone field.

// lostModeView is the device page's lost state: what the device reports, and the command
// that put it there (or is about to).
type lostModeView struct {
	On       bool      // the device says it is in lost mode
	Since    time.Time // when it went into lost mode, by its own clock
	Message  string
	By       string // who marked it lost
	Asked    bool   // the newest lost_mode command asks for it on
	Pending  bool   // that command has not been answered yet
	AskedAt  time.Time
	Venue    string
	Phone    string // the default for the dialog
	DefaultM string // the default message for the dialog
}

// Waiting is "marked lost here, the device has not shown the screen yet".
func (v lostModeView) Waiting() bool { return v.Asked && v.Pending && !v.On }

// Lifting is "lifted here, the device has not answered yet".
func (v lostModeView) Lifting() bool { return !v.Asked && v.Pending && v.On }

func (h *Handler) lostModeFor(ctx context.Context, d *db.Device) lostModeView {
	var x struct {
		Lost struct {
			On      bool   `json:"on"`
			SinceMs int64  `json:"since_ms"`
			Message string `json:"message"`
		} `json:"lost_mode"`
	}
	_ = json.Unmarshal(d.LatestExtra, &x)
	v := lostModeView{On: x.Lost.On, Message: x.Lost.Message, Venue: d.RestaurantName}
	if x.Lost.SinceMs > 0 {
		v.Since = time.UnixMilli(x.Lost.SinceMs)
	}
	if c, err := h.db.LatestDeviceCommand(ctx, d.ID, "lost_mode"); err == nil && c != nil {
		var p struct {
			On bool `json:"on"`
		}
		_ = json.Unmarshal(c.Payload, &p)
		v.Asked, v.AskedAt, v.By = p.On, c.CreatedAt, c.CreatedBy
		switch c.Status {
		case "completed", "failed", "expired", "cancelled", "unsupported":
		default:
			v.Pending = true
		}
	}
	v.Phone = h.cfg.LostModeSupportPhone()
	v.DefaultM = "Please hand it to a member of staff, or call:"
	if d.RestaurantName != "" {
		v.DefaultM = "It belongs to " + d.RestaurantName + ". " + v.DefaultM
	}
	return v
}
