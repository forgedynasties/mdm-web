package dashboard

import (
	"testing"

	"mdm/internal/db"
)

// The update line (U2): steps follow the phase, and each state offers its own action.
func TestOtaLineOf(t *testing.T) {
	u := &db.Update{ID: 431, ReleaseID: 77, DeviceStatus: "downloading", OtaPackage: &db.OTAPackage{Type: "full", TargetBuildID: "v2.1.041", PayloadSize: 1288490189}}
	l := otaLineOf(u, "Downloading", 55)
	if l.Title != "Updating to v2.1.041" || l.Sub != "full · 1.2 GB" || !l.CanCancel || l.Href != "/releases/77/deployments/431" {
		t.Fatalf("line = %+v", l)
	}
	if l.Steps[0].State != "cur" || l.Steps[0].Pct != 55 || l.Steps[1].State != "todo" {
		t.Errorf("steps = %+v", l.Steps)
	}
	// The client reports installing before verifying (27 Oct 2026: the line had them the other way).
	if l := otaLineOf(&db.Update{DeviceStatus: "installing", OtaPackage: u.OtaPackage}, "Verifying", 39); l.Steps[1].Name != "Install" || l.Steps[1].State != "done" || l.Steps[2].Name != "Verify" || l.Steps[2].State != "cur" || l.Steps[2].Pct != 39 {
		t.Errorf("verifying = %+v", l.Steps)
	}
	l = otaLineOf(&db.Update{DeviceStatus: "awaiting_reboot", OtaPackage: u.OtaPackage}, "Awaiting reboot", 100)
	if !l.AwaitsReboot || l.CanCancel || l.Steps[2].State != "done" || l.Steps[3].State != "cur" || l.Steps[3].Pct != 0 {
		t.Errorf("awaiting = %+v", l)
	}
	if l := otaLineOf(&db.Update{DeviceStatus: "failed", OtaPackage: u.OtaPackage}, "Failed", 100); !l.Failed || l.Steps != nil || l.CanCancel {
		t.Errorf("failed = %+v", l)
	}
}
