package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// People and Lineup sync (16 Nov 2026; user: "people and Lineup be synced from mdm.dev to
// prod"). Before devices move to mdm.aioapp.com one by one, it gets the fleet server's
// people, their access and Lineup, through the admin API (no database access):
// ExportPeopleLineup on mdm.dev, ImportPeopleLineup on prod (api/sync_people_lineup.go).
//
// people: users, matched by id; the source wins on every column (role, names, email,
// picture, display settings, password hash). Users only the target has are kept and
// listed. A user whose username or email belongs to a different account on the target is
// skipped and listed. Sessions don't travel: everyone signs in again.
// mirrored (the target ends up exactly like the source): access_levels, access_where,
// product_names, role_apps, firmware_channels. A firmware channel naming a release the
// target lacks is skipped and listed; its rollout link is dropped (rollouts don't travel).
// venues (user: "also sync all the venues created on mdm dev. not groups, only venues not
// the devices in them"): restaurants with every setting on the row (staff PIN, updates,
// lost watch, host app, backend link, icon), their own service hours and venue lock rules;
// added or updated, never removed. Devices, groups and the backend hours copy don't travel.

type syncTable struct {
	name    string
	key     string
	mirror  bool   // delete target rows the source lacks
	where   string // only these rows travel and are compared ('' = all)
	label   string // column that names a row in reports ('' = the key)
	arbiter string // the key's partial unique index condition, when it has one
}

var peopleLineupTables = []syncTable{
	{name: "users", key: "id", label: "username"},
	{name: "access_levels", key: "id", mirror: true},
	{name: "access_where", key: "username", mirror: true},
	{name: "product_names", key: "product", mirror: true},
	{name: "role_apps", key: "role", mirror: true},
	{name: "firmware_channels", key: "model", mirror: true},
	{name: "restaurants", key: "id", label: "name"},
	{name: "service_windows", key: "restaurant_id", where: "restaurant_id IS NOT NULL AND group_id IS NULL", arbiter: "restaurant_id IS NOT NULL"},
	{name: "kiosk_policies", key: "id", where: "target_type = 'restaurant'", label: "name"},
}

// cond is the table's row filter as a WHERE clause body.
func (t syncTable) cond() string {
	if t.where == "" {
		return "TRUE"
	}
	return t.where
}

// PeopleLineupBundle is the export: each table's rows as JSON objects.
type PeopleLineupBundle struct {
	Tables map[string][]json.RawMessage `json:"tables"`
}

// SyncTableReport is what an import did (or would do) to one table.
type SyncTableReport struct {
	Added     int      `json:"added"`
	Changed   int      `json:"changed"`
	Unchanged int      `json:"unchanged"`
	Removed   int      `json:"removed"`
	Skipped   []string `json:"skipped,omitempty"`
	OnlyHere  []string `json:"only_on_target,omitempty"`
}

// ExportPeopleLineup reads the tables to sync.
func (d *DB) ExportPeopleLineup(ctx context.Context) (*PeopleLineupBundle, error) {
	b := &PeopleLineupBundle{Tables: map[string][]json.RawMessage{}}
	for _, t := range peopleLineupTables {
		rows, err := d.pool.Query(ctx, fmt.Sprintf(`SELECT row_to_json(t) FROM %s t WHERE %s ORDER BY %s`,
			pgx.Identifier{t.name}.Sanitize(), t.cond(), pgx.Identifier{t.key}.Sanitize()))
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", t.name, err)
		}
		var out []json.RawMessage
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return nil, err
			}
			if t.name == "restaurants" {
				// A venue hidden here (waiting to move) is a plain venue on the target.
				var row map[string]json.RawMessage
				if json.Unmarshal(raw, &row) == nil {
					row["hidden"] = json.RawMessage("false")
					if r2, err := json.Marshal(row); err == nil {
						raw = r2
					}
				}
			}
			out = append(out, json.RawMessage(raw))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		b.Tables[t.name] = out
	}
	return b, nil
}

