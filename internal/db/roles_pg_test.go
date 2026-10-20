package db

import (
	"context"
	"os"
	"testing"
)

// TestRoleLadderMigrationAgainstPostgres replays what the 20 Oct role ladder meets on a
// database that predates it — the old role constraint, super op and access admin
// accounts with open sessions — and runs the migrations twice, as two restarts would.
func TestRoleLadderMigrationAgainstPostgres(t *testing.T) {
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
	_, err = d.pool.Exec(ctx, `
		DELETE FROM users WHERE username LIKE 'ladder-%';
		ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
		ALTER TABLE users ADD CONSTRAINT users_role_check CHECK (role IN ('viewer','operator','tester','user_manager','super_op','admin','owner'));
		INSERT INTO users (username, password_hash, role) VALUES ('ladder-so', 'x', 'super_op'), ('ladder-um', 'x', 'user_manager'), ('ladder-v', 'x', 'viewer');
		INSERT INTO sessions (id, user_id, username, role, created_at, last_seen, expires_at)
		  SELECT 'ladder-' || username, id, username, role, NOW(), NOW(), NOW() + interval '1 day' FROM users WHERE username LIKE 'ladder-%'`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := d.RunMigrations(ctx); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	want := map[string]string{"ladder-so": "fleet_manager", "ladder-um": "fleet_manager", "ladder-v": "viewer"}
	for u, role := range want {
		var got, sess string
		if err := d.pool.QueryRow(ctx, `SELECT u.role, s.role FROM users u JOIN sessions s ON s.user_id = u.id WHERE u.username = $1`, u).Scan(&got, &sess); err != nil {
			t.Fatal(err)
		}
		if got != role || sess != role {
			t.Errorf("%s: user %s, session %s, want %s", u, got, sess, role)
		}
	}
	for _, r := range []string{"support", "fleet_admin", "staff"} {
		if _, err := d.pool.Exec(ctx, `UPDATE users SET role = $1 WHERE username = 'ladder-v'`, r); err != nil {
			t.Errorf("can't save role %s: %v", r, err)
		}
	}
	if _, err := d.pool.Exec(ctx, `UPDATE users SET role = 'super_op' WHERE username = 'ladder-v'`); err == nil {
		t.Error("the retired super_op role is still accepted")
	}
	d.pool.Exec(ctx, `DELETE FROM users WHERE username LIKE 'ladder-%'`)
}
