package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestJobLeasesAgainstPostgres: one holder at a time, the holder can renew, a released
// or expired lease can be taken.
func TestJobLeasesAgainstPostgres(t *testing.T) {
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
	name := "job-" + time.Now().Format("150405.000")
	try := func(holder string, hold time.Duration) bool {
		t.Helper()
		ok, err := d.TryJobLease(ctx, name, holder, hold)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !try("a", time.Minute) {
		t.Fatal("a free lease should be granted")
	}
	if try("b", time.Minute) {
		t.Fatal("b must not take a's live lease")
	}
	if !try("a", time.Minute) {
		t.Fatal("a may renew its own lease")
	}
	if err := d.ReleaseJobLease(ctx, name, "a", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if !try("b", time.Minute) {
		t.Fatal("b should take a lease a released")
	}
	if err := d.ReleaseJobLease(ctx, name, "a", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if try("a", time.Minute) {
		t.Fatal("a releasing a lease it no longer holds must not free b's")
	}
}
