package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"mdm/internal/command"
	"mdm/internal/metrics"
	prod "mdm/internal/product"
)

// ── Needs-attention dismissals ──────────────────────────────────────────────────

// ListDismissedCommandIDs returns the set of commands cleared from the
// Needs-attention list. Used by the triage classifier to skip them.
func (d *DB) ListDismissedCommandIDs(ctx context.Context) (map[uuid.UUID]bool, error) {
	rows, err := d.pool.Query(ctx, `SELECT command_id FROM dismissed_commands`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]bool)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// DismissCommands marks commands as cleared from Needs-attention (idempotent).
// It does not delete or alter the commands themselves.
func (d *DB) DismissCommands(ctx context.Context, ids []uuid.UUID, by string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO dismissed_commands (command_id, dismissed_by)
		SELECT UNNEST($1::uuid[]), $2
		ON CONFLICT (command_id) DO NOTHING`, ids, by)
	return err
}

// ── Commands ──────────────────────────────────────────────────────────────────

// DeviceCommandType maps a stored command type to what the device agent expects on
// the wire. "query" is a dashboard-only label for an admin-vetted read-only
// diagnostic; the device runs it as an ordinary shell command, so it needs no new
// type. Every place that serializes a command to a device must route through this.
func DeviceCommandType(t string) string {
	// A query and a log bundle are shell commands whose text the server wrote.
	if t == "query" || t == "collect_logs" || t == "canary" {
		return "shell"
	}
	return t
}

// CreateCommand creates a command. For target_type "devices", targetIDs are device UUIDs.
// For "groups", they are group UUIDs. For "all", targetIDs is empty.
// CreateCommand issues a command with no attributed user (system/scheduler-initiated,
// e.g. auto-reboot or a redrive tick). Prefer CreateCommandBy for anything triggered
// from the dashboard or API by a logged-in operator.
func (d *DB) CreateCommand(ctx context.Context, cmdType, apkURL string, payload json.RawMessage, targetType string, targetIDs []uuid.UUID) (*Command, error) {
	return d.CreateCommandBy(ctx, cmdType, apkURL, payload, targetType, targetIDs, "")
}

// CreateCommandBy is CreateCommand plus createdBy, a display name/email snapshotted
// at creation time so "who ran this" survives even if the user account is later
// renamed or deleted. Pass "" for system-initiated commands.
// ListCommandsByBatch returns every command created by one multi-app send
// (payload.batch), oldest first.
func (d *DB) ListCommandsByBatch(ctx context.Context, batch string) ([]Command, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, type, apk_url, payload, target_type, created_by, created_at FROM commands
		WHERE payload->>'batch' = $1 ORDER BY created_at`, batch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Command
	for rows.Next() {
		var c Command
		if err := rows.Scan(&c.ID, &c.Type, &c.ApkURL, &c.Payload, &c.TargetType, &c.CreatedBy, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) CreateCommandBy(ctx context.Context, cmdType, apkURL string, payload json.RawMessage, targetType string, targetIDs []uuid.UUID, createdBy string) (*Command, error) {
	// Every send path ends here, so this is where "wireless adb goes to one device at a
	// time" holds for all of them — the device page, bulk actions, groups and the API.
	if cmdType == "adb_tcp" {
		if msg := prod.ValidateAdbTcp(targetType, len(targetIDs), payload); msg != "" {
			return nil, errors.New(msg)
		}
	}
	// lost_mode / wifi_config: the same check the send paths ran, as a backstop, and the
	// password split off so it never reaches commands.payload.
	payload, msg := prod.NormalizeCommandPayload(cmdType, payload)
	if msg != "" {
		return nil, errors.New(msg)
	}
	payload, secret := prod.SplitCommandSecret(cmdType, payload)
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var cmd Command
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	// Policy from the registry, stamped on the row at creation: lane, guarantee, deadline
	// and attempt cap. Delivery and expiry then read columns instead of each re-deciding
	// what a type means — which is how a 'query' ended up missing from one expiry list and
	// sat pending forever. internal/command is the single table of record.
	spec := command.For(cmdType)
	var deadlineAt any
	if spec.Deadline > 0 {
		deadlineAt = time.Now().Add(spec.Deadline)
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO commands (type, apk_url, payload, target_type, created_by,
		                      lane, guarantee, deadline_at, max_attempts)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, type, apk_url, payload, target_type, created_by, created_at
	`, cmdType, apkURL, payload, targetType, createdBy,
		spec.Lane, string(spec.Guarantee), deadlineAt, spec.MaxAttempts).
		Scan(&cmd.ID, &cmd.Type, &cmd.ApkURL, &cmd.Payload, &cmd.TargetType, &cmd.CreatedBy, &cmd.CreatedAt)
	if err != nil {
		return nil, err
	}
	if secret != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO command_secrets (command_id, secret) VALUES ($1, $2)`, cmd.ID, secret); err != nil {
			return nil, err
		}
	}

	inserted := 0
	for _, tid := range targetIDs {
		// A device reporting to another MDM cannot collect a command from this one, so
		// queueing it produces a row that sits at "sent to device" until the stalled
		// sweep expires it — noise that hides the commands that are really in flight.
		// Group and restaurant targets are resolved at delivery and filtered there.
		if targetType == "devices" {
			var elsewhere bool
			if err := tx.QueryRow(ctx, `SELECT custody_server <> '' FROM devices WHERE id = $1`, tid).Scan(&elsewhere); err == nil && elsewhere {
				continue
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO command_targets (command_id, target_id) VALUES ($1, $2)
		`, cmd.ID, tid); err != nil {
			return nil, err
		}
		inserted++
	}
	// Every named device was skipped: nothing would ever collect this, so no row at all
	// (the rollback in defer drops the command), rather than one that reads "0 devices".
	if targetType == "devices" && len(targetIDs) > 0 && inserted == 0 {
		return nil, errors.New("every target reports to another MDM server, nothing to send")
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	metrics.Default.Emit("command", "warn", cmdType+" → "+targetType+" ("+strconv.Itoa(len(targetIDs))+")")
	// First line of this command's history, written after the commit so the log never
	// mentions a command that was rolled back.
	d.AppendCommandEvent(ctx, cmd.ID, uuid.Nil, "created", map[string]any{
		"type": cmdType, "lane": spec.Lane, "guarantee": string(spec.Guarantee),
		"target_type": targetType, "targets": len(targetIDs), "by": createdBy,
	})
	return &cmd, nil
}

func (d *DB) DeleteCommand(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM commands WHERE id = $1`, id)
	return err
}

func (d *DB) ListCommands(ctx context.Context) ([]Command, error) {
	return d.ListCommandsSince(ctx, 0)
}

// ListCommandsSince returns commands newest-first. sinceDays > 0 limits to the
// recent window (the Actions page uses this — its triage buckets only need recent
// commands, and full history has its own paginated view); 0 returns all.
func (d *DB) ListCommandsSince(ctx context.Context, sinceDays int) ([]Command, error) {
	// update_splash (boot logo) is managed on its own config page (/boot-logo) and
	// deliberately excluded from the Actions and history lists — it's a fleet config
	// action, not a tracked one-off command.
	// A shell with no author is the legacy OTA progress probe (legacy_watch.go): not
	// an action, kept out of every history.
	q := `SELECT id, type, apk_url, payload, target_type, created_by, created_at FROM commands WHERE type != 'update_splash' AND type != 'canary' AND NOT (type = 'shell' AND created_by = '')`
	if sinceDays > 0 {
		q += fmt.Sprintf(" AND created_at >= NOW() - INTERVAL '%d days'", sinceDays)
		// The day window alone doesn't bound the row count — a busy fleet issuing
		// many commands/day can still return a large result set within 30 days. Cap
		// it as a backstop; the Actions page's triage buckets never need more than
		// this. sinceDays == 0 is CommandHistory's real unbounded, paginated view —
		// capping that too would silently truncate it once a fleet passes 2000
		// total commands, so the cap only applies to the windowed call.
		q += " ORDER BY created_at DESC LIMIT 2000"
	} else {
		q += " ORDER BY created_at DESC"
	}
	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cmds []Command
	for rows.Next() {
		var c Command
		if err := rows.Scan(&c.ID, &c.Type, &c.ApkURL, &c.Payload, &c.TargetType, &c.CreatedBy, &c.CreatedAt); err != nil {
			return nil, err
		}
		cmds = append(cmds, c)
	}
	return cmds, rows.Err()
}

// GetLatestCommandByType returns the most recent command of the given type, or
// (nil, nil) if none exists. Used by the Boot logo config page to show the status
// of the last applied splash without surfacing it in the command history lists.
func (d *DB) GetLatestCommandByType(ctx context.Context, cmdType string) (*Command, error) {
	var c Command
	err := d.pool.QueryRow(ctx, `
		SELECT id, type, apk_url, payload, target_type, created_at
		FROM commands WHERE type = $1 ORDER BY created_at DESC LIMIT 1
	`, cmdType).Scan(&c.ID, &c.Type, &c.ApkURL, &c.Payload, &c.TargetType, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ShellCommandSuggestions returns previously-sent shell commands for quick re-use in the
// builder: the most recently used (distinct, newest first) and the most frequently used.
func (d *DB) ShellCommandSuggestions(ctx context.Context, limit int) (recent, popular []string, err error) {
	if limit <= 0 {
		limit = 6
	}
	collect := func(q string) ([]string, error) {
		rows, err := d.pool.Query(ctx, q, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, rows.Err()
	}
	recent, err = collect(`
		SELECT cmd FROM (
			SELECT payload->>'cmd' AS cmd, MAX(created_at) AS last
			FROM commands
			WHERE type = 'shell' AND COALESCE(payload->>'cmd','') <> ''
			GROUP BY payload->>'cmd'
			ORDER BY last DESC
			LIMIT $1
		) t`)
	if err != nil {
		return nil, nil, err
	}
	popular, err = collect(`
		SELECT payload->>'cmd' AS cmd
		FROM commands
		WHERE type = 'shell' AND COALESCE(payload->>'cmd','') <> ''
		GROUP BY payload->>'cmd'
		ORDER BY COUNT(*) DESC, MAX(created_at) DESC
		LIMIT $1`)
	if err != nil {
		return nil, nil, err
	}
	return recent, popular, nil
}

func (d *DB) GetCommand(ctx context.Context, id uuid.UUID) (*Command, error) {
	var c Command
	err := d.pool.QueryRow(ctx, `
		SELECT id, type, apk_url, payload, target_type, created_by, created_at
		FROM commands WHERE id = $1
	`, id).Scan(&c.ID, &c.Type, &c.ApkURL, &c.Payload, &c.TargetType, &c.CreatedBy, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

type CommandDeliverySummary struct {
	CommandID uuid.UUID
	Pending   int
	Delivered int
	Completed int
	Failed    int
	// Downloading/Installing are a phase breakdown of the in-flight (Delivered)
	// devices for install_apk commands, so the Actions in-progress row can show
	// "3 downloading · 1 installing" instead of a single lumped count. They are a
	// subset of Delivered, not additive to the totals.
	Downloading int
	Installing  int
}

// GetCommandDeliverySummaries returns per-command status rollups for the history
// list. It aggregates the real per-device status table (command_status) — the
// same source GetCommandDeliveries reads — and applies the same expiry rule, so a
// device stuck at pending/delivered past the timeout counts as expired rather than
// perpetually "in flight" (otherwise a day-old command still animates as live).
// (A device with no status row yet isn't counted here; the command detail page is
// the authoritative per-device view including those.)
// GetCommandDeliverySummaries rolls up per-command delivery status. sinceDays > 0
// scopes the (otherwise whole-table) scan to commands created within that window
// — the Actions page passes a window since its triage buckets only need recent
// commands; the full history view passes 0.
const cmdSummaryCacheTTL = 5 * time.Second

func (d *DB) GetCommandDeliverySummaries(ctx context.Context, expirySec, sinceDays int) (map[uuid.UUID]CommandDeliverySummary, error) {
	if expirySec <= 0 {
		expirySec = 300
	}
	installExpiry := expirySec * 3
	if installExpiry < 900 {
		installExpiry = 900
	}

	key := [2]int{expirySec, sinceDays}
	d.cmdSummaryMu.Lock()
	if e, ok := d.cmdSummaryCache[key]; ok && time.Since(e.at) < cmdSummaryCacheTTL {
		d.cmdSummaryMu.Unlock()
		return e.data, nil
	}
	d.cmdSummaryMu.Unlock()
	sinceClause := ""
	if sinceDays > 0 {
		sinceClause = fmt.Sprintf("WHERE c.created_at >= NOW() - INTERVAL '%d days'", sinceDays)
	}
	rows, err := d.pool.Query(ctx, fmt.Sprintf(`
		SELECT command_id, status, COUNT(*) FROM (
			SELECT cs.command_id,
				CASE
					WHEN c.type IN `+shortLivedCommandsSQL+`
						AND cs.status IN ('pending','delivered')
						AND c.created_at <= NOW() - INTERVAL '%d seconds'
					THEN 'expired'
					WHEN c.type IN `+prod.InstallShapedSQL()+`
						AND cs.status IN ('pending','delivered')
						AND c.created_at <= NOW() - INTERVAL '%d seconds'
					THEN 'expired'
					ELSE cs.status
				END AS status
			FROM command_status cs
			JOIN commands c ON c.id = cs.command_id
			%s
		) t
		GROUP BY command_id, status
	`, expirySec, installExpiry, sinceClause))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]CommandDeliverySummary)
	for rows.Next() {
		var cid uuid.UUID
		var status string
		var count int
		if err := rows.Scan(&cid, &status, &count); err != nil {
			return nil, err
		}
		s := out[cid]
		s.CommandID = cid
		switch status {
		case "pending":
			s.Pending += count
		// An install actively downloading/installing is in flight: count it as
		// delivered so the command stays in the Actions "In progress" bucket
		// (commandBucket keys "prog" off Pending/Delivered) instead of falling
		// through to "done" and disappearing while the app is still installing.
		// Downloading/Installing are also tracked separately (a subset of
		// Delivered) so the row can show a per-phase tally.
		case "delivered":
			s.Delivered += count
		case "downloading":
			s.Delivered += count
			s.Downloading += count
		case "installing":
			s.Delivered += count
			s.Installing += count
		case "installed", "completed":
			s.Completed += count
		case "failed", "expired":
			s.Failed += count
		}
		out[cid] = s
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	d.cmdSummaryMu.Lock()
	if d.cmdSummaryCache == nil {
		d.cmdSummaryCache = make(map[[2]int]cmdSummaryCacheEntry)
	}
	d.cmdSummaryCache[key] = cmdSummaryCacheEntry{at: time.Now(), data: out}
	d.cmdSummaryMu.Unlock()
	return out, nil
}

// GetCommandTargetSerialsBatch resolves target serials for many commands in one
// query, replacing the per-command N+1 on the Actions and history pages.
func (d *DB) GetCommandTargetSerialsBatch(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]string, error) {
	out := make(map[uuid.UUID][]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT ct.command_id, d.serial_number
		FROM command_targets ct
		JOIN devices d ON d.id = ct.target_id
		WHERE ct.command_id = ANY($1)
		ORDER BY d.serial_number
	`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid uuid.UUID
		var s string
		if err := rows.Scan(&cid, &s); err != nil {
			return nil, err
		}
		out[cid] = append(out[cid], s)
	}
	return out, rows.Err()
}

func (d *DB) GetCommandTargetSerials(ctx context.Context, commandID uuid.UUID) ([]string, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.serial_number FROM devices d
		JOIN command_targets ct ON ct.target_id = d.id
		WHERE ct.command_id = $1
	`, commandID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetCommandTargetIDs returns the target UUIDs stored in command_targets for a command.
func (d *DB) GetCommandTargetIDs(ctx context.Context, commandID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := d.pool.Query(ctx, `SELECT target_id FROM command_targets WHERE command_id = $1`, commandID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetPendingCommandsForDevice returns commands not yet delivered/acked for this device.
// GetCommandDeviceIDs resolves the device IDs a command reached: direct device
// targets, group expansion, and any device that already has a status row. Used to
// push a cancel to exactly those devices when the command is deleted, so a device
// mid-download aborts instead of finishing an install the operator just removed.
func (d *DB) GetCommandDeviceIDs(ctx context.Context, commandID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT ct.target_id FROM command_targets ct JOIN commands c ON c.id = ct.command_id
		  WHERE ct.command_id = $1 AND c.target_type = 'devices'
		UNION
		SELECT dg.device_id FROM command_targets ct JOIN commands c ON c.id = ct.command_id
		  JOIN device_groups dg ON dg.group_id = ct.target_id
		  WHERE ct.command_id = $1 AND c.target_type = 'groups'
		UNION
		SELECT cs.device_id FROM command_status cs WHERE cs.command_id = $1`, commandID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (d *DB) GetPendingCommandsForDevice(ctx context.Context, deviceID uuid.UUID) ([]Command, error) {
	rows, err := d.pool.Query(ctx, `
		-- live = this device's commands that are still CURRENT: created within the hour, or
		-- actively downloading/installing on this device at any age. A command older than an
		-- hour is stale and must NOT suddenly run when a device reconnects (a day-old queued
		-- reboot firing on reconnect is exactly the surprise-reboot we must avoid). This
		-- matches the Queue tab's display window, so what you see queued is what will run.
		--
		-- The same set is both what may be delivered and what may block (below), so it is
		-- built once, from the two small indexed sources. Filtering the whole commands table
		-- row by row instead ran a command_status probe per command ever created: 740 ms for
		-- zero rows on live, on every connect, ack and minute flush — a CPU core, fleet-wide.
		WITH live AS MATERIALIZED (
			-- max_attempts rides along because the delivery filter below reads it; a CTE
			-- column that is selected but unused costs nothing, a missing one is a runtime
			-- error on every delivery path at once.
			SELECT c.id, c.type, c.apk_url, c.payload, c.target_type, c.created_at, c.max_attempts, c.lane
			FROM commands c
			WHERE c.id IN (
				SELECT id FROM commands WHERE created_at > NOW() - INTERVAL '1 hour'
				UNION
				SELECT command_id FROM command_status
				WHERE device_id = $1 AND status IN ('downloading', 'installing')
			)
			AND (
				c.target_type = 'all'
				OR (c.target_type = 'devices' AND EXISTS (
					SELECT 1 FROM command_targets ct
					WHERE ct.command_id = c.id AND ct.target_id = $1
				))
				OR (c.target_type = 'groups' AND EXISTS (
					SELECT 1 FROM command_targets ct
					JOIN device_groups dg ON dg.group_id = ct.target_id
					WHERE ct.command_id = c.id AND dg.device_id = $1
				))
			)
		)
		SELECT c.id, c.type, c.apk_url, c.payload, c.target_type, c.created_at
		FROM live c
		WHERE NOT EXISTS (
			SELECT 1 FROM command_status cs
			WHERE cs.command_id = c.id AND cs.device_id = $1
			AND (
				cs.status IN ('downloading', 'installing', 'installed', 'failed', 'completed', 'cancelled', 'expired')
				-- A 'delivered' command means only that hub.Push enqueued the frame onto the
				-- socket — NOT that the device received it. A half-open socket (Wi-Fi dropped
				-- with no FIN) swallows the frame, so ANY command type can wedge at 'delivered'
				-- and never run. This query runs on a (re)connect flush / check-in, and a fresh
				-- connection is proof the PRIOR delivery is dead — so re-deliver immediately,
				-- with no staleness grace (that ~90s wait is what made a reconnect take minutes
				-- to actually run queued commands). received_at set = the device CONFIRMED
				-- receipt, so it has the command — never re-deliver (stops dup + spam); the
				-- client also dedups by id, so a re-push during an in-flight ack can't double-
				-- run. Reboot is exempt: it completes via CompleteDeliveredReboots, not a re-push.
				-- (The periodic RedriveStuckDeliveries keeps its own staleness window for a
				-- device that stays continuously connected.)
				OR (cs.status = 'delivered'
					AND (cs.received_at IS NOT NULL OR c.type = 'reboot'))
				-- Someone already holds this command and their time has not run out. A WS
				-- push takes a short lease (command.PushLease) because a push is
				-- unconfirmed; an HTTP hand-out takes the type's full lease because its 200
				-- confirms it. Either way the lease expiring is what returns the command to
				-- the queue — no sweep needed. A reconnecting device releases its own leases
				-- (ReleaseDeviceLeases), because a fresh socket proves the old delivery dead.
				OR (cs.lease_expires_at IS NOT NULL AND cs.lease_expires_at > NOW())
				-- Attempts exhausted: stop pretending. 0 = unlimited (the default).
				OR (c.max_attempts > 0 AND cs.attempts >= c.max_attempts)
			)
		)
		-- Serialize the queue per device: deliver a command only if there is NO OTHER
		-- non-terminal command for this device created BEFORE it. So at any moment only the
		-- single oldest unfinished command is eligible — commands run strictly one at a time,
		-- FIFO, whether the device is online or offline. When the current one reaches a
		-- terminal state the next becomes eligible (the ack path flushes it immediately).
		-- Real-time control frames (remote-control capture, checkin_now, etc.) are sent
		-- directly over WS, not as command rows, so they are unaffected.
		--
		-- OTA is exempt from this gate entirely (both directions: c.type='ota' skips being
		-- gated, and c2.type<>'ota' below means an OTA never gates anything else) — a
		-- download+install can run minutes long, and blocking every other command on this
		-- device behind it for that whole time (e.g. a screenshot or reboot request just
		-- sitting there) is worse than the two racing. The client already serializes actually
		-- applying an OTA around other work on its own.
		AND (
			-- OTA and the daily canary are exempt from the per-device gate in BOTH
			-- directions (see the c2 filter below). For OTA because a download runs for
			-- minutes and blocking everything behind it is worse than the two racing; for
			-- the canary because it exists only to measure whether this device can be
			-- commanded, and a measurement that delays an operator's command — on exactly
			-- the devices we are worried about — would be worse than not measuring.
			c.type IN ('ota', 'canary')
			-- A blocker must itself be in live: a command past its delivery window (older than an
			-- hour and not actively downloading/installing) never delivers, so it must not wedge
			-- the queue behind it — otherwise stale pending commands (e.g. offline-device
			-- screenshots that pile up) block every newer command forever.
			OR NOT EXISTS (
				SELECT 1 FROM live c2
				-- Same lane only. One queue per device meant a wedged install held up a
				-- screenshot, a reboot and everything else for the rest of the hour; a
				-- lane ("install", "shell", "firmware", "device") serializes work against
				-- its own kind, which is the actual reason the gate exists. Rows created
				-- before lanes existed default to 'default', so they serialize together
				-- exactly as they did before.
				WHERE c2.id <> c.id AND c2.created_at < c.created_at AND c2.lane = c.lane
				  AND c2.type NOT IN ('ota', 'canary')
				  AND NOT EXISTS (SELECT 1 FROM command_status s2 WHERE s2.command_id = c2.id AND s2.device_id = $1
					AND s2.status IN ('installed','failed','completed','cancelled','expired'))
			)
		)
		ORDER BY c.created_at ASC
	`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cmds []Command
	for rows.Next() {
		var c Command
		if err := rows.Scan(&c.ID, &c.Type, &c.ApkURL, &c.Payload, &c.TargetType, &c.CreatedAt); err != nil {
			return nil, err
		}
		cmds = append(cmds, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// This is a delivery read — every caller hands these to the device — so the secret
	// fields go back in here.
	for i := range cmds {
		cmds[i].Payload = d.DeliveryPayload(ctx, cmds[i].ID, cmds[i].Type, cmds[i].Payload)
	}
	return cmds, nil
}

// DeliveryPayload is a command's payload as the device must receive it: the stored
// payload with its secret fields (command_secrets) joined back in. Call it only where
// the payload is about to go to a device — never for a view. Types without secrets cost
// nothing; a secret that is gone (swept) leaves the stored payload, which the client
// refuses rather than guessing.
func (d *DB) DeliveryPayload(ctx context.Context, id uuid.UUID, cmdType string, payload json.RawMessage) json.RawMessage {
	if !prod.CommandHasSecret(cmdType) {
		return payload
	}
	var secret json.RawMessage
	if err := d.pool.QueryRow(ctx, `SELECT secret FROM command_secrets WHERE command_id = $1`, id).Scan(&secret); err != nil {
		return payload
	}
	return prod.MergeCommandSecret(cmdType, payload, secret)
}

// dropFinishedSecret deletes a command's secret once every device it names has reached
// a terminal state: nobody can be handed it again. Group and fleet commands resolve
// their devices at delivery, so theirs wait for PruneCommandSecrets.
func (d *DB) dropFinishedSecret(ctx context.Context, commandID uuid.UUID) {
	_, err := d.pool.Exec(ctx, `
		DELETE FROM command_secrets s
		USING commands c
		WHERE s.command_id = $1 AND c.id = s.command_id AND c.target_type = 'devices'
		  AND NOT EXISTS (
			SELECT 1 FROM command_targets ct
			WHERE ct.command_id = c.id AND NOT EXISTS (
				SELECT 1 FROM command_status cs
				WHERE cs.command_id = c.id AND cs.device_id = ct.target_id
				  AND cs.status IN ('installed', 'failed', 'completed', 'cancelled', 'expired')))`, commandID)
	if err != nil {
		log.Printf("[command-secret] drop %s: %v", commandID, err)
	}
}

// PruneCommandSecrets deletes the secrets of commands past delivering: a command is only
// handed out within the hour it was created (GetPendingCommandsForDevice), so two hours
// leaves every lease and redrive room to finish.
func (d *DB) PruneCommandSecrets(ctx context.Context) (int64, error) {
	tag, err := d.pool.Exec(ctx, `DELETE FROM command_secrets WHERE created_at < NOW() - INTERVAL '2 hours'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// LearnApkPackage records that apkURL installs packageName, so future installs of
// that APK can be reconciled against a device's reported package list.
func (d *DB) LearnApkPackage(ctx context.Context, apkURL, packageName string) error {
	if apkURL == "" || packageName == "" {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO apk_packages (apk_url, package_name, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (apk_url) DO UPDATE SET package_name = EXCLUDED.package_name, updated_at = NOW()
	`, apkURL, packageName)
	return err
}

// ReconcileInstalledCommands marks as 'installed' any in-flight install_apk command
// targeting this device whose APK's known package is already present in the device's
// reported packages. Recovers from a lost terminal ack: the "installing" row clears
// as soon as the device reports the app present. Returns the affected command IDs so
// the caller can publish live updates.
func (d *DB) ReconcileInstalledCommands(ctx context.Context, deviceID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := d.pool.Query(ctx, `
		WITH affected AS (
			SELECT c.id
			FROM commands c
			-- Resolve the command's package from EITHER the learned apk_url->package map
			-- (apk_packages) OR the app-library row (apps.package_name, known at upload
			-- time). The library path matters for S3/library apps whose apk_packages entry
			-- may not be learned yet — without it a genuinely-installed library app (e.g.
			-- an install whose terminal ack was lost on a half-open WS) never reconciles.
			JOIN device_packages dp ON dp.device_id = $1 AND dp.package_name IN (
				SELECT ap.package_name FROM apk_packages ap WHERE ap.apk_url = c.apk_url
				UNION
				SELECT a.package_name FROM apps a WHERE a.apk_url = c.apk_url AND COALESCE(a.package_name,'') <> ''
			)
			-- install_apk only, deliberately — NOT prod.InstallShapedSQL(): this reconciles
			-- a stalled install by finding the package present on the device, and the
			-- agent's package is the agent itself, always present. Including app_update
			-- would settle every stalled self-update as 'installed', including the ones
			-- where the version never changed. Expiry covers app_update instead.
			WHERE c.type = 'install_apk'
			  AND (
				c.target_type = 'all'
				OR (c.target_type = 'devices' AND EXISTS (
					SELECT 1 FROM command_targets ct WHERE ct.command_id = c.id AND ct.target_id = $1))
				OR (c.target_type = 'groups' AND EXISTS (
					SELECT 1 FROM command_targets ct JOIN device_groups dg ON dg.group_id = ct.target_id
					WHERE ct.command_id = c.id AND dg.device_id = $1))
			  )
			  -- Terminal-correct states ('installed','completed') are left alone.
			  AND NOT EXISTS (
				SELECT 1 FROM command_status cs WHERE cs.command_id = c.id AND cs.device_id = $1
				  AND cs.status IN ('installed','completed'))
			  -- Only recover a LOST-ACK install (the stalled-install sweep failed it, but the
			  -- device never reported anything). A command_results row means the DEVICE
			  -- reported a real outcome — most importantly a genuine failure (downgrade,
			  -- signature mismatch, corrupt APK). Never flip such a device-reported failure
			  -- to 'installed' just because the device still has an OLDER version of the
			  -- package present; that would be a false success. Sweep-failed installs have no
			  -- result row, so they still reconcile.
			  AND NOT EXISTS (
				SELECT 1 FROM command_results cr WHERE cr.command_id = c.id AND cr.device_id = $1)
		), up AS (
			INSERT INTO command_status (command_id, device_id, status, progress, updated_at)
			SELECT id, $1, 'installed', NULL, NOW() FROM affected
			ON CONFLICT (command_id, device_id) DO UPDATE SET status = 'installed', progress = NULL, updated_at = NOW()
			RETURNING command_id
		)
		SELECT command_id FROM up
	`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetApkPackageMap returns the learned apk_url → package_name map for the given APK
// URLs (empty urls => whole table). Used to filter already-installed apps out of the
// pending-install rows at render time.
func (d *DB) GetApkPackageMap(ctx context.Context, apkURLs []string) (map[string]string, error) {
	out := make(map[string]string)
	var rows pgx.Rows
	var err error
	if len(apkURLs) == 0 {
		rows, err = d.pool.Query(ctx, `SELECT apk_url, package_name FROM apk_packages`)
	} else {
		rows, err = d.pool.Query(ctx, `SELECT apk_url, package_name FROM apk_packages WHERE apk_url = ANY($1)`, apkURLs)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u, p string
		if err := rows.Scan(&u, &p); err != nil {
			return nil, err
		}
		out[u] = p
	}
	return out, rows.Err()
}

type DeviceCommand struct {
	ID         uuid.UUID       `json:"id"`
	Type       string          `json:"type"`
	ApkURL     string          `json:"apk_url"`
	Payload    json.RawMessage `json:"payload"`
	TargetType string          `json:"target_type"`
	CreatedBy  string          `json:"created_by"`
	CreatedAt  time.Time       `json:"created_at"`
	Status     string          `json:"status"`
	UpdatedAt  time.Time       `json:"updated_at"`
	Output     string          `json:"output"`
	Progress   *int            `json:"progress,omitempty"` // 0-100 while downloading, nil otherwise
}

// GetDeviceCommands returns all commands targeting a device with their status.
func (d *DB) GetDeviceCommands(ctx context.Context, deviceID uuid.UUID, expirySec int) ([]DeviceCommand, error) {
	if expirySec <= 0 {
		expirySec = 300
	}
	// install_apk gets a longer leash than shell/screenshot/reboot (a large APK can
	// take minutes to download + install), but a delivered/pending install that
	// hasn't acked within this window is dead — expire it so the device page stops
	// showing it as "installing" forever and duplicate clicks don't pile up.
	installExpiry := expirySec * 3
	if installExpiry < 900 {
		installExpiry = 900
	}
	rows, err := d.pool.Query(ctx, fmt.Sprintf(`
		SELECT c.id, c.type, c.apk_url, c.payload, c.target_type, c.created_by, c.created_at,
		       CASE
		         -- Expire an install by LAST ACTIVITY (updated_at), not creation time, so a
		         -- large APK that legitimately takes a while to download isn't shown 'expired'
		         -- while it's actively progressing. This matches ExpireStalledInstalls, which
		         -- keys the actual write on updated_at (the device page previously disagreed
		         -- with the sweep and could prompt an operator to cancel a healthy install).
		         WHEN c.type IN `+prod.InstallShapedSQL()+`
		              AND COALESCE(cs.status, 'pending') IN ('pending', 'delivered', 'downloading', 'installing')
		              AND COALESCE(cs.updated_at, c.created_at) <= NOW() - INTERVAL '%d seconds' THEN 'expired'
		         WHEN c.type IN ('query', 'mic_gain_read', 'mic_gain_set')
		              AND COALESCE(cs.status, 'pending') IN ('pending', 'delivered')
		              AND c.created_at <= NOW() - INTERVAL '%d seconds' THEN 'expired'
		         WHEN cs.status IS NOT NULL THEN cs.status
		         WHEN c.type IN `+shortLivedCommandsSQL+`
		              AND c.created_at <= NOW() - INTERVAL '%d seconds' THEN 'expired'
		         ELSE 'pending'
		       END AS status,
		       COALESCE(cs.updated_at, c.created_at) AS updated_at,
		       -- A log bundle is up to a megabyte; lists carry its size, not its text.
		       CASE WHEN c.type = 'collect_logs' THEN '' ELSE COALESCE(cr.output, '') END AS output,
		       cs.progress
		FROM commands c
		LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = $1
		LEFT JOIN command_results cr ON cr.command_id = c.id AND cr.device_id = $1
		WHERE (
			c.target_type = 'all'
			OR (c.target_type = 'devices' AND EXISTS (
				SELECT 1 FROM command_targets ct WHERE ct.command_id = c.id AND ct.target_id = $1
			))
			OR (c.target_type = 'groups' AND EXISTS (
				SELECT 1 FROM command_targets ct
				JOIN device_groups dg ON dg.group_id = ct.target_id
				WHERE ct.command_id = c.id AND dg.device_id = $1
			))
		)
		-- Boot logo is a config action managed on /boot-logo, not part of the
		-- device's command history; an authorless shell is the OTA progress probe.
		AND c.type != 'update_splash'
		AND NOT (c.type = 'shell' AND c.created_by = '')
		ORDER BY c.created_at DESC
	`, installExpiry, expirySec, expirySec), deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DeviceCommand
	for rows.Next() {
		var dc DeviceCommand
		if err := rows.Scan(&dc.ID, &dc.Type, &dc.ApkURL, &dc.Payload, &dc.TargetType, &dc.CreatedBy, &dc.CreatedAt, &dc.Status, &dc.UpdatedAt, &dc.Output, &dc.Progress); err != nil {
			return nil, err
		}
		out = append(out, dc)
	}
	return out, rows.Err()
}

// GetDeviceQueue returns a device's QUEUE — its non-terminal commands, oldest-first
// (FIFO). Raw status with no display-expiry (the queue does not expire): 'pending' (not
// yet sent), 'delivered' (sent, awaiting the device), 'downloading', 'installing'.
// update_splash (boot logo) is excluded, matching GetDeviceCommands.
func (d *DB) GetDeviceQueue(ctx context.Context, deviceID uuid.UUID) ([]DeviceCommand, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT c.id, c.type, c.apk_url, c.payload, c.target_type, c.created_by, c.created_at,
		       COALESCE(cs.status, 'pending') AS status,
		       COALESCE(cs.updated_at, c.created_at) AS updated_at,
		       '' AS output, cs.progress
		FROM commands c
		LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = $1
		WHERE (
			c.target_type = 'all'
			OR (c.target_type = 'devices' AND EXISTS (
				SELECT 1 FROM command_targets ct WHERE ct.command_id = c.id AND ct.target_id = $1))
			OR (c.target_type = 'groups' AND EXISTS (
				SELECT 1 FROM command_targets ct JOIN device_groups dg ON dg.group_id = ct.target_id
				WHERE ct.command_id = c.id AND dg.device_id = $1))
		)
		AND c.type != 'update_splash'
		AND (
			COALESCE(cs.status, 'pending') IN ('pending', 'delivered', 'downloading', 'installing')
			-- A command that just reached a terminal state lingers here for 5 minutes
			-- instead of vanishing the instant it finishes — so the operator actually
			-- sees "Done"/"Failed" rather than the row disappearing out from under them.
			OR (COALESCE(cs.status, 'pending') IN ('installed', 'completed', 'failed', 'cancelled')
				AND cs.updated_at > NOW() - INTERVAL '5 minutes')
		)
		-- Only show commands that will actually run now — the freshly queued ones. Old stuck
		-- commands (a long-ago delivered/pending that the device never acted on) are not shown
		-- as "queued". An actively downloading/installing command always shows, whatever its
		-- age, since it IS running right now.
		AND (
			c.created_at > NOW() - INTERVAL '1 hour'
			OR COALESCE(cs.status, 'pending') IN ('downloading', 'installing', 'installed', 'completed', 'failed', 'cancelled')
		)
		ORDER BY c.created_at ASC
	`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceCommand
	for rows.Next() {
		var dc DeviceCommand
		if err := rows.Scan(&dc.ID, &dc.Type, &dc.ApkURL, &dc.Payload, &dc.TargetType, &dc.CreatedBy, &dc.CreatedAt, &dc.Status, &dc.UpdatedAt, &dc.Output, &dc.Progress); err != nil {
			return nil, err
		}
		out = append(out, dc)
	}
	return out, rows.Err()
}

// GetCommandDeliveries returns per-device status for a command.
// Shell/screenshot/reboot deliveries that are still 'delivered' or 'pending'
// after 5 minutes from command creation are reported as 'expired'.
func (d *DB) GetCommandDeliveries(ctx context.Context, commandID uuid.UUID, expirySec int) ([]CommandDelivery, error) {
	if expirySec <= 0 {
		expirySec = 300
	}
	installExpiry := expirySec * 3
	if installExpiry < 900 {
		installExpiry = 900
	}
	// Build the full set of targeted devices — explicit device targets and the members
	// of any targeted groups — then LEFT JOIN command_status so devices that haven't
	// received the command yet (offline / not checked in) still show as 'pending'.
	// "all" broadcasts aren't enumerable, so for those we fall back to status rows.
	rows, err := d.pool.Query(ctx, fmt.Sprintf(`
		WITH target_devices AS (
			SELECT ct.target_id AS device_id
			FROM command_targets ct
			JOIN commands c ON c.id = ct.command_id
			WHERE ct.command_id = $1 AND c.target_type = 'devices'
			UNION
			SELECT dg.device_id
			FROM command_targets ct
			JOIN commands c ON c.id = ct.command_id
			JOIN device_groups dg ON dg.group_id = ct.target_id
			WHERE ct.command_id = $1 AND c.target_type = 'groups'
			UNION
			SELECT cs.device_id FROM command_status cs WHERE cs.command_id = $1
		)
		SELECT td.device_id,
		       d.serial_number,
		       CASE
		         WHEN c.type IN `+shortLivedCommandsSQL+`
		              AND COALESCE(cs.status, 'pending') IN ('pending', 'delivered')
		              AND c.created_at <= NOW() - INTERVAL '%d seconds'
		           THEN 'expired'
		         WHEN c.type IN `+prod.InstallShapedSQL()+`
		              AND COALESCE(cs.status, 'pending') IN ('pending', 'delivered')
		              AND c.created_at <= NOW() - INTERVAL '%d seconds'
		           THEN 'expired'
		         ELSE COALESCE(cs.status, 'pending')
		       END AS status,
		       cs.progress,
		       COALESCE(cs.updated_at, c.created_at) AS updated_at,
		       CASE WHEN c.type = 'collect_logs' THEN '' ELSE COALESCE(cr.output, '') END AS output,
		       COALESCE(octet_length(cr.output), 0) AS output_bytes,
		       d.last_seen_at
		FROM target_devices td
		JOIN devices d ON d.id = td.device_id
		JOIN commands c ON c.id = $1
		LEFT JOIN command_status cs ON cs.command_id = $1 AND cs.device_id = td.device_id
		LEFT JOIN command_results cr ON cr.command_id = $1 AND cr.device_id = td.device_id
		ORDER BY COALESCE(cs.updated_at, c.created_at) DESC, d.serial_number
	`, expirySec, installExpiry), commandID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CommandDelivery
	for rows.Next() {
		var cd CommandDelivery
		if err := rows.Scan(&cd.DeviceID, &cd.SerialNumber, &cd.Status, &cd.Progress, &cd.UpdatedAt, &cd.Output, &cd.OutputBytes, &cd.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, cd)
	}
	return out, rows.Err()
}

type CommandResult struct {
	ID        uuid.UUID `json:"id"`
	CommandID uuid.UUID `json:"command_id"`
	DeviceID  uuid.UUID `json:"device_id"`
	Output    string    `json:"output"`
	CreatedAt time.Time `json:"created_at"`
}

// MergeLatestExtra shallow-merges patch into devices.latest_extra (JSONB ||) without
// touching build_id / battery / last_seen. Used when a command result carries a fresher
// reading of a telemetry field than the last check-in (e.g. mic_gain_read).
func (d *DB) MergeLatestExtra(ctx context.Context, deviceID uuid.UUID, patch json.RawMessage) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE devices SET latest_extra = COALESCE(latest_extra, '{}'::jsonb) || $2::jsonb
		WHERE id = $1
	`, deviceID, patch)
	return err
}

func (d *DB) SaveCommandResult(ctx context.Context, commandID, deviceID uuid.UUID, output string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO command_results (command_id, device_id, output)
		VALUES ($1, $2, $3)
		ON CONFLICT (command_id, device_id) DO UPDATE SET output = EXCLUDED.output
	`, commandID, deviceID, output)
	return err
}

// AgentUpdateGiveUps names the devices the auto-update job should stop offering a build
// to: those that refused it for its signing key (a different key never installs, however
// often it is sent) and those it has failed on autoUpdateMaxFails times. Keyed to the
// build's SHA-256, so publishing a new build gives every device a fresh start.
func (d *DB) AgentUpdateGiveUps(ctx context.Context, deviceIDs []uuid.UUID, sha256Hex string, maxFails int) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	if len(deviceIDs) == 0 || sha256Hex == "" {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT ct.target_id,
		       COUNT(*) FILTER (WHERE cs.status = 'failed'),
		       BOOL_OR(r.output ILIKE '%signed with a different key%' OR r.output ILIKE '%INSTALL_FAILED_UPDATE_INCOMPATIBLE%')
		FROM commands c
		JOIN command_targets ct ON ct.command_id = c.id
		LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = ct.target_id
		LEFT JOIN command_results r ON r.command_id = c.id AND r.device_id = ct.target_id
		WHERE c.type = 'app_update'
		  AND c.created_by = 'auto-update'
		  AND ct.target_id = ANY($1)
		  AND c.payload->>'sha256' = $2
		  AND c.created_at > NOW() - INTERVAL '30 days'
		GROUP BY ct.target_id`, deviceIDs, sha256Hex)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var fails int
		var key *bool
		if err := rows.Scan(&id, &fails, &key); err != nil {
			return nil, err
		}
		switch {
		case key != nil && *key:
			out[id] = "signed with a different key"
		case fails >= maxFails:
			out[id] = "failed " + strconv.Itoa(fails) + " times"
		}
	}
	return out, rows.Err()
}

// ListDeviceActiveAlerts returns the non-resolved alerts for a single device, worst
// severity first then newest — the alert feed for that device's Alerts tab.
// InFlightAgentUpdates reports, per device, the state of an agent update that is
// still running: 'pending' while it waits to be picked up, then whatever the device
// last said ('downloading', 'installing'). A client update takes a minute or two and
// the device says nothing while it works, so without this the Clients page snaps
// straight back to "behind" and the update looks like it did nothing.
//
// Scoped to the last 30 minutes: the same window ExpireStalledInstalls uses, so a
// command the device never ran stops claiming to be in flight forever.
func (d *DB) InFlightAgentUpdates(ctx context.Context, deviceIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	if len(deviceIDs) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT ON (ct.target_id) ct.target_id, COALESCE(cs.status, 'pending')
		FROM commands c
		JOIN command_targets ct ON ct.command_id = c.id
		JOIN devices d ON d.id = ct.target_id
		LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = ct.target_id
		WHERE c.type = 'app_update'
		  AND ct.target_id = ANY($1)
		  AND c.created_at > NOW() - INTERVAL '30 minutes'
		  AND COALESCE(cs.status, 'pending') IN ('pending', 'delivered', 'downloading', 'installing')
		  -- A command is only "in flight" if the device could have taken it. One that
		  -- has not checked in since the command was created has not seen it: showing
		  -- an update in progress on a device that has been silent for a day is how a
		  -- moved (or dead) device reads as busy, and the operator waits for nothing.
		  AND (COALESCE(cs.status, 'pending') <> 'pending' OR d.last_seen_at > c.created_at)
		  -- And a device reporting to another MDM will never collect it at all.
		  AND d.custody_server = ''
		ORDER BY ct.target_id, c.created_at DESC`, deviceIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var st string
		if err := rows.Scan(&id, &st); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}
