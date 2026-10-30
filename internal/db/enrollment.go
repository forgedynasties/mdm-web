package db

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	prod "mdm/internal/product"
)

// ── Enrollment profiles ───────────────────────────────────────────────────────

// EnrollmentProfile is a named, revocable enrollment token; devices exchange the token
// once for a per-device API key and (optionally) auto-join the profile's group.
type EnrollmentProfile struct {
	ID          uuid.UUID
	Name        string
	Token       string
	GroupID     *uuid.UUID
	GroupName   string
	Notes       string
	CreatedAt   time.Time
	RevokedAt   *time.Time
	EnrollCount int
	// Intent applied to every device that enrolls through this profile.
	DeviceClass    string     // form factor (prod.Class*), "" = not set
	RestaurantID   *uuid.UUID // site the devices are deployed to
	RestaurantName string
	ExpiresAt      *time.Time // token stops enrolling after this
	MaxEnrolls     *int       // token stops enrolling after this many devices
	LastEnrolledAt *time.Time
	CreatedBy      string // username of the account that minted it (enroll app); '' for dashboard/API profiles
}

func (p EnrollmentProfile) Revoked() bool { return p.RevokedAt != nil }

// Expired reports whether the token's time window has closed.
func (p EnrollmentProfile) Expired() bool {
	return p.ExpiresAt != nil && time.Now().After(*p.ExpiresAt)
}

// Exhausted reports whether the token has enrolled its maximum number of devices.
func (p EnrollmentProfile) Exhausted() bool {
	return p.MaxEnrolls != nil && p.EnrollCount >= *p.MaxEnrolls
}

// Active is the single check the enroll endpoint and the dashboard use: not revoked,
// not expired, not exhausted.
func (p EnrollmentProfile) Active() bool { return !p.Revoked() && !p.Expired() && !p.Exhausted() }

// AutoOnboards reports whether devices enrolling through this profile skip the
// onboarding inbox: the profile already says where the device lives (site). Class and
// group are nice-to-have; the site is what makes a device "deployed" in this system.
func (p EnrollmentProfile) AutoOnboards() bool { return p.RestaurantID != nil }

// ClassLabel is the display label for the profile's class ("—" when unset).
func (p EnrollmentProfile) ClassLabel() string { return prod.ClassLabel(p.DeviceClass) }

// EnrollmentProfileInput is what the dashboard collects when creating / editing a profile.
type EnrollmentProfileInput struct {
	Name         string
	Token        string // create only
	GroupID      *uuid.UUID
	Notes        string
	DeviceClass  string
	RestaurantID *uuid.UUID
	ExpiresAt    *time.Time
	MaxEnrolls   *int
	CreatedBy    string
}

func (d *DB) CreateEnrollmentProfile(ctx context.Context, in EnrollmentProfileInput) (uuid.UUID, error) {
	var id uuid.UUID
	err := d.pool.QueryRow(ctx, `
		INSERT INTO enrollment_profiles (name, token, group_id, notes, device_class, restaurant_id, expires_at, max_enrolls, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		in.Name, in.Token, in.GroupID, in.Notes, in.DeviceClass, in.RestaurantID, in.ExpiresAt, in.MaxEnrolls, in.CreatedBy).Scan(&id)
	return id, err
}

// UpdateEnrollmentProfile edits a profile's intent; the token never changes (revoke
// and create a new profile for that).
func (d *DB) UpdateEnrollmentProfile(ctx context.Context, id uuid.UUID, in EnrollmentProfileInput) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE enrollment_profiles
		SET name = $2, group_id = $3, notes = $4, device_class = $5, restaurant_id = $6,
		    expires_at = $7, max_enrolls = $8
		WHERE id = $1`,
		id, in.Name, in.GroupID, in.Notes, in.DeviceClass, in.RestaurantID, in.ExpiresAt, in.MaxEnrolls)
	return err
}

const enrollmentProfileCols = `p.id, p.name, p.token, p.group_id, COALESCE(g.name, ''), p.notes,
		       p.created_at, p.revoked_at, p.enroll_count,
		       p.device_class, p.restaurant_id, COALESCE(r.name, ''), p.expires_at, p.max_enrolls, p.last_enrolled_at, p.created_by`

func scanEnrollmentProfile(row interface{ Scan(...any) error }, p *EnrollmentProfile) error {
	return row.Scan(&p.ID, &p.Name, &p.Token, &p.GroupID, &p.GroupName, &p.Notes,
		&p.CreatedAt, &p.RevokedAt, &p.EnrollCount,
		&p.DeviceClass, &p.RestaurantID, &p.RestaurantName, &p.ExpiresAt, &p.MaxEnrolls, &p.LastEnrolledAt, &p.CreatedBy)
}

