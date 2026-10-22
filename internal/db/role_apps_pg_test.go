package db

import (
	"context"
	"os"
	"testing"
)

// Each role starts with its app, once: a role an admin cleared stays cleared across
// restarts (the seed runs with every migration), and a change shows without waiting
// out the cache.
func TestRoleAppSeedAgainstPostgres(t *testing.T) {
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
	if _, err := d.pool.Exec(ctx, `DROP TABLE IF EXISTS role_apps CASCADE`); err != nil {
		t.Fatal(err)
	}
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if got := d.RoleAppBase(ctx, "t7"); got != "aio.app.nugget" {
		t.Fatalf("t7 seeded with %q", got)
	}
	if got := d.RoleAppBase(ctx, "other"); got != "" {
		t.Fatalf("other seeded with %q", got)
	}
	if err := d.SetRoleApp(ctx, "t7", "", "test"); err != nil {
		t.Fatal(err)
	}
	if got := d.RoleAppBase(ctx, "t7"); got != "" {
		t.Fatalf("cleared t7 still reads %q", got)
	}
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	d.roleApps.drop()
	if got := d.RoleAppBase(ctx, "t7"); got != "" {
		t.Fatalf("a restart re-seeded the cleared t7: %q", got)
	}
	if err := d.SetRoleApp(ctx, "t7", "aio.app.nugget", "test"); err != nil {
		t.Fatal(err)
	}
	if got := d.RoleAppBase(ctx, "t7"); got != "aio.app.nugget" {
		t.Fatalf("t7 after setting: %q", got)
	}
}
