package db

import "testing"

func TestLostConfigKeys(t *testing.T) {
	// Nothing lost nearby, self-lock off: both keys present, so a partial frame can't mislead.
	k := LostConfigKeys(LostConfig{}, "+1 555")
	if _, ok := k["lost_listen"]; !ok || k["lost_listen"] != nil {
		t.Fatalf("lost_listen must be present and null: %v", k)
	}
	if k["self_lock"].(map[string]any)["enabled"] != false {
		t.Fatalf("self_lock off: %v", k)
	}
	// The Standard client gets lost_listen only.
	k = LostConfigKeys(LostConfig{DPC: true, SelfLockMinutes: 10, Beacons: []string{"0011223344556677"}}, "+1 555")
	if _, ok := k["self_lock"]; ok {
		t.Fatalf("no self_lock for the Standard client: %v", k)
	}
	if k["lost_listen"].(map[string]any)["every_s"] != LostListenEveryS {
		t.Fatalf("lost_listen: %v", k)
	}
	// Firmware at a self-lock venue: the lost screen's defaults ride along.
	k = LostConfigKeys(LostConfig{Venue: "Nugget Downtown", SelfLockMinutes: 15}, "+1 555")
	sl := k["self_lock"].(map[string]any)
	if sl["enabled"] != true || sl["minutes"] != 15 || sl["phone"] != "+1 555" || sl["message"] != "It belongs to Nugget Downtown. Please hand it to a member of staff, or call:" {
		t.Fatalf("self_lock: %v", sl)
	}
	// The venue and its icon version ride along, self-lock on or off (client 1.13.4's lost
	// screen draws them for a lost_mode command too).
	if sl["venue"] != "Nugget Downtown" || sl["venue_icon_v"] != int64(0) {
		t.Fatalf("self_lock venue: %v", sl)
	}
	k = LostConfigKeys(LostConfig{Venue: "Pho 88", VenueIconV: 1792410614}, "")
	off := k["self_lock"].(map[string]any)
	if off["enabled"] != false || off["venue"] != "Pho 88" || off["venue_icon_v"] != int64(1792410614) {
		t.Fatalf("self_lock off with a venue: %v", off)
	}
}

func TestSignalWord(t *testing.T) {
	for rssi, want := range map[int]string{-50: "very close", -70: "close", -85: "in range", -95: "faint"} {
		if got := SignalWord(rssi); got != want {
			t.Errorf("%d: %q want %q", rssi, got, want)
		}
	}
}
