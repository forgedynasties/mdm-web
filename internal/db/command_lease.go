package db

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/google/uuid"
)

// Leases and the command event log.
//
// Before this, "delivered" meant hub.Push had written a frame to a socket — which a
// half-open socket swallows, so the queue could show an operator a delivery tick for a
// command the device never saw (AT070AABU00077, 9 Oct 2026). Three things then existed to
// paper over it: a 90-second re-push sweep, a per-minute re-flush of every connected
// device, and a stalled-install expiry job.
//
// A lease says something checkable instead: THIS device holds this command until THIS
// time. It expires on its own, so a command returns to the queue with no sweep at all,
// and whoever holds it is never ambiguous.

// LeaseCommand hands a command to a device for dur, and records it. Called by every
// delivery path — WS push (with a short command.PushLease, because a push is unconfirmed)
// and the HTTP pull (with the type's own lease, because its 200 confirms the hand-over).
//
// Status is only advanced out of 'pending': a command already 'downloading' on the device
// must not be walked backwards to 'delivered' by a re-lease.
func (d *DB) LeaseCommand(ctx context.Context, commandID, deviceID uuid.UUID, dur time.Duration, transport string) (uuid.UUID, error) {
	leaseID := uuid.New()
	_, err := d.pool.Exec(ctx, `
		INSERT INTO command_status (command_id, device_id, status, lease_id, lease_expires_at, attempts, updated_at)
		VALUES ($1, $2, 'delivered', $3, NOW() + $4::interval, 1, NOW())
		ON CONFLICT (command_id, device_id) DO UPDATE
		SET lease_id         = $3,
		    lease_expires_at = NOW() + $4::interval,
		    attempts         = command_status.attempts + 1,
		    status           = CASE WHEN command_status.status = 'pending'
		                            THEN 'delivered' ELSE command_status.status END,
		    updated_at       = NOW()
	`, commandID, deviceID, leaseID, dur)
	if err != nil {
		return uuid.Nil, err
	}
	d.AppendCommandEvent(ctx, commandID, deviceID, "leased", map[string]any{
		"transport": transport,
		"lease_id":  leaseID,
		"expires_in": dur.String(),
	})
	return leaseID, nil
}

// ReleaseDeviceLeases drops the leases a device holds, making its commands immediately
// deliverable again. Called when a device reconnects over WS: a fresh socket is proof the
// previous delivery is dead, which is the behaviour the WS flush has always relied on —
// without this, a reconnecting device would wait out a lease it can no longer honour.
func (d *DB) ReleaseDeviceLeases(ctx context.Context, deviceID uuid.UUID) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE command_status
		SET lease_id = NULL, lease_expires_at = NULL, updated_at = NOW()
		WHERE device_id = $1 AND lease_expires_at IS NOT NULL
		  AND status NOT IN ('installed', 'failed', 'completed', 'cancelled', 'expired')
	`, deviceID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// AppendCommandEvent writes one line of history. Append-only and never read by the
// delivery path: command_status stays the live state, so nothing here can break a
// delivery. It exists so "when did the device actually get this" is answerable — the
// question that cost a day on 00077 — and so the UI can one day show 'received' as the
// tick that counts rather than 'sent'.
//
// Best-effort by design: history must never fail an ack.
func (d *DB) AppendCommandEvent(ctx context.Context, commandID, deviceID uuid.UUID, kind string, detail any) {
	detailJSON := []byte("{}")
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			detailJSON = b
		}
	}
	var dev *uuid.UUID
	if deviceID != uuid.Nil {
		dev = &deviceID
	}
	if _, err := d.pool.Exec(ctx, `
		INSERT INTO command_events (command_id, device_id, kind, detail)
		VALUES ($1, $2, $3, $4::jsonb)`, commandID, dev, kind, detailJSON); err != nil {
		log.Printf("[command-events] append %s for %s: %v", kind, commandID, err)
	}
}

// CommandEvent is one row of a command's history, for the device/command detail pages.
type CommandEvent struct {
	At     time.Time       `json:"at"`
	Kind   string          `json:"kind"`
	Detail json.RawMessage `json:"detail"`
}

// ListCommandEvents returns one command's history for a device, oldest first.
func (d *DB) ListCommandEvents(ctx context.Context, commandID, deviceID uuid.UUID) ([]CommandEvent, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT at, kind, detail FROM command_events
		WHERE command_id = $1 AND (device_id = $2 OR device_id IS NULL)
		ORDER BY at, id`, commandID, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommandEvent
	for rows.Next() {
		var e CommandEvent
		if err := rows.Scan(&e.At, &e.Kind, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetCommandPolicy stamps a new command with its registry policy, so the delivery and
// expiry paths read a column instead of re-deciding per query. Written at creation; no-op
// for rows created before this existed (they keep the defaults).
func (d *DB) SetCommandPolicy(ctx context.Context, commandID uuid.UUID, lane, guarantee string, deadline time.Duration, maxAttempts int) error {
	var deadlineAt any
	if deadline > 0 {
		deadlineAt = time.Now().Add(deadline)
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE commands SET lane = $2, guarantee = $3, deadline_at = $4, max_attempts = $5
		WHERE id = $1`, commandID, lane, guarantee, deadlineAt, maxAttempts)
	return err
}

// SetCommandChannel records how this device can be commanded right now: "ws" (socket,
// instant), "http" (no socket, polls — reachable but delayed) or "none" (no socket and a
// client with no pull path: every firmware build before 1.9.5, which is all 101 v2.0.x
// devices). One field so the dashboard can answer "will this run?", which no notion of
// presence we had could answer.
func (d *DB) SetCommandChannel(ctx context.Context, deviceID uuid.UUID, channel string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE devices SET command_channel = $2 WHERE id = $1 AND command_channel <> $2`,
		deviceID, channel)
	return err
}

// StampCommandRoundTrip records that a command completed end to end for this device: the
// only evidence that it is actually controllable, as opposed to merely reporting. A daily
// canary plus this column turns "we think the fleet is manageable" into a number.
func (d *DB) StampCommandRoundTrip(ctx context.Context, deviceID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE devices SET last_round_trip_ok_at = NOW() WHERE id = $1`, deviceID)
	return err
}
