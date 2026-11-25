package dashboard

import (
	"net/http"
	"strings"
	"time"
	"unicode"

	"context"

	"mdm/internal/db"
)

// noteLogItem is one line of the Notes sheet: a person's entry, or (Auto) what the MDM did.
// personInitials: "Ali Hassan" → "AH", "ali.hassan@aioapp.com" → "AL"; "?" for an entry
// with no author ("Earlier note").
func personInitials(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == "Earlier note" {
		return "?"
	}
	if i := strings.Index(name, "@"); i > 0 {
		name = name[:i]
	}
	var out []rune
	for _, w := range strings.Fields(name) {
		r := []rune(w)
		out = append(out, unicode.ToUpper(r[0]))
		if len(out) == 2 {
			break
		}
	}
	if len(out) == 1 {
		if r := []rune(strings.Fields(name)[0]); len(r) > 1 {
			out = append(out, unicode.ToUpper(r[1]))
		}
	}
	return string(out)
}

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
		// The author's name as people know it ("First Last"), not the username (25 Nov 2026,
		// user: "just display name in the notes entry"). An entry copied from the old single
		// note has no author.
		names := map[string]string{}
		for _, n := range notes {
			who := n.Author
			if who == "" {
				who = "Earlier note"
			} else if nm, ok := names[who]; ok {
				who = nm
			} else {
				nm = h.displayName(r, who)
				names[n.Author] = nm
				who = nm
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
