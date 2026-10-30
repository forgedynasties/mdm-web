package db

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	prod "mdm/internal/product"
)

// productNamesSchema holds admin-set display names for products. Our own hardware
// has catalog labels; a stock product key ("rockchip029", "a14xmtfn") means nothing
// to a person, so an admin can name it ("Menu board TV box").
const productNamesSchema = `
CREATE TABLE IF NOT EXISTS product_names (
	product    TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	updated_by TEXT NOT NULL DEFAULT '',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Default role (device class) for every device of a stock model: many vendor models
-- serve one role (POS terminal, menu board, …). '' = no default.
ALTER TABLE product_names ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT '';
-- A model can serve several roles (29 Oct 2026: the Galaxy Tab A9+ is a POS terminal or a
-- kitchen display). roles is the truth; role keeps the single role of a one-role model
-- ('' with none or several) for readers that only know one.
ALTER TABLE product_names ADD COLUMN IF NOT EXISTS roles TEXT[] NOT NULL DEFAULT '{}';
UPDATE product_names SET roles = ARRAY[role] WHERE role <> '' AND cardinality(roles) = 0;
-- Where a device's class came from: '' its model (or a guess), 'set' a person on its
-- page, 'enroll' picked in AIO Enroll, 'backend' the AIO backend's device type.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS class_source TEXT NOT NULL DEFAULT '';
-- A role picked in AIO Enroll before the device has enrolled; EnrollDevice applies it.
CREATE TABLE IF NOT EXISTS device_role_requests (
	serial     TEXT PRIMARY KEY,
	class      TEXT NOT NULL,
	requested_by TEXT NOT NULL DEFAULT '',
	requested_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

// ProductNames returns every admin-set product display name, keyed by normalized product key.
func (d *DB) ProductNames(ctx context.Context) (map[string]string, error) {
	rows, err := d.pool.Query(ctx, `SELECT product, name FROM product_names WHERE name <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, n string
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// SetProductName sets (or, with an empty name, clears) a product's display name.
func (d *DB) SetProductName(ctx context.Context, key, name, by string) error {
	key = prod.Normalize(key)
	name = strings.TrimSpace(name)
	if name == "" {
		// Keep the row if it still carries a default role.
		_, err := d.pool.Exec(ctx, `UPDATE product_names SET name = '', updated_by = $2, updated_at = now() WHERE product = $1`, key, by)
		return err
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO product_names (product, name, updated_by, updated_at) VALUES ($1, $2, $3, now())
		ON CONFLICT (product) DO UPDATE SET name = EXCLUDED.name, updated_by = EXCLUDED.updated_by, updated_at = now()`,
		key, name, by)
	return err
}

// ProductRow is one line of the Products page: a product the fleet reports, how many
// devices run it under each client type, and what the devices say they are.
type ProductRow struct {
	Key          string
	Devices      int
	Firmware     int // MDM Firmware
	DPC          int // MDM DPC
	Lite         int // MDM Lite
	Manufacturer string
	Model        string
	Name         string   // admin-set display name, "" when none
	Role         string   // the model's role when it has exactly one, "" otherwise
	Roles        []string // every role the model is attached to
	UpdatedBy    string
}

// ProductOverview lists every product with at least one (non-hidden) device.
func (d *DB) ProductOverview(ctx context.Context) ([]ProductRow, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT COALESCE(NULLIF(d.product, ''), '`+prod.DefaultKey+`') AS p,
		       COUNT(*)::int,
		       COUNT(*) FILTER (WHERE d.agent_kind <> '`+prod.KindDPC+`')::int,
		       COUNT(*) FILTER (WHERE d.agent_kind = '`+prod.KindDPC+`' AND COALESCE(d.latest_extra->>'agent_type', '') <> '`+prod.AgentTypeMDMLite+`')::int,
		       COUNT(*) FILTER (WHERE d.agent_kind = '`+prod.KindDPC+`' AND COALESCE(d.latest_extra->>'agent_type', '') = '`+prod.AgentTypeMDMLite+`')::int,
		       COALESCE((array_agg(d.latest_extra->>'manufacturer') FILTER (WHERE d.latest_extra->>'manufacturer' <> ''))[1], ''),
		       COALESCE((array_agg(d.latest_extra->>'model') FILTER (WHERE d.latest_extra->>'model' <> ''))[1], ''),
		       COALESCE(MAX(pn.name), ''), COALESCE(MAX(pn.role), ''), COALESCE(pn.roles, '{}'), COALESCE(MAX(pn.updated_by), '')
		FROM devices d
		LEFT JOIN product_names pn ON pn.product = COALESCE(NULLIF(d.product, ''), '`+prod.DefaultKey+`')
		WHERE NOT d.hidden
		GROUP BY p, pn.roles ORDER BY COUNT(*) DESC, p`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProductRow
	for rows.Next() {
		var r ProductRow
		if err := rows.Scan(&r.Key, &r.Devices, &r.Firmware, &r.DPC, &r.Lite, &r.Manufacturer, &r.Model, &r.Name, &r.Role, &r.Roles, &r.UpdatedBy); err != nil {
			return nil, err
		}
		r.Roles = liveRoles(r.Roles)
		out = append(out, r)
	}
	return out, rows.Err()
}

// liveRoles drops roles whose class has since been retired.
func liveRoles(roles []string) []string {
	out := roles[:0:0]
	for _, r := range roles {
		if prod.IsClass(r) {
			out = append(out, r)
		}
	}
	return out
}

// ProductRole is a stock model's role when it has exactly one ("" with none or several):
// the class every device of the model takes. A model with several roles leaves each
// device's own class alone (see ProductRoles).
func (d *DB) ProductRole(ctx context.Context, key string) (string, error) {
	roles, _ := d.ProductRoles(ctx, key)
	if len(roles) != 1 {
		return "", nil
	}
	return roles[0], nil
}

// ProductRoles lists every role a stock model is attached to (nil when none).
func (d *DB) ProductRoles(ctx context.Context, key string) ([]string, error) {
	var roles []string
	err := d.pool.QueryRow(ctx, `SELECT roles FROM product_names WHERE product = $1`, prod.Normalize(key)).Scan(&roles)
	if err != nil {
		return nil, nil // no row: no role
	}
	return liveRoles(roles), nil
}

// SetProductRole makes role the model's only role (an empty role detaches it from all) and
// applies it to every device of the model, returning how many devices changed. New
// devices of the model take it at enrollment (see EnrollDevice and UpsertCheckin).
func (d *DB) SetProductRole(ctx context.Context, key, role, by string) (int64, error) {
	var roles []string
	if role != "" {
		roles = []string{role}
	}
	return d.setProductRoles(ctx, prod.Normalize(key), roles, by)
}

// AttachProductRole adds role to the model's roles; the roles it had stay. Devices that
// already have one of the model's roles keep it.
func (d *DB) AttachProductRole(ctx context.Context, key, role, by string) (int64, error) {
	key = prod.Normalize(key)
	roles, _ := d.ProductRoles(ctx, key)
	for _, r := range roles {
		if r == role {
			return 0, nil
		}
	}
	return d.setProductRoles(ctx, key, append(roles, role), by)
}

// DetachProductRole takes role away from the model. Its devices in that role follow the
// model's one remaining role, or have none until someone picks (several left or none).
func (d *DB) DetachProductRole(ctx context.Context, key, role, by string) (int64, error) {
	key = prod.Normalize(key)
	roles, _ := d.ProductRoles(ctx, key)
	var keep []string
	for _, r := range roles {
		if r != role {
			keep = append(keep, r)
		}
	}
	return d.setProductRoles(ctx, key, keep, by)
}

// setProductRoles stores the model's roles and brings its devices in line: with one role
// every device takes it; otherwise a device keeps its class only while it is one of the
// roles, and the rest have none (the role app and lock wait until someone picks).
func (d *DB) setProductRoles(ctx context.Context, key string, roles []string, by string) (int64, error) {
	if roles == nil {
		roles = []string{}
	}
	single := ""
	if len(roles) == 1 {
		single = roles[0]
	}
	if _, err := d.pool.Exec(ctx, `
		INSERT INTO product_names (product, name, role, roles, updated_by, updated_at) VALUES ($1, '', $2, $3, $4, now())
		ON CONFLICT (product) DO UPDATE SET role = EXCLUDED.role, roles = EXCLUDED.roles, updated_by = EXCLUDED.updated_by, updated_at = now()`,
		key, single, roles, by); err != nil {
		return 0, err
	}
	if single != "" {
		tag, err := d.pool.Exec(ctx, `UPDATE devices SET device_class = $2, class_source = '' WHERE product = $1 AND device_class <> $2 AND class_source <> 'pin'`, key, single)
		if err != nil {
			return 0, err
		}
		return tag.RowsAffected(), nil
	}
	// No role: its devices lose the class it gave them; the next check-in fills a guess
	// from the model they report, if there is one. Several: only a class the model no
	// longer has is cleared.
	tag, err := d.pool.Exec(ctx, `UPDATE devices SET device_class = '', class_source = '' WHERE product = $1 AND device_class <> '' AND NOT (device_class = ANY($2)) AND class_source <> 'pin'`, key, roles)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// SetDeviceClass gives one device of a several-role model its role (source: "set",
// "enroll" or "backend"). The class must be one of its model's roles.
func (d *DB) SetDeviceClass(ctx context.Context, serial, class, source string) error {
	tag, err := d.pool.Exec(ctx, `
		UPDATE devices d SET device_class = $2, class_source = $3
		FROM product_names pn
		WHERE d.serial_number = $1 AND pn.product = d.product AND $2 = ANY(pn.roles) AND cardinality(pn.roles) > 1`,
		serial, class, source)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrRoleNotAllowed
	}
	return nil
}

// PinDeviceClass sets one Standard-client device's class by its serial, any class
// (class_source "pin"; user, 30 Oct 2026: "allow engineers to change the class of each
// device … per serial not per model"). Check-ins, Lineup changes to its model and
// enrolment leave it (AIO Enroll's own pick aside) until UnpinDeviceClass. Firmware
// devices are refused: their class is their product's.
func (d *DB) PinDeviceClass(ctx context.Context, serial, class string) error {
	tag, err := d.pool.Exec(ctx, `UPDATE devices SET device_class = $2, class_source = 'pin'
		WHERE serial_number = $1 AND agent_kind <> 'firmware'`, serial, class)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrRoleNotAllowed
	}
	return nil
}

