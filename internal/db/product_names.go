package db

import (
	"context"
	"strings"

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
	Name         string // admin-set display name, "" when none
	Role         string // admin-set default role for the model, "" when none
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
		       COALESCE(MAX(pn.name), ''), COALESCE(MAX(pn.role), ''), COALESCE(MAX(pn.updated_by), '')
		FROM devices d
		LEFT JOIN product_names pn ON pn.product = COALESCE(NULLIF(d.product, ''), '`+prod.DefaultKey+`')
		WHERE NOT d.hidden
		GROUP BY p ORDER BY COUNT(*) DESC, p`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProductRow
	for rows.Next() {
		var r ProductRow
		if err := rows.Scan(&r.Key, &r.Devices, &r.Firmware, &r.DPC, &r.Lite, &r.Manufacturer, &r.Model, &r.Name, &r.Role, &r.UpdatedBy); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ProductRole is a stock model's default role ("" when none).
func (d *DB) ProductRole(ctx context.Context, key string) (string, error) {
	var role string
	err := d.pool.QueryRow(ctx, `SELECT role FROM product_names WHERE product = $1`, prod.Normalize(key)).Scan(&role)
	if err != nil {
		return "", nil // no row: no default
	}
	return role, nil
}

// SetProductRole sets a stock model's default role and applies it to every device of
// that model, returning how many devices changed. New devices of the model take it
// at enrollment (see UpsertDevice). A device can still be moved to another role on
// its own page afterwards; setting the model's role again re-applies it to all.
func (d *DB) SetProductRole(ctx context.Context, key, role, by string) (int64, error) {
	key = prod.Normalize(key)
	if _, err := d.pool.Exec(ctx, `
		INSERT INTO product_names (product, name, role, updated_by, updated_at) VALUES ($1, '', $2, $3, now())
		ON CONFLICT (product) DO UPDATE SET role = EXCLUDED.role, updated_by = EXCLUDED.updated_by, updated_at = now()`,
		key, role, by); err != nil {
		return 0, err
	}
	if role == "" {
		return 0, nil
	}
	tag, err := d.pool.Exec(ctx, `UPDATE devices SET device_class = $2 WHERE product = $1 AND device_class <> $2`, key, role)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
