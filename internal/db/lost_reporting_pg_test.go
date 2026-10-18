package db

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestLostReportingAgainstPostgres runs the lost-reporting plumbing end to end: a lost
// session with a beacon from the lost_mode command, the venue's other devices told to
// listen for it, what they hear, a self-lock, and trail scans. Needs MDM_TEST_DATABASE_URL.
func TestLostReportingAgainstPostgres(t *testing.T) {
	url := os.Getenv("MDM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MDM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	d, err := New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Format("150405.000")
	rest, err := d.CreateRestaurant(ctx, Restaurant{Name: "Lost venue " + stamp})
	if err != nil {
		t.Fatal(err)
	}
	b := 70
	mk := func(sfx string) uuid.UUID {
		id, _, _, _, _, err := d.UpsertCheckin(ctx, "LR"+sfx+stamp, "v2.1.099", &b, json.RawMessage(`{"lost_mode_cmd":true}`), false, "t7")
		if err != nil {
			t.Fatal(err)
		}
		if err := d.AssignDeviceToRestaurant(ctx, "LR"+sfx+stamp, &rest.ID); err != nil {
			t.Fatal(err)
		}
		return id
	}
	lost, near, far := mk("A"), mk("B"), uuid.Nil
	_ = far

	// Nothing lost: nobody listens, self-lock off.
	c, err := d.LostConfigFor(ctx, near)
	if err != nil || len(c.Beacons) != 0 || c.Lost {
		t.Fatalf("before: %+v %v", c, err)
	}
	if k := LostConfigKeys(c, "+1 555"); k["lost_listen"] != nil || k["self_lock"].(map[string]any)["enabled"] != false {
		t.Fatalf("keys before: %v", k)
	}

	// Marked lost: a beacon in the payload, a session, the neighbour listens for it.
	gen := d.LostConfigGen()
	cmd, err := d.CreateCommandBy(ctx, "lost_mode", "", json.RawMessage(`{"on":true,"message":"x","beacon":"ffffffffffffffff"}`), "devices", []uuid.UUID{lost}, "Sara")
	if err != nil {
		t.Fatal(err)
	}
	var p struct{ Beacon string }
	_ = json.Unmarshal(cmd.Payload, &p)
	if len(p.Beacon) != 16 || p.Beacon == "ffffffffffffffff" {
		t.Fatalf("beacon: %q (a caller's own must be replaced)", p.Beacon)
	}
	if d.LostConfigGen() == gen {
		t.Fatal("generation did not move")
	}
	s, _ := d.ActiveLostSession(ctx, lost)
	if s == nil || s.Beacon != p.Beacon || s.StartedBy != "Sara" {
		t.Fatalf("session: %+v", s)
	}
	c, _ = d.LostConfigFor(ctx, near)
	if len(c.Beacons) != 1 || c.Beacons[0] != p.Beacon {
		t.Fatalf("neighbour listens for: %v", c.Beacons)
	}
	if own, _ := d.LostConfigFor(ctx, lost); len(own.Beacons) != 0 || !own.Lost {
		t.Fatalf("the lost device does not listen for itself: %+v", own)
	}
	if ids, _ := d.VenueDeviceIDs(ctx, []uuid.UUID{lost}); len(ids) != 1 || ids[0] != near {
		t.Fatalf("venue devices: %v", ids)
	}

	// The neighbour hears it twice in one minute and once with an unknown beacon.
	now := time.Now().UTC()
	ext := func(r1, r2 int) json.RawMessage {
		x, _ := json.Marshal(map[string]any{"ble_sightings": []map[string]any{
			{"beacon": p.Beacon, "rssi": r1, "at_ms": now.UnixMilli()},
			{"beacon": p.Beacon, "rssi": r2, "at_ms": now.Add(time.Second).UnixMilli()},
			{"beacon": "0011223344556677", "rssi": -40, "at_ms": now.UnixMilli()},
		}})
		return x
	}
	d.IngestBLESightings(ctx, near, ext(-80, -62), now)
	got, err := d.LostSightings(ctx, lost, now.Add(-time.Hour))
	if err != nil || len(got) != 1 || got[0].RSSI != -62 || got[0].Count != 1 || got[0].HeardBy != near {
		t.Fatalf("sightings: %+v %v", got, err)
	}

	// Trail scans stored unplaced; pending for a lost device.
	sc := json.RawMessage(`[{"bssid":"aa:bb:cc:dd:ee:ff","rssi":-60}]`)
	if n, err := d.InsertTrailScans(ctx, lost, []TrailScan{{At: now.Add(-10 * time.Minute), Scan: sc}, {At: now.Add(-10 * time.Minute), Scan: sc}}); err != nil || n != 1 {
		t.Fatalf("scans: %d %v", n, err)
	}
	pend, _ := d.PendingTrailScans(ctx, 100)
	var pid int64
	for _, x := range pend {
		if x.DeviceID == lost {
			pid = x.ID
		}
	}
	if pid == 0 {
		t.Fatal("lost device's scan is not pending")
	}
	if err := d.ResolveTrailScan(ctx, pid, 37.77, -122.41, 40, true); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertTrailFix(ctx, lost, now, 37.78, -122.42, 30, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	_ = d.InsertTrailFix(ctx, lost, now.Add(time.Minute), 37.79, -122.43, 30, 5*time.Minute) // within 5 min: skipped
	tr, _ := d.DeviceTrail(ctx, lost, now.Add(-time.Hour))
	if len(tr) != 2 || tr[0].Source != "scan" || tr[1].Source != "live" {
		t.Fatalf("trail: %+v", tr)
	}

	// Lifted: session ends, neighbour stops listening.
	if _, err := d.CreateCommandBy(ctx, "lost_mode", "", json.RawMessage(`{"on":false}`), "devices", []uuid.UUID{lost}, "Sara"); err != nil {
		t.Fatal(err)
	}
	if s, _ := d.ActiveLostSession(ctx, lost); s != nil {
		t.Fatalf("still lost: %+v", s)
	}
	if c, _ = d.LostConfigFor(ctx, near); len(c.Beacons) != 0 {
		t.Fatalf("still listening: %v", c.Beacons)
	}

	// Self-lock: a device-started session, ended only by the device lifting it.
	if err := d.SetRestaurantSelfLock(ctx, rest.ID, 10); err != nil {
		t.Fatal(err)
	}
	c, _ = d.LostConfigFor(ctx, near)
	if k := LostConfigKeys(c, "+1 555"); k["self_lock"].(map[string]any)["minutes"] != 10 {
		t.Fatalf("self_lock: %v", k)
	}
	d.RecordSelfLock(ctx, near, now.Add(-5*time.Minute))
	d.RecordSelfLock(ctx, near, now.Add(-5*time.Minute))
	if s, _ := d.ActiveLostSession(ctx, near); s == nil || s.StartedBy != "self" {
		t.Fatalf("self session: %+v", s)
	}
	d.EndSelfLock(ctx, near)
	if s, _ := d.ActiveLostSession(ctx, near); s != nil {
		t.Fatalf("self session not ended: %+v", s)
	}
}
