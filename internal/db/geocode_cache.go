package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// geocodeCacheSchema stores reverse-geocoded addresses by ~110 m cell (19 Nov 2026). The Google
// Geocoding API is billed per request and was the whole of November's Google bill ($21 by
// the 18th): the in-memory cache was per server copy, emptied by every restart, and keyed on
// ~11 m cells that Wi-Fi position jitter kept missing. An address for a fixed point does not
// change, so it is kept here and shared by both copies.
const geocodeCacheSchema = `
CREATE TABLE IF NOT EXISTS geocode_cache (
    cell       TEXT PRIMARY KEY,
    address    TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS geocode_calls (
    day   DATE PRIMARY KEY,
    calls INT NOT NULL DEFAULT 0
);
`

// GetGeocodedAddress returns the stored address for a cell newer than maxAgeDays.
func (d *DB) GetGeocodedAddress(ctx context.Context, cell string, maxAgeDays int) (string, bool, error) {
	var addr string
	err := d.pool.QueryRow(ctx, `SELECT address FROM geocode_cache
		WHERE cell = $1 AND updated_at > now() - make_interval(days => $2)`, cell, maxAgeDays).Scan(&addr)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return addr, err == nil, err
}

// PutGeocodedAddress stores (or refreshes) a cell's address.
func (d *DB) PutGeocodedAddress(ctx context.Context, cell, address string) error {
	_, err := d.pool.Exec(ctx, `INSERT INTO geocode_cache (cell, address) VALUES ($1, $2)
		ON CONFLICT (cell) DO UPDATE SET address = EXCLUDED.address, updated_at = now()`, cell, address)
	return err
}

// CountGeocodeCall adds one paid Geocoding request to today's total (UTC) and returns it, so
// every server copy shares one daily cap.
func (d *DB) CountGeocodeCall(ctx context.Context) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `INSERT INTO geocode_calls (day, calls) VALUES ((now() AT TIME ZONE 'utc')::date, 1)
		ON CONFLICT (day) DO UPDATE SET calls = geocode_calls.calls + 1 RETURNING calls`).Scan(&n)
	return n, err
}
