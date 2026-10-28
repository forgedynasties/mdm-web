package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// An invite token can be stored and consumed (the purpose check refused it until 28 Oct 2026).
func TestInviteTokenStores(t *testing.T) {
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
	// Twice: the migration must be safe to run again on every start.
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	email := "invite-" + time.Now().Format("150405.000") + "@example.com"
	u, err := d.CreateUser(ctx, email, "x", "viewer", &email, true)
	if err != nil {
		t.Fatal(err)
	}
	tok := "tok-" + email
	if err := d.CreateUserToken(ctx, tok, u.ID, "invite", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("invite token refused: %v", err)
	}
	if id, err := d.ConsumeUserToken(ctx, tok, "invite"); err != nil || id != u.ID {
		t.Fatalf("consume: %v %v", id, err)
	}
}
