package dashboard

import (
	"context"
	"strings"

	"mdm/internal/db"
)

// Person page PE1 (18 Nov 2026): "Can <person> reach a device?" asks the same evaluator the
// rest of the dashboard uses, once per level, for one serial typed on the page (GET ?serial=).

type reachRow struct {
	Level   string // View / Operate / Manage
	Does    string // what that level means for this device
	Allowed bool
	Reason  string
}

type personReach struct {
	Serial, Venue string
	Err           string
	Rows          []reachRow
}

func (h *Handler) personReachOf(ctx context.Context, u *db.User, serial string) *personReach {
	serial = strings.TrimSpace(serial)
	if serial == "" {
		return nil
	}
	dev, err := h.db.GetDevice(ctx, serial)
	if err != nil || dev == nil {
		return &personReach{Serial: serial, Err: "No device with serial " + serial}
	}
	invalidatePolicy(u.Username)
	acc := h.accessFor(ctx, u.Role, u.Username)
	out := &personReach{Serial: dev.SerialNumber, Venue: dev.RestaurantName}
	for _, l := range []struct{ level, does, action string }{
		{"View", "looks at it, takes screenshots", "view"},
		{"Operate", "reboots, locks, installs apps", "reboot"},
		{"Manage", "remote control, Wi-Fi", "remote"},
	} {
		d := acc.decide(l.action, &dev.ID)
		out.Rows = append(out.Rows, reachRow{Level: l.level, Does: l.does, Allowed: d.Allowed, Reason: d.Reason})
	}
	return out
}
