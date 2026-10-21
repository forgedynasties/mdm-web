package dashboard

import (
	"path/filepath"
	"testing"

	"mdm/internal/config"
	"mdm/internal/db"
)

// TestGMSClientFolded: v2.0.x T7s (firmware-gms) are measured against the GMS build and
// served by auto-update under the Firmware MDM switch; a Kiosk 22 still has no build.
func TestGMSClientFolded(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg}
	d := &db.Device{SerialNumber: "AT070AA2600044", BuildID: "v2.0.997"}

	if got := h.foldedStanding(d, "firmware-gms", 35); got != "nobuild" {
		t.Errorf("nothing hosted: %q, want nobuild", got)
	}
	if err := cfg.SetAgentAPKSlot("firmware-gms", config.AgentAPKBuild{SHA: "d969", Package: "com.aioapp.mdm", Version: "1.14.0", VersionCode: 101}); err != nil {
		t.Fatal(err)
	}
	for code, want := range map[int64]string{35: "behind", 101: "current", 0: "unknown"} {
		if got := h.foldedStanding(d, "firmware-gms", code); got != want {
			t.Errorf("code %d: %q, want %q", code, got, want)
		}
	}
	if got := h.foldedStanding(d, "firmware-kiosk22", 35); got != "nobuild" {
		t.Errorf("kiosk22: %q, want nobuild", got)
	}

	found := false
	for _, s := range autoUpdateSlots() {
		found = found || s == "firmware-gms"
	}
	if !found {
		t.Error("auto-update does not serve firmware-gms")
	}
	if h.autoUpdatePolicy("firmware-gms").Active() {
		t.Error("gms active while Firmware MDM auto-update is off")
	}
	if err := cfg.SetClientAutoUpdate("firmware-qcom", config.ClientAutoUpdate{On: true, WindowStart: "02:00", WindowEnd: "05:00"}); err != nil {
		t.Fatal(err)
	}
	if p := h.autoUpdatePolicy("firmware-gms"); !p.Active() || p.WindowStart != "02:00" {
		t.Errorf("gms policy = %+v, want Firmware MDM's", p)
	}
}
