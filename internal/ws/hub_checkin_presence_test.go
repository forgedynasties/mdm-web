package ws

import (
	"testing"

	"github.com/google/uuid"
)

// An MDM-lite device has no socket: a check-in shows it present for display only.
func TestCheckinPresenceIsDisplayOnly(t *testing.T) {
	h := NewHub()
	id := uuid.New()
	if h.IsConnectedForDisplay(id) {
		t.Fatal("unknown device shown present")
	}
	h.MarkCheckinPresence(id)
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
