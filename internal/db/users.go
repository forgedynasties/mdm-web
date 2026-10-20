package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ── Users ─────────────────────────────────────────────────────────────────────

type User struct {
	ID              uuid.UUID  `json:"id"`
	Username        string     `json:"username"`
	Role            string     `json:"role"` // see roleDefs in dashboard/roles.go
	PasswordHash    string     `json:"-"`
	Email           *string    `json:"email,omitempty"`
	EmailVerifiedAt *time.Time `json:"email_verified_at,omitempty"`
	FirstName       string     `json:"first_name"`
	LastName        string     `json:"last_name"`
	CreatedAt       time.Time  `json:"created_at"`
	// AvatarVer is the unix time the profile picture was set (0 = none); it versions
	// the /users/{id}/avatar.png URL so browsers refetch after a change.
	AvatarVer int64 `json:"avatar_ver"`
}

// HasAvatar reports whether a profile picture is on file.
func (u User) HasAvatar() bool { return u.AvatarVer > 0 }

// Initial is the single (upper-cased) letter shown in a user bubble without a picture.
func (u User) Initial() string {
	n := strings.TrimSpace(u.FirstName)
	if n == "" {
		n = u.Username
	}
	for _, r := range n {
		return strings.ToUpper(string(r))
	}
	return "?"
}

// DisplayName returns "First Last" when a name is on file, else the username
// (usually an email) so the UI always has something readable to show.
func (u User) DisplayName() string {
	n := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if n == "" {
		return u.Username
	}
	return n
}

// CreateUser inserts a new DB user. email may be nil (legacy/admin-created accounts
// with no email on file); emailVerified marks it pre-verified (admin vouches for it,
// or the account has no email so verification doesn't apply) vs. NULL (self-signup,
// pending the emailed verify link).
func (d *DB) CreateUser(ctx context.Context, username, passwordHash, role string, email *string, emailVerified bool) (*User, error) {
	return d.CreateUserNamed(ctx, username, passwordHash, role, email, emailVerified, "", "")
}

