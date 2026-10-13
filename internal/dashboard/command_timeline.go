package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"mdm/internal/db"
)

// The command timeline: what happened to one command on one device, in order, with the
// waits named.
//
// The queue used to show a single status, and that status said "delivered" the moment the
// server wrote the command to a socket. A half-open socket swallows that frame, so the page
// reported success for a command the device never saw — which is why AT070AABU00077 took a
// day (9 Oct 2026) rather than a minute: the evidence was on screen, reading as fine.
//
// The gap between two events is where the answer lives, so the gaps are what this labels.

// timelineStep is one rendered line: an event, or the wait before it.
type timelineStep struct {
	Label string // "Sent over the WebSocket"
	At    string // "06:03:41"
	ISO   string // for the relative-time script
	Note  string // the detail worth reading, if any
	Gap   string // a wait long enough to name, e.g. "2h 14m with no answer"
	Bad   bool   // this step is where it went wrong
	Done  bool   // a terminal step that went well
}

// gapWorthNaming is how long a wait has to be before it is the story rather than noise.
// Under this, a timeline that labelled every gap would bury the one that matters.
const gapWorthNaming = 45 * time.Second

func eventLabel(kind string, detail json.RawMessage) (string, string) {
	var d struct {
		Transport string `json:"transport"`
		Lane      string `json:"lane"`
		Guarantee string `json:"guarantee"`
		By        string `json:"by"`
		Status    string `json:"status"`
		Percent   *int   `json:"percent"`
		ExpiresIn string `json:"expires_in"`
	}
	_ = json.Unmarshal(detail, &d)
	switch kind {
	case "created":
		note := ""
		if d.By != "" {
			note = "by " + d.By
		}
		if d.Lane != "" {
			if note != "" {
				note += " · "
			}
			note += "lane " + d.Lane
		}
		return "Queued", note
	case "leased":
		switch d.Transport {
		case "http":
			// The device asked for it, so the hand-over is confirmed by the response it
			// read — unlike a socket write, which proves nothing about receipt.
			return "Handed over when the device asked (HTTP)", "held for up to " + d.ExpiresIn
		case "checkin":
			return "Sent back in its check-in", "held for up to " + d.ExpiresIn
		case "ws":
			return "Pushed over the WebSocket", "unconfirmed until the device says otherwise"
		}
		return "Handed over", ""
	case "received":
		return "Received by the device", "it confirmed it has the command"
	case "progress":
		label := "Working"
		if d.Status != "" {
			label = "Working — " + d.Status
		}
		if d.Percent != nil {
			label = fmt.Sprintf("%s %d%%", label, *d.Percent)
		}
		return label, ""
	case "completed", "installed":
		return "Completed", ""
	case "failed":
		return "Failed on the device", ""
	case "cancelled":
		return "Cancelled", ""
	case "expired":
		return "Expired before it ran", ""
	case "received_ack":
		return "Receipt acknowledged", ""
	}
	return kind, ""
}

func isTerminal(kind string) bool {
	switch kind {
	case "completed", "installed", "failed", "cancelled", "expired":
		return true
	}
	return false
}

// buildTimeline turns one device's events into rendered steps, naming every wait long
// enough to matter and marking the one that went wrong.
func buildTimeline(events []db.CommandEventRow) []timelineStep {
	var steps []timelineStep
	var prev time.Time
	for i, e := range events {
		label, note := eventLabel(e.Kind, e.Detail)
		s := timelineStep{
			Label: label,
			At:    e.At.Local().Format("15:04:05"),
			ISO:   e.At.UTC().Format(time.RFC3339),
			Note:  note,
			Bad:   e.Kind == "failed" || e.Kind == "expired",
			Done:  e.Kind == "completed" || e.Kind == "installed",
		}
		if !prev.IsZero() {
			if d := e.At.Sub(prev); d >= gapWorthNaming {
				// A wait before a receipt is the interesting one: it is the time the
				// device was holding a command nobody could prove it had.
				what := "waiting"
				if e.Kind == "received" {
					what = "with no confirmation from the device"
				}
				s.Gap = roughDuration(d) + " " + what
				// Re-handing a command over means the previous attempt was lost, so the
				// wait before it is a failure, not latency.
				if e.Kind == "leased" && i > 0 {
					s.Gap = roughDuration(d) + " before it was handed over again"
					s.Bad = true
				}
			}
		}
		steps = append(steps, s)
		prev = e.At
	}
	// Still outstanding: the open-ended wait is the whole point, so say it rather than
	// ending the list on an event that implies something happened.
	if n := len(events); n > 0 && !isTerminal(events[n-1].Kind) {
		if d := time.Since(events[n-1].At); d >= gapWorthNaming {
			steps = append(steps, timelineStep{
				Label: "Still waiting",
				Gap:   roughDuration(d) + " and counting",
				Bad:   d > 10*time.Minute,
			})
		}
	}
	return steps
}

// commandTimelines returns one timeline per targeted device, keyed by serial. The
// device-less 'created' event starts every device's story, because that is what happened
// to that device too.
func (h *Handler) commandTimelines(ctx context.Context, commandID uuid.UUID, deliveries []db.CommandDelivery) map[string][]timelineStep {
	events, err := h.db.ListAllCommandEvents(ctx, commandID)
	if err != nil || len(events) == 0 {
		return nil
	}
	var shared []db.CommandEventRow
	perDevice := map[uuid.UUID][]db.CommandEventRow{}
	for _, e := range events {
		if e.DeviceID == nil {
			shared = append(shared, e)
			continue
		}
		perDevice[*e.DeviceID] = append(perDevice[*e.DeviceID], e)
	}
	out := map[string][]timelineStep{}
	for _, d := range deliveries {
		merged := append(append([]db.CommandEventRow{}, shared...), perDevice[d.DeviceID]...)
		if len(merged) == 0 {
			continue
		}
		out[d.SerialNumber] = buildTimeline(merged)
	}
	return out
}
