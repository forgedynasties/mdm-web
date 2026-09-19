package ws

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// An MDM-lite device has no socket: a check-in shows it present for display only.
func TestCheckinPresenceIsDisplayOnly(t *testing.T) {
	h := NewHub()
	id := uuid.New()
	if h.IsConnectedForDisplay(id) {
		t.Fatal("unknown device shown present")
	}
	h.MarkCheckinPresence(id, true)
	if !h.IsConnectedForDisplay(id) {
		t.Error("checked-in device not shown present")
	}
	if _, ok := h.ConnectedIDsForDisplay()[id]; !ok {
		t.Error("checked-in device missing from the display set")
	}
	if h.IsConnected(id) {
		t.Error("check-in presence leaked into IsConnected (would try to push commands)")
	}
}

// A background check-in (15-minute job) keeps the device present far longer than a
// foreground one, and a foreground check-in's window stays short.
func TestBackgroundCheckinPresenceLasts(t *testing.T) {
	h := NewHub()
	fg, bg := uuid.New(), uuid.New()
	h.MarkCheckinPresence(fg, true)
	h.MarkCheckinPresence(bg, false)
	h.presenceMu.Lock()
	fgUntil, bgUntil := h.checkinSeenAt[fg], h.checkinSeenAt[bg]
	h.presenceMu.Unlock()
	if d := time.Until(fgUntil); d > CheckinPresence || d < CheckinPresence-time.Second {
		t.Errorf("foreground window %v, want ~%v", d, CheckinPresence)
	}
	if d := time.Until(bgUntil); d < 30*time.Minute {
		t.Errorf("background window %v, want >= 30m", d)
	}
}
