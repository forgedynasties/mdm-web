// Package aiobackend reads the AIO restaurant backend (the database behind the
// restaurant apps) through its Metabase (23 Oct 2026; the user: "this metabase key is all
// we have").
//
// The MDM uses it to link each venue to its backend restaurant (Prod or Stage, chosen per
// venue), fill the venue's profile and hours, and list the devices a restaurant has. No
// dashboard page queries it live: a sync copies the few tables into the MDM's own
// database and pages read that copy.
//
// Metabase is shared and fragile, so this client runs one small native query at a time,
// pages anything that could grow past Metabase's 2000-row cap, never asks for table
// metadata (GET /api/database/{id}/metadata took it down for 30 s on 23 Oct) and refuses
// SQL naming a secret column (push tokens, device private keys).
package aiobackend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Env is a backend environment: "prod" or "stage".
type Env string

const (
	Prod  Env = "prod"
	Stage Env = "stage"
)

// Envs lists the environments in display order.
var Envs = []Env{Prod, Stage}

// Valid reports whether e is a known environment.
func (e Env) Valid() bool { return e == Prod || e == Stage }

// Label is the environment's name for people.
func (e Env) Label() string {
	switch e {
	case Prod:
		return "Prod"
	case Stage:
		return "Stage"
	}
	return string(e)
}

// pageSize stays under Metabase's 2000-row cap on /api/dataset.
const pageSize = 1500

// Client talks to one Metabase. Safe for concurrent use; queries run one at a time.
type Client struct {
	base string
	key  string
	dbs  map[Env]int
	http *http.Client
	mu   sync.Mutex
}

// New returns a client for the Metabase at base (".../api" not included) using key, with
// Metabase database ids per environment. A nil client is returned when key is empty, and
// every method of a nil client reports ErrNotConfigured.
func New(base, key string, dbs map[Env]int) *Client {
	if key == "" {
		return nil
	}
	if base == "" {
		base = "https://metabase.dev.aioapp.com"
	}
	return &Client{
		base: strings.TrimRight(base, "/"),
		key:  key,
		dbs:  dbs,
		http: &http.Client{Timeout: 90 * time.Second},
	}
}

// ErrNotConfigured is returned when the server has no Metabase key.
var ErrNotConfigured = errors.New("AIO backend is not configured")

// secretCol matches columns this client must never read.
var secretCol = regexp.MustCompile(`(?i)\b"?(token|privatekey|endpointarn|serialnumberhash)"?\b|\*`)

type datasetResp struct {
	Status string `json:"status"`
	Error  any    `json:"error"`
	Data   struct {
		Rows [][]any `json:"rows"`
		Cols []struct {
			Name string `json:"name"`
		} `json:"cols"`
	} `json:"data"`
}