// CreateUserNamed is CreateUser plus a first/last name captured at sign-up.
func (d *DB) CreateUserNamed(ctx context.Context, username, passwordHash, role string, email *string, emailVerified bool, firstName, lastName string) (*User, error) {
	var u User
	err := d.pool.QueryRow(ctx, `
		INSERT INTO users (username, password_hash, role, email, email_verified_at, first_name, last_name)
		VALUES ($1, $2, $3, $4, CASE WHEN $5 THEN NOW() ELSE NULL END, $6, $7)
		RETURNING id, username, role, password_hash, email, email_verified_at, first_name, last_name, created_at, COALESCE(EXTRACT(EPOCH FROM avatar_updated_at), 0)::bigint
	`, username, passwordHash, role, email, emailVerified, firstName, lastName).Scan(
		&u.ID, &u.Username, &u.Role, &u.PasswordHash, &u.Email, &u.EmailVerifiedAt, &u.FirstName, &u.LastName, &u.CreatedAt, &u.AvatarVer)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (d *DB) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	var u User
	err := d.pool.QueryRow(ctx, `
		SELECT id, username, role, password_hash, email, email_verified_at, first_name, last_name, created_at, COALESCE(EXTRACT(EPOCH FROM avatar_updated_at), 0)::bigint
		FROM users WHERE username = $1
	`, username).Scan(&u.ID, &u.Username, &u.Role, &u.PasswordHash, &u.Email, &u.EmailVerifiedAt, &u.FirstName, &u.LastName, &u.CreatedAt, &u.AvatarVer)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUserByEmail looks up a user by their (case-insensitive) email — used by the
// forgot-password flow. Accounts with no email on file never match.
func (d *DB) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	var u User
	err := d.pool.QueryRow(ctx, `
		SELECT id, username, role, password_hash, email, email_verified_at, first_name, last_name, created_at, COALESCE(EXTRACT(EPOCH FROM avatar_updated_at), 0)::bigint
		FROM users WHERE lower(email) = lower($1)
	`, email).Scan(&u.ID, &u.Username, &u.Role, &u.PasswordHash, &u.Email, &u.EmailVerifiedAt, &u.FirstName, &u.LastName, &u.CreatedAt, &u.AvatarVer)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUser looks up a user by id (e.g. to resolve "who ran this command" for the
// activity log / actions history).
func (d *DB) GetUser(ctx context.Context, id uuid.UUID) (*User, error) {
	var u User
	err := d.pool.QueryRow(ctx, `
		SELECT id, username, role, password_hash, email, email_verified_at, first_name, last_name, created_at, COALESCE(EXTRACT(EPOCH FROM avatar_updated_at), 0)::bigint
		FROM users WHERE id = $1
	`, id).Scan(&u.ID, &u.Username, &u.Role, &u.PasswordHash, &u.Email, &u.EmailVerifiedAt, &u.FirstName, &u.LastName, &u.CreatedAt, &u.AvatarVer)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// SetUserEmailVerified marks a user's email as verified now (consumed by the
// verify-email link).
func (d *DB) SetUserEmailVerified(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `UPDATE users SET email_verified_at = NOW() WHERE id = $1`, id)
	return err
}

// SetUserAvatar stores a (already resized) PNG as the user's profile picture.
func (d *DB) SetUserAvatar(ctx context.Context, id uuid.UUID, png []byte) error {
	_, err := d.pool.Exec(ctx, `UPDATE users SET avatar = $2, avatar_updated_at = now() WHERE id = $1`, id, png)
	return err
}

// ClearUserAvatar removes the profile picture.
func (d *DB) ClearUserAvatar(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `UPDATE users SET avatar = NULL, avatar_updated_at = NULL WHERE id = $1`, id)
	return err
}

// GetUserAvatar returns the stored PNG and its version (unix seconds); nil when none.
func (d *DB) GetUserAvatar(ctx context.Context, id uuid.UUID) ([]byte, int64, error) {
	var png []byte
	var ver int64
	err := d.pool.QueryRow(ctx, `SELECT avatar, COALESCE(EXTRACT(EPOCH FROM avatar_updated_at), 0)::bigint FROM users WHERE id = $1`, id).Scan(&png, &ver)
	return png, ver, err
}

func (d *DB) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, username, role, email, email_verified_at, first_name, last_name, created_at, COALESCE(EXTRACT(EPOCH FROM avatar_updated_at), 0)::bigint FROM users ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.Email, &u.EmailVerifiedAt, &u.FirstName, &u.LastName, &u.CreatedAt, &u.AvatarVer); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, nil
}

func (d *DB) DeleteUser(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	return err
}

// UpdateUserRole changes a user's role. The caller validates the role value.
func (d *DB) UpdateUserRole(ctx context.Context, id uuid.UUID, role string) error {
	_, err := d.pool.Exec(ctx, `UPDATE users SET role = $2 WHERE id = $1`, id, role)
	return err
}

// UpdateUserName sets a user's first/last name — set at sign-up, editable
// afterward by the user or an admin (e.g. for accounts created before names
// existed, or to correct a typo).
// actorColumns lists every (table, column) that snapshots a username as the
// person who did something. Kept in one place so MergeActor and ListOrphanActors
// can never drift apart.
var actorColumns = [][2]string{
	{"audit_log", "actor"},
	{"commands", "created_by"},
	{"test_cases", "created_by"},
	{"command_recipes", "created_by"},
	{"scheduled_recipes", "created_by"},
	{"device_queries", "created_by"},
	{"release_test_results", "tested_by"},
	{"qfil_packages", "added_by"},
	{"dismissed_commands", "dismissed_by"},
	{"release_problems", "reported_by"},
	{"releases", "signed_off_by"},
	{"releases", "testing_done_by"},
	{"releases", "merged_by"},
}

// OrphanActor is a username that appears on past actions but no longer matches
// any account — typically a user deleted after their replacement (e.g. a
// Microsoft sign-in) was created. Merging re-points those rows to the new user.
type OrphanActor struct {
	Username string
	Rows     int64
	LastAt   *time.Time
}

