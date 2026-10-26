package dashboard

import (
	"testing"

	"github.com/google/uuid"

	"mdm/internal/db"
)

func TestPickAutoPullsNewestOnly(t *testing.T) {
	d1, d2, d3 := uuid.New(), uuid.New(), uuid.New()
	cands := []db.PullCandidate{ // freshest device first
		{DeviceID: d1, Serial: "A", Package: "aio.app.kiosk", Version: "52.0.0"},
		{DeviceID: d2, Serial: "B", Package: "aio.app.kiosk", Version: "54.0.0"},
		{DeviceID: d3, Serial: "C", Package: "aio.app.kds.qa", Version: "1.1.3-qa"},
		{DeviceID: d1, Serial: "A", Package: "aio.app.nugget", Version: "17.0.0"},
	}
	apps := []db.App{
		{PackageName: "aio.app.nugget", VersionName: "18.0.0"}, // library is ahead: no pull
	}
	got := pickAutoPulls(cands, apps, map[string]bool{}, 10)
	if len(got) != 2 {
		t.Fatalf("got %d pulls %+v, want kiosk and kds.qa", len(got), got)
	}
	if got[0].Package != "aio.app.kiosk" || got[0].Version != "54.0.0" || got[0].Serial != "B" {
		t.Errorf("kiosk pull = %+v, want 54.0.0 from B (the newest only)", got[0])
	}
	// Already asked for lately, or already in the library: left alone.
	if got := pickAutoPulls(cands, apps, map[string]bool{"aio.app.kiosk@54.0.0": true, "aio.app.kds.qa@1.1.3-qa": true}, 10); len(got) != 0 {
		t.Errorf("recent pulls must not repeat: %+v", got)
	}
	if got := pickAutoPulls(cands, append(apps, db.App{PackageName: "aio.app.kiosk", VersionName: "54.0.0"}), map[string]bool{}, 10); len(got) != 1 || got[0].Package != "aio.app.kds.qa" {
		t.Errorf("a version in the library must not be pulled again: %+v", got)
	}
	if got := pickAutoPulls(cands, apps, map[string]bool{}, 1); len(got) != 1 {
		t.Errorf("the per-run cap is not respected: %+v", got)
	}
}