// Query runs one read-only native query against env's database and returns its rows as
// column-name maps.
func (c *Client) Query(ctx context.Context, env Env, sql string) ([]map[string]any, error) {
	if c == nil {
		return nil, ErrNotConfigured
	}
	if secretCol.MatchString(sql) {
		return nil, errors.New("aiobackend: query names a secret column or uses *")
	}
	db, ok := c.dbs[env]
	if !ok {
		return nil, fmt.Errorf("aiobackend: no database for %q", env)
	}
	body, _ := json.Marshal(map[string]any{
		"database": db,
		"type":     "native",
		"native":   map[string]any{"query": sql},
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/dataset", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("aiobackend: metabase HTTP %d", resp.StatusCode)
	}
	var r datasetResp
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("aiobackend: %w", err)
	}
	if r.Error != nil || (r.Status != "" && r.Status != "completed") {
		return nil, fmt.Errorf("aiobackend: query %s: %v", r.Status, r.Error)
	}
	out := make([]map[string]any, 0, len(r.Data.Rows))
	for _, row := range r.Data.Rows {
		m := make(map[string]any, len(row))
		for i, v := range row {
			if i < len(r.Data.Cols) {
				m[r.Data.Cols[i].Name] = v
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// queryAll pages a query by id. sql must select "id" and contain the placeholder
// {{after}} in its WHERE clause, ordered by id.
func (c *Client) queryAll(ctx context.Context, env Env, sql string) ([]map[string]any, error) {
	var out []map[string]any
	after := int64(0)
	for {
		q := strings.ReplaceAll(sql, "{{after}}", fmt.Sprint(after))
		q += fmt.Sprintf(" LIMIT %d", pageSize)
		rows, err := c.Query(ctx, env, q)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if len(rows) < pageSize {
			return out, nil
		}
		next := num(rows[len(rows)-1]["id"])
		if next <= after {
			return nil, errors.New("aiobackend: paging did not advance")
		}
		after = next
	}
}

// Restaurant is one row of public.restaurant with its business name.
type Restaurant struct {
	ID           int64
	Name         string
	BusinessID   int64
	BusinessName string
	Address      string
	City         string
	State        string
	Zip          string
	Latitude     *float64
	Longitude    *float64
	Timezone     string
	Phone        string
	Status       string
	RecordStatus string // Active / Inactive (the backend's own on/off for a restaurant)
}

// FullAddress is the address on one line ("609 1st St, Brentwood, CA 94513"). Many rows
// already hold the whole address in address; city, state and zip are added only when it
// doesn't name them.
func (r Restaurant) FullAddress() string {
	a := strings.ToLower(r.Address)
	if r.Address != "" && ((r.Zip != "" && strings.Contains(a, strings.ToLower(r.Zip))) || (r.City != "" && strings.Contains(a, strings.ToLower(r.City)))) {
		return strings.TrimSpace(r.Address)
	}
	var parts []string
	for _, p := range []string{r.Address, r.City, strings.TrimSpace(r.State + " " + r.Zip)} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ", ")
}

// Restaurants returns every restaurant in env.
func (c *Client) Restaurants(ctx context.Context, env Env) ([]Restaurant, error) {
	rows, err := c.queryAll(ctx, env, `SELECT r.id, r.name, r."businessId", b."businessName", r.address, r.city, r.state,
	       r."zipCode", r.latitude, r.longitude, r.timezone, r."phoneNo", r.status::text AS status,
	       r."recordStatus"::text AS record_status
	FROM public.restaurant r LEFT JOIN public.business b ON b.id = r."businessId"
	WHERE r.id > {{after}} ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	out := make([]Restaurant, 0, len(rows))
	for _, m := range rows {
		out = append(out, Restaurant{
			ID: num(m["id"]), Name: str(m["name"]), BusinessID: num(m["businessId"]),
			BusinessName: str(m["businessName"]), Address: str(m["address"]), City: str(m["city"]),
			State: str(m["state"]), Zip: str(m["zipCode"]), Latitude: flt(m["latitude"]),
			Longitude: flt(m["longitude"]), Timezone: str(m["timezone"]), Phone: str(m["phoneNo"]),
			Status: str(m["status"]), RecordStatus: str(m["record_status"]),
		})
	}
	return out, nil
}

// Device is one row of public.restaurant_devices (no token, key or hash columns).
type Device struct {
	ID           int64
	RestaurantID int64
	Type         string // pos, kds, kiosk, tv, nugget, printer, …
	Serial       string // hardware serial; the Nugget app's Android ID on tablets for now
	IP           string
	Onboarded    bool
	Revoked      bool
	UpdatedAt    string
}

// Devices returns every device row in env.
func (c *Client) Devices(ctx context.Context, env Env) ([]Device, error) {
	rows, err := c.queryAll(ctx, env, `SELECT id, "restaurantId", "deviceType"::text AS type, "deviceSerialNumber",
	       "ipAddress", COALESCE("isOnBoarded", false) AS onboarded, COALESCE("isRevoked", false) AS revoked,
	       "updatedAt"::text AS updated
	FROM public.restaurant_devices WHERE id > {{after}} ORDER BY id`)
	if err != nil {
		return nil, err
	}
	out := make([]Device, 0, len(rows))
	for _, m := range rows {
		out = append(out, Device{
			ID: num(m["id"]), RestaurantID: num(m["restaurantId"]), Type: str(m["type"]),
			Serial: strings.TrimSpace(str(m["deviceSerialNumber"])), IP: str(m["ipAddress"]),
			Onboarded: m["onboarded"] == true, Revoked: m["revoked"] == true, UpdatedAt: str(m["updated"]),
		})
	}
	return out, nil
}

// DayHours is one weekday's opening hours. Slots are local wall-clock "HH:MM" pairs.
type DayHours struct {
	Day   string // monday … sunday
	Open  bool
	Slots [][2]string
}

// Hours returns a restaurant's opening hours (restaurant_timing type 'restaurant' in its
// business's tenant schema), one entry per day that has a row.
func (c *Client) Hours(ctx context.Context, env Env, businessID, restaurantID int64) ([]DayHours, error) {
	if businessID <= 0 || restaurantID <= 0 {
		return nil, errors.New("aiobackend: hours need a business and restaurant id")
	}
	schema := fmt.Sprintf("tenant_%d", businessID)
	rows, err := c.Query(ctx, env, fmt.Sprintf(`SELECT t.id, t.day::text AS day, t.status::text AS status,
	       to_char(s."startTime", 'HH24:MI') AS start, to_char(s."endTime", 'HH24:MI') AS end
	FROM %s.restaurant_timing t LEFT JOIN %s.restaurant_timing_slots s ON s."restaurantTimingId" = t.id
	WHERE t."restaurantId" = %d AND t.type::text = 'restaurant'
	ORDER BY t.id, s."startTime"`, schema, schema, restaurantID))
	if err != nil {
		return nil, err
	}
	var out []DayHours
	idx := map[string]int{}
	for _, m := range rows {
		day := str(m["day"])
		i, ok := idx[day]
		if !ok {
			i = len(out)
			idx[day] = i
			out = append(out, DayHours{Day: day, Open: str(m["status"]) == "open"})
		}
		if s, e := str(m["start"]), str(m["end"]); s != "" && e != "" {
			out[i].Slots = append(out[i].Slots, [2]string{s, e})
		}
	}
	return out, nil
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

func num(v any) int64 {
	switch t := v.(type) {
	case json.Number:
		n, _ := t.Int64()
		return n
	case float64:
		return int64(t)
	}
	return 0
}

func flt(v any) *float64 {
	n, ok := v.(json.Number)
	if !ok {
		return nil
	}
	f, err := n.Float64()
	if err != nil {
		return nil
	}
	return &f
}
