package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ── Apps ──────────────────────────────────────────────────────────────────────

type App struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	ApkURL      string    `json:"apk_url"`
	PackageName string    `json:"package_name"`
	VersionName string    `json:"version_name,omitempty"` // parsed from the APK manifest at upload, "" if unknown
	S3Key       string    `json:"s3_key,omitempty"`       // set for S3-hosted uploads; empty for URL apps
	Icon        string    `json:"icon,omitempty"`         // resolved from app_icons on the read path
	CreatedAt   time.Time `json:"created_at"`
	// Family grouping (app_family.go). Variant is "prod" for the base package.
	FamilyID     *uuid.UUID `json:"family_id,omitempty"`
	Variant      string     `json:"variant,omitempty"`
	FamilyPinned bool       `json:"-"`
}

func (d *DB) ListApps(ctx context.Context) ([]App, error) {
	// Resolve each app's icon from the shared app_icons index (populated when an APK is
	// uploaded and parsed) so the library can render real icons.
	rows, err := d.pool.Query(ctx, `
		SELECT a.id, a.name, a.apk_url, a.package_name, a.version_name, a.created_at, COALESCE(ai.icon, ''), a.family_id, a.variant, a.family_pinned
		FROM apps a
		LEFT JOIN app_icons ai ON ai.package_name = a.package_name
		ORDER BY a.name ASC, a.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		var a App
		if err := rows.Scan(&a.ID, &a.Name, &a.ApkURL, &a.PackageName, &a.VersionName, &a.CreatedAt, &a.Icon, &a.FamilyID, &a.Variant, &a.FamilyPinned); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (d *DB) GetApp(ctx context.Context, id uuid.UUID) (*App, error) {
	var a App
	err := d.pool.QueryRow(ctx,
		`SELECT id, name, apk_url, package_name, version_name, COALESCE(s3_key, ''), created_at FROM apps WHERE id = $1`, id,
	).Scan(&a.ID, &a.Name, &a.ApkURL, &a.PackageName, &a.VersionName, &a.S3Key, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// CreateS3App inserts a repository app backed by an S3 object. The apk_url is the
// server's own stable proxy (baseURL + /apps/{id}/apk) that 302-redirects to a fresh
// presigned download URL, so the bucket stays private and the URL never expires. The
// device fetches apk_url directly, so baseURL must be the device-reachable origin.
func (d *DB) CreateS3App(ctx context.Context, name, packageName, versionName, s3Key, baseURL string) (*App, error) {
	id := uuid.New()
	apkURL := fmt.Sprintf("%s/apps/%s/apk", strings.TrimRight(baseURL, "/"), id)
	var a App
	err := d.pool.QueryRow(ctx,
		`INSERT INTO apps (id, name, apk_url, package_name, version_name, s3_key) VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, name, apk_url, package_name, version_name, COALESCE(s3_key, ''), created_at`,
		id, name, apkURL, packageName, versionName, s3Key,
	).Scan(&a.ID, &a.Name, &a.ApkURL, &a.PackageName, &a.VersionName, &a.S3Key, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// GetAppIconsByPackages resolves base64 PNG icons for a set of package names from the
// shared app_icons index in one query, for pages (e.g. Alerts) that show several
// crash/ANR cards at once and want each app's real icon instead of a generic glyph.
// Packages with no known icon are simply absent from the returned map.
func (d *DB) GetAppIconsByPackages(ctx context.Context, packages []string) (map[string]string, error) {
	out := map[string]string{}
	if len(packages) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx,
		`SELECT package_name, icon FROM app_icons WHERE package_name = ANY($1)`, packages)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pkg, icon string
		if err := rows.Scan(&pkg, &icon); err != nil {
			return nil, err
		}
		out[pkg] = icon
	}
	return out, rows.Err()
}

// UpsertAppIcon stores one package's launcher icon in the shared app_icons index, so
// it shows across all devices immediately (same index the device-reported icons use).
func (d *DB) UpsertAppIcon(ctx context.Context, packageName, iconB64 string) error {
	if packageName == "" || iconB64 == "" {
		return nil
	}
	_, err := d.pool.Exec(ctx,
		`INSERT INTO app_icons (package_name, icon) VALUES ($1, $2)
		 ON CONFLICT (package_name) DO UPDATE SET icon = EXCLUDED.icon, updated_at = NOW()`,
		packageName, iconB64)
	return err
}

func (d *DB) CreateApp(ctx context.Context, name, apkURL, packageName string) (*App, error) {
	var a App
	err := d.pool.QueryRow(ctx,
		`INSERT INTO apps (name, apk_url, package_name) VALUES ($1, $2, $3) RETURNING id, name, apk_url, package_name, created_at`,
		name, apkURL, packageName,
	).Scan(&a.ID, &a.Name, &a.ApkURL, &a.PackageName, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	// A known package lets installs dedup against already-installed devices from
	// the start, rather than waiting for a first install to learn the mapping.
	if packageName != "" {
		_ = d.LearnApkPackage(ctx, apkURL, packageName)
	}
	return &a, err
}

// UpdateApp edits an app's display name, APK URL and package name. When a package
// name is set it re-seeds the apk_url → package map so dedup stays accurate.
func (d *DB) UpdateApp(ctx context.Context, id uuid.UUID, name, apkURL, packageName string) (*App, error) {
	var a App
	err := d.pool.QueryRow(ctx,
		`UPDATE apps SET name = $2, apk_url = $3, package_name = $4 WHERE id = $1
		 RETURNING id, name, apk_url, package_name, created_at`,
		id, name, apkURL, packageName,
	).Scan(&a.ID, &a.Name, &a.ApkURL, &a.PackageName, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	if packageName != "" {
		_ = d.LearnApkPackage(ctx, apkURL, packageName)
	}
	return &a, nil
}

func (d *DB) DeleteApp(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM apps WHERE id = $1`, id)
	return err
}

// DevicesWithPackageInstalled returns which of deviceIDs already report the app's
// package present. The package is resolved from the learned apk_url → package map
// (seeded when an app has a package name, or learned from a prior install), so an
// install can skip devices that already have the app instead of re-downloading it.
func (d *DB) DevicesWithPackageInstalled(ctx context.Context, apkURL string, deviceIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool)
	if apkURL == "" || len(deviceIDs) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT dp.device_id
		FROM apk_packages ap
		JOIN device_packages dp ON dp.package_name = ap.package_name
		WHERE ap.apk_url = $1 AND dp.device_id = ANY($2)
	`, apkURL, deviceIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