func (d *DB) ListEnrollmentProfiles(ctx context.Context) ([]EnrollmentProfile, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT `+enrollmentProfileCols+`
		FROM enrollment_profiles p
		LEFT JOIN groups g ON g.id = p.group_id
		LEFT JOIN restaurants r ON r.id = p.restaurant_id
		ORDER BY p.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EnrollmentProfile
	for rows.Next() {
		var p EnrollmentProfile
		if err := scanEnrollmentProfile(rows, &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *DB) GetEnrollmentProfile(ctx context.Context, id uuid.UUID) (*EnrollmentProfile, error) {
	var p EnrollmentProfile
	err := scanEnrollmentProfile(d.pool.QueryRow(ctx, `
		SELECT `+enrollmentProfileCols+`
		FROM enrollment_profiles p
		LEFT JOIN groups g ON g.id = p.group_id
		LEFT JOIN restaurants r ON r.id = p.restaurant_id
		WHERE p.id = $1`, id), &p)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ActiveEnrollmentProfileByToken resolves a usable profile from its token: not
// revoked, not past expires_at, not at max_enrolls. Returns pgx.ErrNoRows for unknown,
// revoked, expired and exhausted tokens alike (indistinguishable on purpose).
func (d *DB) ActiveEnrollmentProfileByToken(ctx context.Context, token string) (*EnrollmentProfile, error) {
	var p EnrollmentProfile
	err := scanEnrollmentProfile(d.pool.QueryRow(ctx, `
		SELECT `+enrollmentProfileCols+`
		FROM enrollment_profiles p
		LEFT JOIN groups g ON g.id = p.group_id
		LEFT JOIN restaurants r ON r.id = p.restaurant_id
		WHERE p.token = $1 AND p.revoked_at IS NULL
		  AND (p.expires_at IS NULL OR p.expires_at > NOW())
		  AND (p.max_enrolls IS NULL OR p.enroll_count < p.max_enrolls)`, token), &p)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (d *DB) SetEnrollmentProfileRevoked(ctx context.Context, id uuid.UUID, revoked bool) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE enrollment_profiles
		SET revoked_at = CASE WHEN $2 THEN NOW() ELSE NULL END
		WHERE id = $1`, id, revoked)
	return err
}

func (d *DB) DeleteEnrollmentProfile(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM enrollment_profiles WHERE id = $1`, id)
	return err
}

// DeleteExpiredEnrollmentProfiles removes every profile whose expiry has passed. Devices
// they enrolled keep working — a profile is only a token for new enrolments. Returns how
// many went, for the toast.
func (d *DB) DeleteExpiredEnrollmentProfiles(ctx context.Context) (int64, error) {
	tag, err := d.pool.Exec(ctx, `DELETE FROM enrollment_profiles WHERE expires_at IS NOT NULL AND expires_at < NOW()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// EnrollResult is what EnrollDevice learned, returned to the agent so it can show the
// operator where the device landed.
type EnrollResult struct {
	DeviceID   uuid.UUID
	ReEnrolled bool // the serial already existed: key rotated, history kept
	Onboarded  bool // profile pre-assigned a site, so the device skipped the inbox
}

// EnrollDevice registers (or re-registers, e.g. after factory reset) a device under an
// enrollment profile: upserts the device row as a DPC device, stores the new key hash
// (rotating out any previous key), applies the profile's intent (site, group),
// and bumps the profile counter. A profile that names a site auto-onboards the device;
// otherwise it waits in the onboarding inbox (onboarded_at NULL). Re-enrolling keeps
// the device's existing site/onboarded state unless the profile overrides them.
func (d *DB) EnrollDevice(ctx context.Context, profile *EnrollmentProfile, serial, product, keyHash string) (EnrollResult, error) {
	var res EnrollResult
	// The class comes from the model (the role it is attached to on Lineup). A model with
	// several roles takes the one picked in AIO Enroll (device_role_requests), else none
	// until someone picks on the device page. A model not attached yet leaves it empty and
	// the first check-in fills a guess from the model it reports.
	var roles []string
	if product != "" {
		roles, _ = d.ProductRoles(ctx, product)
	}
	var class, source string
	if len(roles) == 1 {
		class = roles[0]
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)
	if req, err := takeRoleRequest(ctx, tx, serial); err != nil {
		return res, err
	} else if len(roles) > 1 && slices.Contains(roles, req) {
		class, source = req, "enroll"
	}

	var onboardedAt *time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO devices (serial_number, product, device_key_hash, enrolled_via,
		                     agent_kind, enrollment_status, enrolled_at, device_class, restaurant_id, onboarded_at, enrolled_by, class_source)
		VALUES ($1, $2, $3, $4, 'dpc', 'enrolled', NOW(), $5, $6,
		        CASE WHEN $6::uuid IS NOT NULL THEN NOW() ELSE NULL END, $7, $8)
		ON CONFLICT (serial_number) DO UPDATE
		SET device_key_hash   = EXCLUDED.device_key_hash,
		    key_rotated_at    = CASE WHEN devices.device_key_hash IS NOT NULL THEN NOW() ELSE devices.key_rotated_at END,
		    enrolled_via      = EXCLUDED.enrolled_via,
		    agent_kind        = 'dpc',
		    enrollment_status = 'enrolled',
		    enrolled_at       = CASE WHEN devices.enrollment_status IN ('retired', 'wiped', 'unenrolled') THEN NOW() ELSE devices.enrolled_at END,
		    hidden            = false,
		    product           = CASE WHEN EXCLUDED.product <> '' THEN EXCLUDED.product ELSE devices.product END,
		    -- A class pinned to this serial stays, unless AIO Enroll was asked for one.
		    device_class      = CASE WHEN devices.class_source = 'pin' AND EXCLUDED.class_source <> 'enroll' THEN devices.device_class
		                             WHEN EXCLUDED.device_class <> '' THEN EXCLUDED.device_class ELSE devices.device_class END,
		    class_source      = CASE WHEN devices.class_source = 'pin' AND EXCLUDED.class_source <> 'enroll' THEN 'pin'
		                             WHEN EXCLUDED.device_class <> '' THEN EXCLUDED.class_source ELSE devices.class_source END,
		    restaurant_id     = COALESCE(EXCLUDED.restaurant_id, devices.restaurant_id),
		    onboarded_at      = COALESCE(devices.onboarded_at, EXCLUDED.onboarded_at),
		    enrolled_by       = CASE WHEN EXCLUDED.enrolled_by <> '' THEN EXCLUDED.enrolled_by ELSE devices.enrolled_by END
		RETURNING id, (xmax <> 0), onboarded_at`,
		serial, product, keyHash, profile.ID, class, profile.RestaurantID, profile.CreatedBy, source).
		Scan(&res.DeviceID, &res.ReEnrolled, &onboardedAt)
	if err != nil {
		return res, err
	}
	res.Onboarded = onboardedAt != nil
	if profile.GroupID != nil {
		if _, err := tx.Exec(ctx, `
			INSERT INTO device_groups (device_id, group_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, res.DeviceID, *profile.GroupID); err != nil {
			return res, err
		}
	}
	if _, err = tx.Exec(ctx, `
		UPDATE enrollment_profiles
		SET enroll_count = enroll_count + 1, last_enrolled_at = NOW()
		WHERE id = $1`, profile.ID); err != nil {
		return res, err
	}
	return res, tx.Commit(ctx)
}

// ── Device lifecycle (onboarding inbox, retire) ───────────────────────────────

// MarkOnboarded confirms a device's placement and takes it out of the onboarding
// inbox. Idempotent.
func (d *DB) MarkOnboarded(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE devices SET onboarded_at = COALESCE(onboarded_at, NOW()) WHERE id = $1`, id)
	return err
}

// SetEnrollmentStatus moves a device through its lifecycle (retired / wiped / back to
// its enrolled state). Retired devices keep their history and placement.
func (d *DB) SetEnrollmentStatus(ctx context.Context, id uuid.UUID, status string) error {
	_, err := d.pool.Exec(ctx, `UPDATE devices SET enrollment_status = $2 WHERE id = $1`, id, status)
	return err
}

// ListOnboardingInbox returns devices nobody has confirmed yet (onboarded_at NULL),
// newest enrollment first. Retired/wiped and hidden devices are excluded.
func (d *DB) ListOnboardingInbox(ctx context.Context, limit int) ([]Device, error) {
	if limit <= 0 {
		limit = 100
	}
	return d.ListDevices(ctx, DeviceFilter{Onboarding: "pending"}, 0, limit, "enrolled_at", "desc")
}

// CountOnboardingInbox is the badge number for the inbox.
func (d *DB) CountOnboardingInbox(ctx context.Context) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM devices
		WHERE onboarded_at IS NULL AND NOT hidden AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')`).Scan(&n)
	return n, err
}

// ResolveSerials splits a list of client-typed serials into the ones that name an
// active device (returned in their canonical stored form) and the rest. Matching is
// case-insensitive so a pasted list survives a lower-cased spreadsheet.
func (d *DB) ResolveSerials(ctx context.Context, serials []string) (found, missing []string, err error) {
	rows, err := d.pool.Query(ctx, `
		SELECT serial_number FROM devices
		WHERE NOT hidden AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')
		  AND LOWER(serial_number) = ANY($1)`, lowerAll(serials))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	known := map[string]string{}
	for rows.Next() {
		var sn string
		if err := rows.Scan(&sn); err != nil {
			return nil, nil, err
		}
		known[strings.ToLower(sn)] = sn
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	for _, s := range serials {
		k := strings.ToLower(strings.TrimSpace(s))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		if sn, ok := known[k]; ok {
			found = append(found, sn)
		} else {
			missing = append(missing, strings.TrimSpace(s))
		}
	}
	return found, missing, nil
}

func lowerAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// DeviceSerialByKeyHash resolves the device bound to a per-device API key (by hash).
// Used by device-API auth to derive the acting device from its credential.
func (d *DB) DeviceSerialByKeyHash(ctx context.Context, keyHash string) (uuid.UUID, string, error) {
	var id uuid.UUID
	var serial string
	err := d.pool.QueryRow(ctx, `
		SELECT id, serial_number FROM devices WHERE device_key_hash = $1`, keyHash).Scan(&id, &serial)
	return id, serial, err
}
