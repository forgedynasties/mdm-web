package dashboard

import (
	"testing"
	"time"

	"mdm/internal/db"
)

func TestNewestBuildPrefersHigherVersion(t *testing.T) {
	now := time.Now()
	apps := []db.App{
		{PackageName: "p", VersionName: "18.0.0", ApkURL: "new", CreatedAt: now.Add(-time.Hour)},
		{PackageName: "p", VersionName: "17.0.0", ApkURL: "old-reupload", CreatedAt: now},
	}
	if got := newestBuilds(apps, map[string]bool{"p": true})["p"]; got.ApkURL != "new" {
		t.Fatalf("re-uploaded old build won: %s", got.ApkURL)
	}
	apps[1].VersionName = "18.0.0"
	if got := newestBuilds(apps, map[string]bool{"p": true})["p"]; got.ApkURL != "old-reupload" {
		t.Fatalf("same version should go by upload time: %s", got.ApkURL)
	}
}
