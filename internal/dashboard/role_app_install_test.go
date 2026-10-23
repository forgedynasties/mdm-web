package dashboard

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

func TestRoleAppInstallPick(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	bases := map[string]string{"t7": "aio.app.nugget", "kiosk": "aio.app.kiosk"}
	lib := newestProdBuilds([]db.App{
		{PackageName: "aio.app.nugget", VersionName: "17.0.0", ApkURL: "u17", CreatedAt: now.Add(-48 * time.Hour)},
		{PackageName: "aio.app.nugget", VersionName: "16.0.0", ApkURL: "u16", CreatedAt: now.Add(-96 * time.Hour)},
		{PackageName: "aio.app.nugget.uatv2", VersionName: "48.0.0-uatv2", ApkURL: "uat", CreatedAt: now},
		{PackageName: "aio.app.kiosk", VersionName: "54.0.0", CreatedAt: now}, // no APK
	}, bases)
	if lib["aio.app.nugget"] == nil || lib["aio.app.nugget"].VersionName != "17.0.0" || lib["aio.app.kiosk"] != nil {
		t.Fatalf("library picks = %+v", lib)
	}
	dev := func(sn, product string, seen time.Duration) db.Device {
		return db.Device{ID: uuid.New(), SerialNumber: sn, Product: product, AgentKind: "firmware", LastSeenAt: now.Add(-seen)}
	}
	devs := []db.Device{
		dev("MISSING", "t7", time.Minute),    // no venue, lacks prod: gets it
		dev("HASPROD", "t7", time.Minute),    // has aio.app.nugget
		dev("OFFLINE", "t7", time.Hour),      // lacks it, not online
		dev("BUSY", "t7", time.Minute),       // install on its way
		dev("KIOSK", "kiosk22", time.Minute), // its product has no prod APK
		dev("UNKNOWN", "t7", time.Minute),    // never reported its apps
	}
	missing := map[string]map[uuid.UUID]bool{"aio.app.nugget": {devs[0].ID: true, devs[2].ID: true, devs[3].ID: true}}
	skip := map[uuid.UUID]bool{devs[3].ID: true}
	got := pickRoleAppInstalls(devs, bases, lib, missing, skip, 3*time.Minute, now)
	if len(got) != 1 || got[0].Device.SerialNumber != "MISSING" || got[0].App.ApkURL != "u17" {
		t.Fatalf("picked = %+v", got)
	}
}