// ListOrphanActors returns usernames present in attribution columns but absent
// from users, with how many rows carry them and when they were last active.
// Non-account actors ("unknown", "", "Scheduled recipe: …") are excluded.
func (d *DB) ListOrphanActors(ctx context.Context) ([]OrphanActor, error) {
	var parts []string
	for _, tc := range actorColumns {
		ts := "NULL::timestamptz"
		if tc[0] == "audit_log" || tc[0] == "commands" {
			ts = "created_at"
		}
		parts = append(parts, fmt.Sprintf("SELECT %s AS u, %s AS t FROM %s", tc[1], ts, tc[0]))
	}
	rows, err := d.pool.Query(ctx, `
		SELECT u, COUNT(*), MAX(t) FROM (`+strings.Join(parts, " UNION ALL ")+`) a
		WHERE u <> '' AND u <> 'unknown' AND u NOT LIKE '%: %'
		  AND NOT EXISTS (SELECT 1 FROM users x WHERE x.username = a.u)
		GROUP BY u ORDER BY MAX(t) DESC NULLS LAST, u`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrphanActor
	for rows.Next() {
		var o OrphanActor
		if err := rows.Scan(&o.Username, &o.Rows, &o.LastAt); err != nil {
			return nil, err
		}
		if _, sys := SystemActorLabel(o.Username); sys {
			continue // the server's own work, never an account to merge
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// MergeActor rewrites every attribution row carrying username `from` to `to`
// (an existing account's username), plus the saved dashboard layout if the
// target has none. Returns rows rewritten. Runs in one transaction.
func (d *DB) MergeActor(ctx context.Context, from, to string) (int64, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var total int64
	for _, tc := range actorColumns {
		tag, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET %s = $2 WHERE %s = $1`, tc[0], tc[1], tc[1]), from, to)
		if err != nil {
			return 0, fmt.Errorf("%s.%s: %w", tc[0], tc[1], err)
		}
		total += tag.RowsAffected()
	}
	if _, err := tx.Exec(ctx, `
		UPDATE user_layouts SET username = $2 WHERE username = $1
		  AND NOT EXISTS (SELECT 1 FROM user_layouts x WHERE x.username = $2)`, from, to); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_layouts WHERE username = $1`, from); err != nil {
		return 0, err
	}
	// The account now owns history older than itself: date it from its first action.
	if _, err := tx.Exec(ctx, `
		UPDATE users u SET created_at = LEAST(u.created_at, s.first_at)
		FROM (SELECT MIN(created_at) AS first_at FROM audit_log WHERE actor = $1) s
		WHERE u.username = $1 AND s.first_at IS NOT NULL`, to); err != nil {
		return 0, err
	}
	return total, tx.Commit(ctx)
}

// UserStats is one person's footprint in the system, for the profile page and the
// "your year" slide of Fleet Wrapped. Everything is keyed by the snapshotted
// username on attribution columns (see actorColumns), so merged/renamed users
// stay consistent with Activity.
type UserStats struct {
	Username string

	Actions    int64 // audit_log rows, excluding page views
	PageViews  int64 // dashboard pages opened
	ActiveDays int   // distinct days with at least one action
	FirstAt    *time.Time
	LastAt     *time.Time
	TopActions []NamedCount // audit actions, most frequent first (max 6)
	Rank       int          // 1 = most active actor by audit rows; 0 = no actions
	Actors     int          // number of actors ranked

	Commands       int64
	CommandTypes   []NamedCount // install_apk, reboot, … most frequent first
	DevicesTouched int64        // distinct devices directly targeted by their commands
	Installs       int64

	ReleasesSignedOff int64
	ReleasesTested    int64
	ReleasesMerged    int64
	TestResults       int64
	TestsPassed       int64
	TestsFailed       int64
	QueriesCreated    int64
	RecipesCreated    int64
	TestCasesCreated  int64
}

// NamedCount is a label with a count, for small breakdown lists.
type NamedCount struct {
	Name  string
	Count int64
	Pct   int // share of the largest entry in its list, 0–100, for bar widths
}

func fillPct(list []NamedCount) {
	if len(list) == 0 || list[0].Count == 0 {
		return
	}
	for i := range list {
		list[i].Pct = int(100 * list[i].Count / list[0].Count)
	}
}

// TopActors ranks people by recorded actions (page views excluded), with
// display names resolved live from users; former usernames show as-is.
func (d *DB) TopActors(ctx context.Context, limit int, exclude string) ([]NamedCount, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT a.actor, COALESCE(NULLIF(TRIM(u.first_name || ' ' || u.last_name), ''), a.actor), COUNT(*) AS n
		FROM audit_log a JOIN users u ON u.username = a.actor AND `+teamAccountSQL+`
		WHERE a.actor <> $2 AND a.action <> '`+PageViewAction+`'
		GROUP BY a.actor, u.first_name, u.last_name ORDER BY n DESC LIMIT $1`, limit, exclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NamedCount
	for rows.Next() {
		var key string
		var nc NamedCount
		if err := rows.Scan(&key, &nc.Name, &nc.Count); err != nil {
			return nil, err
		}
		out = append(out, nc)
	}
	fillPct(out)
	return out, rows.Err()
}

// ActorSummary is the per-username footprint shown on the Users roster.
type ActorSummary struct {
	Actions  int64
	Commands int64
	LastAt   *time.Time
}

// ActorSummaries returns actions/commands/last-active per username in two
// grouped queries (cheap: audit_log and commands are small tables).
func (d *DB) ActorSummaries(ctx context.Context) (map[string]ActorSummary, error) {
	out := map[string]ActorSummary{}
	rows, err := d.pool.Query(ctx, `SELECT actor, COUNT(*) FILTER (WHERE action <> '`+PageViewAction+`'), MAX(created_at) FROM audit_log WHERE actor <> '' GROUP BY actor`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var u string
		var a ActorSummary
		if err := rows.Scan(&u, &a.Actions, &a.LastAt); err != nil {
			rows.Close()
			return nil, err
		}
		out[u] = a
	}
	rows.Close()
	rows, err = d.pool.Query(ctx, `
		SELECT u, GREATEST(MAX(n), MAX(m)), MAX(t) FROM (
			SELECT created_by AS u, COUNT(*) AS n, 0 AS m, MAX(created_at) AS t FROM commands WHERE created_by <> '' GROUP BY created_by
			UNION ALL
			SELECT actor, 0, COUNT(*), MAX(created_at) FROM audit_log WHERE actor <> '' AND action = 'command.send' GROUP BY actor
		) x GROUP BY u`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u string
		var n int64
		var t *time.Time
		if err := rows.Scan(&u, &n, &t); err != nil {
			return nil, err
		}
		a := out[u]
		a.Commands = n
		if t != nil && (a.LastAt == nil || t.After(*a.LastAt)) {
			a.LastAt = t
		}
		out[u] = a
	}
	return out, rows.Err()
}

// excludeFromRank is a username left out of the rank population (the env admin
// login, which is not a person on the team) — the same exclusion TopActors
// applies, so "#1 of N" on a profile matches the Wrapped leaderboard. When the
// excluded user asks for their own stats, they are ranked against everyone.
func (d *DB) UserStats(ctx context.Context, username, excludeFromRank string) (*UserStats, error) {
	st := &UserStats{Username: username}
	if excludeFromRank == username {
		excludeFromRank = ""
	}
	if username == "" {
		return st, nil
	}
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE action <> '`+PageViewAction+`'),
		       COUNT(*) FILTER (WHERE action = '`+PageViewAction+`'),
		       COUNT(DISTINCT (created_at AT TIME ZONE 'UTC')::date), MIN(created_at), MAX(created_at)
		FROM audit_log WHERE actor = $1`, username).Scan(&st.Actions, &st.PageViews, &st.ActiveDays, &st.FirstAt, &st.LastAt)
	if err != nil {
		return nil, err
	}
	rows, err := d.pool.Query(ctx, `
		SELECT action, COUNT(*) FROM audit_log WHERE actor = $1 AND action <> '`+PageViewAction+`'
		GROUP BY action ORDER BY COUNT(*) DESC, action LIMIT 6`, username)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var nc NamedCount
		if err := rows.Scan(&nc.Name, &nc.Count); err != nil {
			rows.Close()
			return nil, err
		}
		st.TopActions = append(st.TopActions, nc)
	}
	rows.Close()
	if st.Actions > 0 {
		err = d.pool.QueryRow(ctx, `
			WITH r AS (SELECT a.actor, RANK() OVER (ORDER BY COUNT(*) DESC) AS rk, COUNT(*) OVER () AS n
			           FROM audit_log a JOIN users u ON u.username = a.actor AND `+teamAccountSQL+`
			           WHERE a.actor <> $2 AND a.action <> '`+PageViewAction+`' GROUP BY a.actor)
			SELECT rk, n FROM r WHERE actor = $1`, username, excludeFromRank).Scan(&st.Rank, &st.Actors)
		if err != nil && err != pgx.ErrNoRows {
			return nil, err
		}
	}
	// "Commands sent" counts the sends the person made (audit command.send rows —
	// the one record every era of the app wrote), while installs and devices
	// touched come from the command rows that carry a sender.
	err = d.pool.QueryRow(ctx, `
		SELECT GREATEST(
		         (SELECT COUNT(*) FROM audit_log WHERE actor = $1 AND action = 'command.send'),
		         (SELECT COUNT(*) FROM commands WHERE created_by = $1)),
		       (SELECT COUNT(*) FROM commands WHERE created_by = $1 AND type = 'install_apk'),
		       (SELECT COUNT(DISTINCT ct.target_id) FROM commands c2 JOIN command_targets ct ON ct.command_id = c2.id
		        WHERE c2.created_by = $1 AND c2.target_type = 'devices')`, username).Scan(&st.Commands, &st.Installs, &st.DevicesTouched)
	if err != nil {
		return nil, err
	}
	rows, err = d.pool.Query(ctx, `
		SELECT type, COUNT(*) FROM commands WHERE created_by = $1
		GROUP BY type ORDER BY COUNT(*) DESC, type`, username)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var nc NamedCount
		if err := rows.Scan(&nc.Name, &nc.Count); err != nil {
			rows.Close()
			return nil, err
		}
		st.CommandTypes = append(st.CommandTypes, nc)
	}
	rows.Close()
	err = d.pool.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM releases WHERE signed_off_by = $1),
		  (SELECT COUNT(*) FROM releases WHERE testing_done_by = $1),
		  (SELECT COUNT(*) FROM releases WHERE merged_by = $1),
		  (SELECT COUNT(*) FROM release_test_results WHERE tested_by = $1 AND status <> 'untested'),
		  (SELECT COUNT(*) FROM release_test_results WHERE tested_by = $1 AND status = 'pass'),
		  (SELECT COUNT(*) FROM release_test_results WHERE tested_by = $1 AND status = 'fail'),
		  (SELECT COUNT(*) FROM device_queries WHERE created_by = $1),
		  (SELECT COUNT(*) FROM command_recipes WHERE created_by = $1),
		  (SELECT COUNT(*) FROM test_cases WHERE created_by = $1)`, username).Scan(
		&st.ReleasesSignedOff, &st.ReleasesTested, &st.ReleasesMerged, &st.TestResults, &st.TestsPassed, &st.TestsFailed,
		&st.QueriesCreated, &st.RecipesCreated, &st.TestCasesCreated)
	if err != nil {
		return nil, err
	}
	fillPct(st.TopActions)
	fillPct(st.CommandTypes)
	return st, nil
}

func (d *DB) UpdateUserName(ctx context.Context, id uuid.UUID, firstName, lastName string) error {
	_, err := d.pool.Exec(ctx, `UPDATE users SET first_name = $2, last_name = $3 WHERE id = $1`, id, firstName, lastName)
	return err
}

// SetUserPassword replaces a user's bcrypt password hash.
func (d *DB) SetUserPassword(ctx context.Context, id uuid.UUID, passwordHash string) error {
	_, err := d.pool.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, id, passwordHash)
	return err
}

// UserToken backs the sign-up email-verification and forgot-password links: a
// single-use, time-limited token tied to one user and one purpose.
type UserToken struct {
	Token     string
	UserID    uuid.UUID
	Purpose   string // "verify" | "reset"
	ExpiresAt time.Time
	UsedAt    *time.Time
	CreatedAt time.Time
}

// CreateUserToken stores a caller-generated token (random, unguessable — see
// internal/dashboard's newToken helper) for one user/purpose.
func (d *DB) CreateUserToken(ctx context.Context, token string, userID uuid.UUID, purpose string, expiresAt time.Time) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO user_tokens (token, user_id, purpose, expires_at)
		VALUES ($1, $2, $3, $4)
	`, token, userID, purpose, expiresAt)
	return err
}

