package db

import (
	"context"
	"os"
	"testing"
)

func TestDisplayPrefsAgainstPostgres(t *testing.T) {
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
	if _, err := d.pool.Exec(ctx, `INSERT INTO users (username, password_hash, role) VALUES ('dp-1', 'x', 'viewer')`); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM users WHERE username = 'dp-1'`)
	p, err := d.UserDisplayPrefs(ctx, "dp-1")
	if err != nil || p.PageSize != nil || p.Use24 != nil {
		t.Fatalf("fresh user = %+v, %v", p, err)
	}
	n, s, on := 75, "serial", true
	if err := d.SetUserDisplayPrefs(ctx, "dp-1", DisplayPrefs{PageSize: &n, Sort: &s, Use24: &on}); err != nil {
		t.Fatal(err)
	}
	p, _ = d.UserDisplayPrefs(ctx, "dp-1")
	if p.PageSize == nil || *p.PageSize != 75 || p.Sort == nil || *p.Sort != "serial" || p.Density != nil || p.Use24 == nil || !*p.Use24 {
		t.Fatalf("round trip = %+v", p)
	}
}
