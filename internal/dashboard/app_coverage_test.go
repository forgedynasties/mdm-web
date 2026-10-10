package dashboard

import (
	"strings"
	"testing"

	"mdm/internal/db"
)

// The rules that decide what counts as a finding, against the shape the live fleet
// actually has (9 Oct 2026): one channel on six versions with half of its installs
// reporting no version at all, a channel running a version the library does not hold, and
// a package installed on devices that the library has never heard of.
func TestCoverageFindsWhatTheFleetGetsWrong(t *testing.T) {
	installs := []db.PackageInstall{
		// uatv2: 108 devices, 6 known versions, 50 with no version reported
		{PackageName: "aio.app.nugget.uatv2", AppName: "Nugget-uatv2", Version: "", Devices: 50},
		{PackageName: "aio.app.nugget.uatv2", AppName: "Nugget-uatv2", Version: "117-uatv2", Devices: 19},
		{PackageName: "aio.app.nugget.uatv2", AppName: "Nugget-uatv2", Version: "46.0.0-uatv2", Devices: 13},
		{PackageName: "aio.app.nugget.uatv2", AppName: "Nugget-uatv2", Version: "47.0.0-uatv2", Devices: 12},
		// stagev2: every device on 19.0.0 while the library holds 18.0.0
		{PackageName: "aio.app.nugget.stagev2", AppName: "Nugget-stagev2", Version: "19.0.0-stagev2", Devices: 22},
		// prod: everyone on the library's newest — must produce NO finding
		{PackageName: "aio.app.nugget", AppName: "Nugget", Version: "17.0.0", Devices: 12},
		// seen on devices, absent from the library
		{PackageName: "com.aioapp.kiosk.s3switchstatus", AppName: "S3 Switch", Version: "15", Devices: 3},
	}
	apps := []db.App{
		{PackageName: "aio.app.nugget.uatv2", VersionName: "47.0.0-uatv2"},
		{PackageName: "aio.app.nugget.stagev2", VersionName: "18.0.0-stagev2"},
		{PackageName: "aio.app.nugget", VersionName: "17.0.0"},
	}
	v := buildCoverage(installs, apps, db.FleetAppTotals{Devices: 187, ReportingApps: 186})

	uat := v.Of("aio.app.nugget.uatv2")
	if uat.Devices != 94 || uat.Unknown != 50 || uat.Known != 44 {
		t.Fatalf("uatv2 totals wrong: %d devices, %d unknown, %d known", uat.Devices, uat.Unknown, uat.Known)
	}
	if uat.OnLatest != 12 {
		t.Fatalf("uatv2: want 12 on the library's newest, got %d", uat.OnLatest)
	}
	// Versions are ordered by how many devices run them, so the biggest problem reads first.
	if uat.Versions[0].Devices != 50 || uat.Versions[0].Version != "" {
		t.Fatalf("versions not ordered by device count: %+v", uat.Versions[0])
	}

	has := func(sub string) bool {
		for _, f := range v.Findings {
			if strings.Contains(f.Headline, sub) {
				return true
			}
		}
		return false
	}
	if !has("report no version") {
		t.Error("the 50 installs with no reported version must be a finding")
	}
	if !has("running 3 different versions") {
		t.Errorf("version drift missed; findings: %+v", v.Findings)
	}
	if !has("not in the library") {
		t.Error("a package the library has never heard of must be a finding")
	}
	// The library holds 18.0.0-stagev2 and no device runs it: that is not automatically a
	// fleet problem (the library may be the stale one), so it must be reported as needing a
	// human rather than as a fix to apply.
	if !has("No device runs the version the library holds") {
		t.Error("a library version present on no device must be a finding")
	}
	for _, f := range v.Findings {
		if f.Package == "aio.app.nugget" {
			t.Errorf("a package that matches the library exactly must produce no finding, got %q", f.Headline)
		}
	}
	// An unknown version is never counted as up to date.
	if uat.OnLatest+uat.Unknown > uat.Devices {
		t.Error("unknown installs are being double-counted as current")
	}
}
