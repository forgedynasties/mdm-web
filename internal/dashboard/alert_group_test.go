package dashboard

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func ha(typ, restaurant, serial string, occ int) humanAlert {
	return humanAlert{
		ID: uuid.New(), Type: typ, Restaurant: restaurant, Serial: serial,
		Occurrences: occ, Headline: "Charged slowly overnight",
	}
}

// One rule firing across a site folds into a single row; the count the operator
// acts on is the device count, not the row count.
func TestGroupAlertsFoldsSameTypeAndSite(t *testing.T) {
	in := []humanAlert{
		ha("slow_charge_night", "Flights Vegas", "AT070AABU00673", 1),
		ha("slow_charge_night", "Flights Vegas", "AT070AABU00537", 2),
		ha("slow_charge_night", "Flights Vegas", "AT070AABU00802", 1),
	}
	got := groupAlerts(in)
	if len(got) != 1 {
		t.Fatalf("want 1 group, got %d", len(got))
	}
	if got[0].DeviceCount != 3 {
		t.Errorf("DeviceCount = %d, want 3", got[0].DeviceCount)
	}
	if got[0].Occurrences != 4 {
		t.Errorf("Occurrences = %d, want 4", got[0].Occurrences)
	}
	if got[0].Lead.Serial != "AT070AABU00673" {
		t.Errorf("lead = %q, want the first (newest) member", got[0].Lead.Serial)
	}
	// Every folded serial stays searchable, or the client-side filter would lose it.
	for _, want := range []string{"AT070AABU00537", "AT070AABU00802"} {
		if !strings.Contains(got[0].Search, want) {
			t.Errorf("Search %q missing %q", got[0].Search, want)
		}
	}
}

// The same rule at different sites is different problems, and stays separate rows.
func TestGroupAlertsKeepsSitesApart(t *testing.T) {
	in := []humanAlert{
		ha("slow_charge_night", "Flights Vegas", "A1", 1),
		ha("slow_charge_night", "Flights Reno", "A2", 1),
		ha("overheating", "Flights Vegas", "A3", 1),
	}
	if got := groupAlerts(in); len(got) != 3 {
		t.Fatalf("want 3 groups, got %d", len(got))
	}
}

// Crash alerts carry a per-device trace, so folding them would hide the payload.
func TestGroupAlertsNeverFoldsCrashes(t *testing.T) {
	in := []humanAlert{
		ha("device_crash", "Flights Vegas", "A1", 1),
		ha("device_crash", "Flights Vegas", "A2", 1),
	}
	got := groupAlerts(in)
	if len(got) != 2 {
		t.Fatalf("want 2 ungrouped crash rows, got %d", len(got))
	}
	for _, g := range got {
		if g.DeviceCount != 1 {
			t.Errorf("crash row folded: DeviceCount = %d", g.DeviceCount)
		}
	}
}

// Order is the page's existing severity/fired_at order: a group sits where its
// newest member sat.
func TestGroupAlertsPreservesOrder(t *testing.T) {
	in := []humanAlert{
		ha("overheating", "Vegas", "A1", 1),
		ha("slow_charge_night", "Vegas", "A2", 1),
		ha("overheating", "Vegas", "A3", 1),
	}
	got := groupAlerts(in)
	if len(got) != 2 {
		t.Fatalf("want 2 groups, got %d", len(got))
	}
	if got[0].Lead.Type != "overheating" || got[1].Lead.Type != "slow_charge_night" {
		t.Errorf("order changed: %q then %q", got[0].Lead.Type, got[1].Lead.Type)
	}
	if got[0].DeviceCount != 2 {
		t.Errorf("overheating DeviceCount = %d, want 2", got[0].DeviceCount)
	}
}
