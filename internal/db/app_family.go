package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// App families: one product shipped as several packages (aio.app.nugget,
// aio.app.nugget.internal, aio.app.nugget.uatv2) and several versions of each.
// The library shows one tile per family; installs pick variant + version.
// Versions of the same package always group; variants group per the
// app_family_mode setting (auto / suggest / off), with manual overrides here.

const appFamilySchema = `
CREATE TABLE IF NOT EXISTS app_families (
	id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
	name         TEXT        NOT NULL,
	base_package TEXT        NOT NULL DEFAULT '',
	created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
ALTER TABLE apps ADD COLUMN IF NOT EXISTS family_id UUID REFERENCES app_families(id) ON DELETE SET NULL;
ALTER TABLE apps ADD COLUMN IF NOT EXISTS variant TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN IF NOT EXISTS family_pinned BOOLEAN NOT NULL DEFAULT false;
`

// AppFamily is the umbrella for a set of package variants.
type AppFamily struct {
	ID          uuid.UUID
	Name        string
	BasePackage string
	CreatedAt   time.Time
}

func (d *DB) ListAppFamilies(ctx context.Context) ([]AppFamily, error) {
	rows, err := d.pool.Query(ctx, `SELECT id, name, base_package, created_at FROM app_families ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppFamily
	for rows.Next() {
		var f AppFamily
		if err := rows.Scan(&f.ID, &f.Name, &f.BasePackage, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (d *DB) CreateAppFamily(ctx context.Context, name, basePackage string) (uuid.UUID, error) {
	var id uuid.UUID
	err := d.pool.QueryRow(ctx, `INSERT INTO app_families (name, base_package) VALUES ($1, $2) RETURNING id`, name, basePackage).Scan(&id)
	return id, err
}

func (d *DB) RenameAppFamily(ctx context.Context, id uuid.UUID, name string) error {
	_, err := d.pool.Exec(ctx, `UPDATE app_families SET name = $2 WHERE id = $1`, id, name)
	return err
}

// DeleteAppFamily ungroups: member apps stay, detached and pinned so auto mode
// does not regroup them straight away.
func (d *DB) DeleteAppFamily(ctx context.Context, id uuid.UUID) error {
	if _, err := d.pool.Exec(ctx, `UPDATE apps SET family_id = NULL, family_pinned = true WHERE family_id = $1`, id); err != nil {
		return err
	}
	_, err := d.pool.Exec(ctx, `DELETE FROM app_families WHERE id = $1`, id)
	return err
}

// SetAppFamily assigns (or, with a nil family, detaches) every row of a package.
// pinned marks a manual decision that auto grouping must respect.
func (d *DB) SetAppFamily(ctx context.Context, packageName string, familyID *uuid.UUID, variant string, pinned bool) error {
	_, err := d.pool.Exec(ctx, `UPDATE apps SET family_id = $2, variant = $3, family_pinned = $4 WHERE package_name = $1`, packageName, familyID, variant, pinned)
	return err
}

// DeleteEmptyAppFamilies removes families with no member left.
func (d *DB) DeleteEmptyAppFamilies(ctx context.Context) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM app_families f WHERE NOT EXISTS (SELECT 1 FROM apps a WHERE a.family_id = f.id)`)
	return err
}
