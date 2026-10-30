package db

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// QA builds (30 Oct 2026): follows, the pending-install read the strip uses, and the key
// clash bookkeeping.
func TestAppFollowsAndKeyClash(t *testing.T) {
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
	sfx := time.Now().Format("150405000")
	id, _, _, _, _, err := d.UpsertCheckin(ctx, "QA"+sfx, "b1", nil, json.RawMessage(`{"agent_type":"dpc"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	pkg := "aio.app.kiosk.qa" + sfx
	if err := d.FollowApp(ctx, id, pkg, "tester"); err != nil {
		t.Fatal(err)
	}
	if err := d.FollowApp(ctx, id, pkg, "tester"); err != nil {
		t.Fatal("follow is idempotent:", err)
	}
	all, _ := d.AllFollows(ctx)
	if len(all[id]) != 1 || all[id][0] != pkg {
		t.Fatalf("follows = %v", all[id])
	}
	if err := d.UpsertDevicePackages(ctx, id, []DevicePackage{{PackageName: pkg, VersionName: "999-qa"}}); err != nil {
		t.Fatal(err)
	}
	if v, _ := d.DevicePackageVersions(ctx, id); v[pkg] != "999-qa" {
		t.Fatalf("versions = %v", v)
	}
	if r, _ := d.PackagesReported(ctx, []uuid.UUID{id, uuid.New()}); !r[id] || len(r) != 1 {
		t.Fatalf("reported = %v", r)
	}

	app, err := d.CreateApp(ctx, "KIOSK-QA", "https://x/"+sfx+".apk", pkg)
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := d.CreateCommandBy(ctx, "install_apk", app.ApkURL, json.RawMessage(`{}`), "devices", []uuid.UUID{id}, "app-follow")
	if err != nil {
		t.Fatal(err)
	}
	p := 42
	if err := d.SetCommandProgress(ctx, cmd.ID, id, "downloading", &p); err != nil {
		t.Fatal(err)
	}
	if pend, _ := d.PendingInstalls(ctx, id); pend[pkg].Status != "downloading" || pend[pkg].Percent != 42 {
		t.Fatalf("pending = %+v", pend)
	}
	_ = d.SaveCommandResult(ctx, cmd.ID, id, "error: INSTALL_FAILED_UPDATE_INCOMPATIBLE: signatures do not match")
	if err := d.AckCommand(ctx, cmd.ID, id, "failed"); err != nil {
		t.Fatal(err)
	}
	var mine *KeyClash
	ks, err := d.NewKeyClashes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ks {
		if ks[i].CommandID == cmd.ID {
			mine = &ks[i]
		}
	}
	if mine == nil || mine.Package != pkg || mine.AtVenue {
		t.Fatalf("clash = %+v", mine)
	}
	un, _ := d.CreateCommandBy(ctx, "uninstall", "", json.RawMessage(`{"package":"`+pkg+`"}`), "devices", []uuid.UUID{id}, "key-clash")
	if err := d.StartKeyClashRetry(ctx, *mine, "uninstall", &un.ID); err != nil {
		t.Fatal(err)
	}
	ks, _ = d.NewKeyClashes(ctx)
	for _, k := range ks {
		if k.CommandID == cmd.ID {
			t.Fatal("a recorded clash is not listed again")
		}
	}
	find := func() *KeyClashRetry {
		ws, err := d.WaitingKeyClashRetries(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i := range ws {
			if ws[i].CommandID == cmd.ID {
				return &ws[i]
			}
		}
		return nil
	}
	if w := find(); w == nil || w.UninstallStatus != "" {
		t.Fatalf("waiting = %+v", w)
	}
	_ = d.AckCommand(ctx, un.ID, id, "completed")
	if w := find(); w == nil || w.UninstallStatus != "completed" {
		t.Fatalf("after uninstall = %+v", w)
	}
	again, _ := d.CreateCommandBy(ctx, "install_apk", app.ApkURL, json.RawMessage(`{}`), "devices", []uuid.UUID{id}, "key-clash")
	if err := d.SetKeyClashStage(ctx, cmd.ID, id, "install", &again.ID); err != nil {
		t.Fatal(err)
	}
	if find() != nil {
		t.Fatal("an installed retry is no longer waiting")
	}
	// The reinstall failing the same way is never retried again.
	_ = d.SaveCommandResult(ctx, again.ID, id, "INSTALL_FAILED_UPDATE_INCOMPATIBLE")
	_ = d.AckCommand(ctx, again.ID, id, "failed")
	ks, _ = d.NewKeyClashes(ctx)
	for _, k := range ks {
		if k.CommandID == again.ID {
			t.Fatal("the retry's own failure must not start another retry")
		}
	}
	if err := d.UnfollowApp(ctx, id, pkg); err != nil {
		t.Fatal(err)
	}
	if fs, _ := d.DeviceFollows(ctx, id); len(fs) != 0 {
		t.Fatalf("after unfollow = %v", fs)
	}
}
