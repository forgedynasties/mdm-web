package dashboard

import (
	"encoding/json"
	"testing"

	"mdm/internal/db"
)

func TestLockAppFor(t *testing.T) {
	all := func(string) bool { return true }
	t7 := db.Device{DeviceClass: "t7", LatestExtra: json.RawMessage(`{"host_app":{"variants":[{"package":"aio.app.nugget.uatv2","foreground":true}]}}`)}
	pkgs := []db.DevicePackage{
		{PackageName: "aio.app.nugget.qa", VersionName: "3"},
		{PackageName: "com.android.chrome"},
		{PackageName: "aio.app.nugget", VersionName: "1"},
		{PackageName: "aio.app.nugget.uatv2", VersionName: "2"},
	}

	l := lockAppFor(t7, "aio.app.nugget", pkgs, &db.DeviceConfig{}, all)
	if l == nil || l.Name != "Nugget" || l.On || l.Label != "Lock" || !l.Menu() {
		t.Fatalf("unlocked, three builds: %+v", l)
	}
	got := []string{l.Variants[0].Package, l.Variants[1].Package, l.Variants[2].Package}
	want := []string{"aio.app.nugget.uatv2", "aio.app.nugget", "aio.app.nugget.qa"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want on screen, then prod, then the rest: %v", got, want)
		}
	}

	one := lockAppFor(t7, "aio.app.nugget", pkgs[2:3], &db.DeviceConfig{KioskEnabled: true, KioskPackage: "aio.app.nugget"}, all)
	if one.Menu() || one.Label != "Locked" || one.Sub != "Nugget" {
		t.Fatalf("one build, locked: %+v", one)
	}

	multi := lockAppFor(t7, "aio.app.nugget", pkgs, &db.DeviceConfig{KioskEnabled: true, KioskPackage: "aio.app.nugget.qa"}, all)
	if multi.Sub != "Nugget QA" {
		t.Fatalf("locked to a lab build: sub = %q", multi.Sub)
	}

	foreign := lockAppFor(t7, "aio.app.nugget", pkgs[2:3], &db.DeviceConfig{KioskEnabled: true, KioskPackage: "com.android.chrome"}, all)
	if !foreign.Foreign || !foreign.Menu() || foreign.Sub != "other app" {
		t.Fatalf("locked to another app: %+v", foreign)
	}

	none := lockAppFor(t7, "aio.app.nugget", pkgs[1:2], &db.DeviceConfig{}, all)
	if len(none.Variants) != 0 || !none.Menu() {
		t.Fatalf("not installed: %+v", none)
	}

	refused := lockAppFor(t7, "aio.app.nugget", pkgs, &db.DeviceConfig{}, func(p string) bool { return p == "aio.app.nugget" })
	if len(refused.Variants) != 1 {
		t.Fatalf("allowlist not applied: %+v", refused.Variants)
	}

	if lockAppFor(db.Device{DeviceClass: "kds"}, "", pkgs, &db.DeviceConfig{}, all) != nil {
		t.Fatal("a role with no app keeps the picker")
	}
}

func TestLockPackageFor(t *testing.T) {
	base := "aio.app.nugget"
	d := db.Device{LatestExtra: json.RawMessage(`{"foreground_pkg":"aio.app.nugget.uatv2"}`)}
	inst := map[string]string{"aio.app.nugget": "1", "aio.app.nugget.uatv2": "2", "aio.app.nugget.qa": "3"}

	// The venue's variant wins once installed, even over the build it is locked to.
	if got := lockPackageFor(d, base, "aio.app.nugget.qa", inst, db.KioskState{Enabled: true, Package: "aio.app.nugget.uatv2"}); got != "aio.app.nugget.qa" {
		t.Errorf("venue variant: got %q", got)
	}
	if got := lockPackageFor(d, base, "aio.app.nugget.none", inst, db.KioskState{}); got != "aio.app.nugget.uatv2" {
		t.Errorf("venue variant not installed: got %q", got)
	}
	if got := lockPackageFor(d, base, "", inst, db.KioskState{}); got != "aio.app.nugget.uatv2" {
		t.Fatalf("on screen first: %q", got)
	}
	if got := lockPackageFor(db.Device{}, base, "", inst, db.KioskState{}); got != "aio.app.nugget" {
		t.Fatalf("then prod: %q", got)
	}
	if got := lockPackageFor(d, base, "", inst, db.KioskState{Enabled: true, Package: "aio.app.nugget.qa"}); got != "aio.app.nugget.qa" {
		t.Fatalf("keeps the build it is locked to: %q", got)
	}
	if got := lockPackageFor(d, base, "", inst, db.KioskState{Enabled: true, Package: "aio.app.nugget.gone"}); got != "aio.app.nugget.uatv2" {
		t.Fatalf("a locked build no longer installed is not kept: %q", got)
	}
	if got := lockPackageFor(d, "", "", inst, db.KioskState{}); got != "" {
		t.Fatalf("no role app: %q", got)
	}
	if got := lockPackageFor(d, "aio.app.kds", "", inst, db.KioskState{}); got != "" {
		t.Fatalf("role app not installed: %q", got)
	}
}