// PeekUserToken validates a token without consuming it — read-only, safe to call
// from a plain GET. Used to render the verify-email confirm page: many mail
// clients/security gateways prefetch/scan links in an email automatically before
// the user ever clicks, which would silently consume a token on GET and leave the
// user's real click seeing "invalid or expired" even though verification already
// succeeded. Requiring an explicit POST (which prefetchers never submit) before
// ConsumeUserToken actually fires avoids that. Same validity rule as
// ConsumeUserToken (unused, unexpired), same errors.
func (d *DB) PeekUserToken(ctx context.Context, token, purpose string) (uuid.UUID, error) {
	var userID uuid.UUID
	err := d.pool.QueryRow(ctx, `
		SELECT user_id FROM user_tokens
		WHERE token = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > NOW()
	`, token, purpose).Scan(&userID)
	if err != nil {
		return uuid.Nil, err
	}
	return userID, nil
}

// ConsumeUserToken atomically validates and marks a token used in one statement,
// so a token can never be redeemed twice even under concurrent requests. Returns
// the owning user_id, or an error (including pgx.ErrNoRows) if the token is
// missing, wrong purpose, already used, or expired.
func (d *DB) ConsumeUserToken(ctx context.Context, token, purpose string) (uuid.UUID, error) {
	var userID uuid.UUID
	err := d.pool.QueryRow(ctx, `
		UPDATE user_tokens SET used_at = NOW()
		WHERE token = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > NOW()
		RETURNING user_id
	`, token, purpose).Scan(&userID)
	if err != nil {
		return uuid.Nil, err
	}
	return userID, nil
}

