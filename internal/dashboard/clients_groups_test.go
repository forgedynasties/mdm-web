package dashboard

import (
	"testing"
	"time"
)

// The devices table is grouped so the rows an operator can act on are not buried under
// the ones they cannot: a device that never reported a version takes a firmware OTA, not
// a button on this page, and two thirds of a typical client's roster is that.
func TestGroupClientRows(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	fresh, dormant := now.Add(-2*time.Hour), now.Add(-60*24*time.Hour)
	rows := []ClientDeviceRow{
		{Serial: "behind-fresh", State: "behind", LastSeen: fresh},
		{Serial: "behind-dormant", State: "behind", LastSeen: dormant},
		{Serial: "updating", State: "updating", LastSeen: fresh},
		{Serial: "unknown-fresh", State: "unknown", LastSeen: fresh},
		{Serial: "unknown-dormant", State: "unknown", LastSeen: dormant},
		{Serial: "current", State: "current", LastSeen: fresh},
	}

	groups := groupClientRows(rows, "1.2.0", now)
	if len(groups) != 3 {
		t.Fatalf("want 3 groups, got %d", len(groups))
	}
	if groups[0].Key != "behind" || groups[1].Key != "unknown" || groups[2].Key != "current" {
		t.Fatalf("groups out of action order: %s, %s, %s", groups[0].Key, groups[1].Key, groups[2].Key)
	}

	// A device mid-update belongs with "behind" — it is behind, it is just already
	// being dealt with, and moving it elsewhere hides that the push is working.
	if got := len(groups[0].Rows); got != 2 {
		t.Errorf("behind group: want 2 live rows (behind + updating), got %d", got)
	}
	if got := len(groups[0].Dormant); got != 1 || groups[0].Dormant[0].Serial != "behind-dormant" {
		t.Errorf("behind group: want the long-unseen device folded, got %v", groups[0].Dormant)
	}
	if !groups[0].Dormant[0].Dormant {
		t.Error("a folded row should be marked Dormant, so the template can dim it")
	}
	if got := len(groups[1].Rows); got != 1 || groups[1].Rows[0].Serial != "unknown-fresh" {
		t.Errorf("unknown group: want just the fresh device inline, got %v", groups[1].Rows)
	}
	if groups[0].Why != "can install 1.2.0 now" {
		t.Errorf("behind group should name the build it can install, got %q", groups[0].Why)
	}
}

// An empty group is dropped rather than rendered as a header over nothing, and a slot
// with nothing hosted still explains itself instead of saying "can install  now".
func TestGroupClientRowsEmptyAndUnhosted(t *testing.T) {
	now := time.Now()
	groups := groupClientRows([]ClientDeviceRow{
		{Serial: "a", State: "unknown", LastSeen: now},
	}, "", now)
	if len(groups) != 1 || groups[0].Key != "unknown" {
		t.Fatalf("want only the unknown group, got %+v", groups)
	}
	groups = groupClientRows([]ClientDeviceRow{
		{Serial: "b", State: "behind", LastSeen: now},
	}, "", now)
	if groups[0].Why != "nothing is hosted for this client yet" {
		t.Errorf("unhosted slot should say so, got %q", groups[0].Why)
	}
	if len(groupClientRows(nil, "1.0.0", now)) != 0 {
		t.Error("no rows should mean no groups")
	}
}
