package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"mdm/internal/db"
)

// deviceSecurity is the device page's Security card: the posture flags the client reports
// (nil = never reported, shown as a dash rather than "Off") and the wireless adb switch.
type deviceSecurity struct {
	USBDebugging   string // "On", "Off" or "—" (never reported)
	DevOptions     string
	UnknownSources string
	AdbKnown       bool // the client reports wireless adb at all
	AdbOn          bool
	// Wireless adb detail, from the last adb_tcp command the device completed: the client
	// reports only that adb is listening, not on which port or until when.
	AdbPort    int
	AdbOffIn   string // "23 h", "40 min"; "" when it stays on or nothing is known
	AdbStaysOn bool
	IP         string
	CanAdbTCP  bool // the device's agent takes adb_tcp (firmware client)
}

// Reported is whether the client sent any posture at all; older clients send none.
func (s deviceSecurity) Reported() bool {
	return s.AdbKnown || s.USBDebugging != "—" || s.DevOptions != "—" || s.UnknownSources != "—"
}

func postureWord(b *bool) string {
	switch {
	case b == nil:
		return "—"
	case *b:
		return "On"
	default:
		return "Off"
	}
}

func (h *Handler) securityFor(ctx context.Context, d *db.Device) deviceSecurity {
	var p struct {
		AdbEnabled     *bool  `json:"adb_enabled"`
		AdbTCP         *bool  `json:"adb_tcp"`
		DevOptions     *bool  `json:"dev_options_enabled"`
		UnknownSources *bool  `json:"unknown_sources"`
		IP             string `json:"ip_address"`
	}
	if len(d.LatestExtra) > 0 {
		_ = json.Unmarshal(d.LatestExtra, &p)
	}
	s := deviceSecurity{USBDebugging: postureWord(p.AdbEnabled), DevOptions: postureWord(p.DevOptions),
		UnknownSources: postureWord(p.UnknownSources), AdbKnown: p.AdbTCP != nil,
		AdbOn: p.AdbTCP != nil && *p.AdbTCP, IP: p.IP, CanAdbTCP: d.Supports("adb_tcp")}
	if s.AdbOn {
		if g, ok, _ := h.db.LastAdbTCP(ctx, d.ID); ok && g.Port > 0 {
			s.AdbPort = g.Port
			if g.OffAt.IsZero() {
				s.AdbStaysOn = true
			} else if left := time.Until(g.OffAt); left > 0 {
				s.AdbOffIn = durShort(left)
			}
		}
	}
	return s
}

// durShort is "23 h" / "40 min" for the time left on wireless adb.
func durShort(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%d h", int(d.Hours()+0.5))
	}
	return fmt.Sprintf("%d min", int(d.Minutes()+0.5))
}