// PendingInvites maps each user with an unused, unexpired invite to when the latest
// one was sent — the people who were invited and haven't set a password yet.
func (d *DB) PendingInvites(ctx context.Context) (map[uuid.UUID]time.Time, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT user_id, MAX(created_at) FROM user_tokens
		WHERE purpose = 'invite' AND used_at IS NULL AND expires_at > NOW()
		GROUP BY user_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]time.Time{}
	for rows.Next() {
		var id uuid.UUID
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = at
	}
	return out, rows.Err()
}

// GetUserLayout returns the saved layout JSON for one user + page, or nil when the
// user has never customised that page (caller falls back to the default layout).
func (d *DB) GetUserLayout(ctx context.Context, username, page string) ([]byte, error) {
	var raw []byte
	err := d.pool.QueryRow(ctx, `SELECT layout FROM user_layouts WHERE username = $1 AND page = $2`,
		username, page).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return raw, err
}

// SetUserLayout upserts a user's layout JSON for a page.
func (d *DB) SetUserLayout(ctx context.Context, username, page string, layout []byte) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO user_layouts (username, page, layout, updated_at) VALUES ($1, $2, $3, NOW())
		ON CONFLICT (username, page) DO UPDATE SET layout = EXCLUDED.layout, updated_at = NOW()
	`, username, page, layout)
	return err
}

// DeleteUserLayout removes a user's saved layout so the page returns to its default.
func (d *DB) DeleteUserLayout(ctx context.Context, username, page string) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM user_layouts WHERE username = $1 AND page = $2`, username, page)
	return err
}