// UnpinDeviceClass hands a pinned device back to its model: the model's one role, else
// its own class while it is one of the model's roles, else none.
func (d *DB) UnpinDeviceClass(ctx context.Context, serial string) error {
	_, err := d.pool.Exec(ctx, `UPDATE devices d SET class_source = '',
		device_class = COALESCE((SELECT CASE WHEN pn.role <> '' THEN pn.role
		                                     WHEN d.device_class = ANY(pn.roles) THEN d.device_class ELSE '' END
		                         FROM product_names pn WHERE pn.product = d.product), d.device_class)
		WHERE d.serial_number = $1 AND d.class_source = 'pin'`, serial)
	return err
}

// DeviceClassSource is where the device's class came from ("" its model).
func (d *DB) DeviceClassSource(ctx context.Context, serial string) (string, error) {
	var src string
	err := d.pool.QueryRow(ctx, `SELECT class_source FROM devices WHERE serial_number = $1`, serial).Scan(&src)
	return src, err
}

// ErrRoleNotAllowed: the device is unknown, or its model has one role (or none), or the
// class is not one of the model's roles.
var ErrRoleNotAllowed = errors.New("that role is not one of the model's roles")

// RequestDeviceRole stores a role picked in AIO Enroll for a device that has not enrolled
// yet; EnrollDevice applies it (when it is one of the model's roles) and drops it.
func (d *DB) RequestDeviceRole(ctx context.Context, serial, class, by string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO device_role_requests (serial, class, requested_by, requested_at) VALUES ($1, $2, $3, now())
		ON CONFLICT (serial) DO UPDATE SET class = EXCLUDED.class, requested_by = EXCLUDED.requested_by, requested_at = now()`,
		serial, class, by)
	return err
}

// takeRoleRequest returns and removes the role requested for serial within the last
// day ("" when none).
func takeRoleRequest(ctx context.Context, tx pgx.Tx, serial string) (string, error) {
	var class string
	err := tx.QueryRow(ctx, `DELETE FROM device_role_requests WHERE serial = $1 RETURNING CASE WHEN requested_at > now() - interval '1 day' THEN class ELSE '' END`, serial).Scan(&class)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return class, err
}

// ProductClassCounts counts the (non-hidden) devices of each product in each class:
// [product][class] → n. A several-role model's devices with no role yet are under "".
func (d *DB) ProductClassCounts(ctx context.Context) (map[string]map[string]int, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT COALESCE(NULLIF(product, ''), '`+prod.DefaultKey+`'), device_class, COUNT(*)::int
		FROM devices WHERE NOT hidden GROUP BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int{}
	for rows.Next() {
		var p, c string
		var n int
		if err := rows.Scan(&p, &c, &n); err != nil {
			return nil, err
		}
		if out[p] == nil {
			out[p] = map[string]int{}
		}
		out[p][c] += n
	}
	return out, rows.Err()
}

// AttachedModels lists every stock model attached to a class, devices or not: a model can
// be attached before any device of it has enrolled.
func (d *DB) AttachedModels(ctx context.Context) ([]ProductRow, error) {
	rows, err := d.pool.Query(ctx, `SELECT product, name, role, roles, updated_by FROM product_names WHERE cardinality(roles) > 0 ORDER BY product`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProductRow
	for rows.Next() {
		var r ProductRow
		if err := rows.Scan(&r.Key, &r.Name, &r.Role, &r.Roles, &r.UpdatedBy); err != nil {
			return nil, err
		}
		r.Roles = liveRoles(r.Roles)
		out = append(out, r)
	}
	return out, rows.Err()
}