// ImportPeopleLineup applies a bundle in one transaction; dryRun rolls it back and reports
// what would have happened.
func (d *DB) ImportPeopleLineup(ctx context.Context, b *PeopleLineupBundle, dryRun bool) (map[string]*SyncTableReport, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	report := map[string]*SyncTableReport{}
	for _, t := range peopleLineupTables {
		rows, ok := b.Tables[t.name]
		if !ok {
			continue // a table the source didn't send stays as it is
		}
		rep := &SyncTableReport{}
		report[t.name] = rep
		cols, err := tableColumns(ctx, tx, t.name)
		if err != nil {
			return nil, err
		}
		var set, cmpT, cmpE []string
		for _, c := range cols {
			if c == t.key {
				continue
			}
			q := pgx.Identifier{c}.Sanitize()
			set = append(set, q)
			cmpT = append(cmpT, "t."+q)
			cmpE = append(cmpE, "excluded."+q)
		}
		tbl, key := pgx.Identifier{t.name}.Sanitize(), pgx.Identifier{t.key}.Sanitize()
		exc := make([]string, len(set))
		for i, c := range set {
			exc[i] = "excluded." + c
		}
		conflict := key
		if t.arbiter != "" {
			conflict = key + ") WHERE (" + t.arbiter
		}
		upsert := fmt.Sprintf(`INSERT INTO %[1]s AS t SELECT * FROM json_populate_record(NULL::%[1]s, $1::json)
			ON CONFLICT (%[2]s) DO UPDATE SET (%[3]s) = ROW(%[4]s)
			WHERE ROW(%[5]s) IS DISTINCT FROM ROW(%[6]s)
			RETURNING (xmax = 0)`, tbl, conflict, strings.Join(set, ", "), strings.Join(exc, ", "),
			strings.Join(cmpT, ", "), strings.Join(cmpE, ", "))

		// Every incoming key first: rows only the target has are listed, or, for a mirrored
		// table, removed before anything is added, so a target row holding the same natural
		// key under another id (access_levels' seeded grants) can't block the source's.
		keys := make([]string, 0, len(rows))
		objs := make([]map[string]any, 0, len(rows))
		for _, raw := range rows {
			var obj map[string]any
			if err := json.Unmarshal(raw, &obj); err != nil {
				return nil, fmt.Errorf("%s: bad row: %w", t.name, err)
			}
			keys = append(keys, fmt.Sprint(obj[t.key]))
			objs = append(objs, obj)
		}
		label := key
		if t.label != "" {
			label = pgx.Identifier{t.label}.Sanitize()
		}
		q := fmt.Sprintf(`SELECT %[3]s::text FROM %[1]s WHERE (%[4]s) AND NOT (%[2]s::text = ANY($1)) ORDER BY 1`, tbl, key, label, t.cond())
		extra, err := tx.Query(ctx, q, keys)
		if err != nil {
			return nil, err
		}
		for extra.Next() {
			var s string
			if err := extra.Scan(&s); err != nil {
				extra.Close()
				return nil, err
			}
			rep.OnlyHere = append(rep.OnlyHere, s)
		}
		extra.Close()
		if t.mirror {
			tag, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %[1]s WHERE (%[3]s) AND NOT (%[2]s::text = ANY($1))`, tbl, key, t.cond()), keys)
			if err != nil {
				return nil, fmt.Errorf("%s remove: %w", t.name, err)
			}
			rep.Removed = int(tag.RowsAffected())
			rep.OnlyHere = nil // removed, not kept
		}

		for i, obj := range objs {
			k := keys[i]
			switch t.name {
			case "users":
				// A different account here already has this username or email: skip it.
				var other string
				err := tx.QueryRow(ctx, `SELECT username FROM users WHERE id <> $1::uuid
					AND (username = $2 OR (email IS NOT NULL AND lower(email) = lower(NULLIF($3, ''))))
					LIMIT 1`, k, fmt.Sprint(obj["username"]), strOf(obj["email"])).Scan(&other)
				if err == nil {
					rep.Skipped = append(rep.Skipped, fmt.Sprintf("%v (taken here by %s)", obj["username"], other))
					continue
				} else if err != pgx.ErrNoRows {
					return nil, err
				}
			case "firmware_channels":
				var has bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM releases WHERE id::text = $1)`, strOf(obj["release_id"])).Scan(&has); err != nil {
					return nil, err
				}
				if !has {
					rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s (release %v is not here)", k, obj["release_id"]))
					continue
				}
				obj["deployment_id"] = nil // rollouts don't travel
			}
			raw, _ := json.Marshal(obj)
			var inserted bool
			err := tx.QueryRow(ctx, upsert, string(raw)).Scan(&inserted)
			switch {
			case err == pgx.ErrNoRows:
				rep.Unchanged++
			case err != nil:
				return nil, fmt.Errorf("%s %s: %w", t.name, k, err)
			case inserted:
				rep.Added++
			default:
				rep.Changed++
			}
		}
	}
	if dryRun {
		return report, nil
	}
	return report, tx.Commit(ctx)
}

func tableColumns(ctx context.Context, tx pgx.Tx, table string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1 ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func strOf(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
