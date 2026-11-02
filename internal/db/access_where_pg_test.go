package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A person's Where: set, replaced, read back, survives a migration re-run, cleared with
// their grants.
func TestAccessWhere(t *testing.T) {
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
	user := "where-" + time.Now().Format("150405.000")
	if w, err := d.AccessWhereFor(ctx, user); err != nil || w != nil {
		t.Fatalf("no Where yet: %+v %v", w, err)
	}
	g := uuid.New()
	if err := d.SetAccessWhere(ctx, AccessWhere{Username: user, Scope: "groups", Elsewhere: "view"}); err == nil {
		t.Error("groups with no group accepted")
	}
	if err := d.SetAccessWhere(ctx, AccessWhere{Username: user, Scope: "groups", GroupIDs: []uuid.UUID{g}, Elsewhere: "hidden", SetBy: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	w, err := d.AccessWhereFor(ctx, user)
	if err != nil || w == nil || w.Scope != "groups" || len(w.GroupIDs) != 1 || w.GroupIDs[0] != g || w.Elsewhere != "hidden" {
		t.Fatalf("read back %+v %v", w, err)
	}
	if err := d.SetAccessWhere(ctx, AccessWhere{Username: user, Scope: "lab", GroupIDs: []uuid.UUID{g}, Elsewhere: "x"}); err != nil {
		t.Fatal(err)
	}
	if w, _ := d.AccessWhereFor(ctx, user); w.Scope != "lab" || len(w.GroupIDs) != 0 || w.Elsewhere != "view" {
		t.Errorf("replaced: %+v", w)
	}
	if all, err := d.ListAccessWhere(ctx); err != nil || all[user].Scope != "lab" {
		t.Errorf("list: %v %v", all[user], err)
	}
	if err := d.DeleteUserAccessLevels(ctx, user); err != nil {
		t.Fatal(err)
	}
	if w, _ := d.AccessWhereFor(ctx, user); w != nil {
		t.Error("deleting a person's access leaves their Where")
	}
}
