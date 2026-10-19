package db

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Venue icons (I1, picked 19 Oct 2026). The PNG itself is read only by the image route;
// lists and headers need just whether a venue has one and its version, which every
// page asks for, so that comes from a short cache.

// SetRestaurantIcon stores a venue's icon (already normalised), or clears it with nil.
func (d *DB) SetRestaurantIcon(ctx context.Context, id uuid.UUID, png []byte, source string) error {
	defer d.venueIcons.drop()
	if png == nil {
		_, err := d.pool.Exec(ctx, `UPDATE restaurants SET icon = NULL, icon_source = '', icon_at = NULL WHERE id = $1`, id)
		venueChanged(err)
		return err
	}
	_, err := d.pool.Exec(ctx, `UPDATE restaurants SET icon = $2, icon_source = $3, icon_at = now() WHERE id = $1`, id, png, source)
	venueChanged(err)
	return err
}

// RestaurantIcon returns a venue's icon and when it was set; nil when it has none.
func (d *DB) RestaurantIcon(ctx context.Context, id uuid.UUID) ([]byte, time.Time, string, error) {
	var b []byte
	var at *time.Time
	var src string
	err := d.pool.QueryRow(ctx, `SELECT icon, icon_at, icon_source FROM restaurants WHERE id = $1`, id).Scan(&b, &at, &src)
	if err != nil || at == nil {
		return nil, time.Time{}, "", err
	}
	return b, *at, src, nil
}

type venueIconCache struct {
	mu sync.Mutex
	at time.Time
	m  map[uuid.UUID]int64
}

func (c *venueIconCache) drop() {
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
}

// VenueIconVersions maps every venue that has an icon to its version (unix seconds of
// icon_at), from a 30-second cache that SetRestaurantIcon drops.
func (d *DB) VenueIconVersions(ctx context.Context) map[uuid.UUID]int64 {
	c := &d.venueIcons
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m != nil && time.Since(c.at) < 30*time.Second {
		return c.m
	}
	rows, err := d.pool.Query(ctx, `SELECT id, icon_at FROM restaurants WHERE icon_at IS NOT NULL`)
	if err != nil {
		return c.m
	}
	defer rows.Close()
	m := map[uuid.UUID]int64{}
	for rows.Next() {
		var id uuid.UUID
		var at time.Time
		if rows.Scan(&id, &at) == nil {
			m[id] = at.Unix()
		}
	}
	c.m, c.at = m, time.Now()
	return m
}
