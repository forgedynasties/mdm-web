package db

import (
	"context"
	"time"
)

// Enroller is one account that has enrolled devices through AIO Enroll, with how many.
type Enroller struct {
	Username string
	Count    int
}

// ListEnrollers is every username on devices.enrolled_by, most devices first — the
// fleet list's "Enrolled by" filter.
func (d *DB) ListEnrollers(ctx context.Context) ([]Enroller, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT enrolled_by, COUNT(*) FROM devices
		WHERE enrolled_by <> '' AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')
		GROUP BY enrolled_by ORDER BY COUNT(*) DESC, enrolled_by`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Enroller
	for rows.Next() {
		var e Enroller
		if err := rows.Scan(&e.Username, &e.Count); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Enrollment is one device's enrollment, for the Enrollment page's log and a person's
// profile: who, when, what. EnrolledBy is "" for devices enrolled before AIO Enroll
// recorded it, or moved from another server by hand.
type Enrollment struct {
	Serial         string
	DeviceClass    string
	AgentKind      string
	RestaurantName string
	EnrolledAt     time.Time
	EnrolledBy     string
}

// RecentEnrollments lists enrollments newest first: standard-client devices only, since a
// firmware device is onboarded by its image and nobody "enrolls" it. by narrows to one
// username ("" = everyone); since drops older ones (zero = no limit).
func (d *DB) RecentEnrollments(ctx context.Context, by string, since time.Time, limit int) ([]Enrollment, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.serial_number, d.device_class, d.agent_kind, COALESCE(r.name, ''), d.enrolled_at, d.enrolled_by
		FROM devices d LEFT JOIN restaurants r ON r.id = d.restaurant_id
		WHERE d.enrollment_status NOT IN ('retired', 'wiped', 'unenrolled') AND d.agent_kind = 'dpc'
		  AND ($1 = '' OR d.enrolled_by = $1)
		  AND ($2::timestamptz IS NULL OR d.enrolled_at >= $2)
		ORDER BY d.enrolled_at DESC LIMIT $3`, by, nullTime(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Enrollment
	for rows.Next() {
		var e Enrollment
		if err := rows.Scan(&e.Serial, &e.DeviceClass, &e.AgentKind, &e.RestaurantName, &e.EnrolledAt, &e.EnrolledBy); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
