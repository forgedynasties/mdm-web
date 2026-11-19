package dashboard

import (
	"net/http"
	"time"

	"context"

	"mdm/internal/db"
)

// noteLogItem is one line of the Notes sheet: a person's entry, or (Auto) what the MDM did.
type noteLogItem struct {
	At   time.Time
	Who  string
	Kind string // note | issue | fix | hardware | auto
	Text string
	Auto bool
}

// noteLogFor merges a device's manual entries with what was done to it (the audit log, in the
// words the Activity tab already uses), newest first. Admin-only actors stay hidden from
// everyone below admin, as in the Activity card.
func (h *Handler) noteLogFor(ctx context.Context, r *http.Request, device *db.Device) []noteLogItem {
	var out []noteLogItem
	if notes, err := h.db.ListDeviceNotes(ctx, device.ID, 200); err == nil {
		for _, n := range notes {
			who := n.Author
			if who == "" {
				who = "earlier note"
			}
			out = append(out, noteLogItem{At: n.At, Who: who, Kind: n.Kind, Text: n.Body})
		}
	}
	if entries, err := h.db.ListAuditForDevice(ctx, device.ID, 200); err == nil {
		for _, a := range h.deviceActivityItems(ctx, h.role(r), entries, 100) {
			if a.Action == "device.notes" { // the note itself is the entry
				continue
			}
			out = append(out, noteLogItem{At: a.At, Who: a.Actor, Kind: "auto", Text: a.Did, Auto: true})
		}
	}
	// newest first
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].At.After(out[j-1].At); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
