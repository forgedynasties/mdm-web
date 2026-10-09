// Package command is the registry of what each command type is and how it must be
// delivered.
//
// These rules existed before this package — spread across a SQL CASE in
// ExpireOverdueCommands, a `break` after reboot in three delivery loops, the OTA
// exemptions inside GetPendingCommandsForDevice, and two minute-jobs. Spreading them
// is how they drifted: a 'query' missing from one list sat pending forever, and a boot
// splash command could be created for a DPC agent that can only answer "unsupported".
// One table, read at enqueue and at delivery, is the fix.
package command

import "time"

// Guarantee is what a type promises about running more than once. It is a property of
// the action, not of the transport: rebooting a tablet twice is a visit from a customer's
// point of view, installing the same APK twice is nothing.
type Guarantee string

const (
	// AtMostOnce: a double-run is worse than a miss. Never re-delivered on a hunch.
	AtMostOnce Guarantee = "at_most_once"
	// AtLeastOnce: safe to hand out again; the client dedups by command id.
	AtLeastOnce Guarantee = "at_least_once"
	// BestEffortLive: only meaningful on a live socket. Never queued.
	BestEffortLive Guarantee = "best_effort_live"
)

// Lanes serialize work against its own kind instead of against everything for the device.
// A wedged install must not hold up a screenshot or a reboot (the per-device FIFO gate
// today does exactly that, with OTA carved out of it by hand).
const (
	LaneDefault  = "default"
	LaneInstall  = "install"
	LaneFirmware = "firmware"
	LaneShell    = "shell"
	LaneDevice   = "device"
)

// Spec is one command type's delivery policy.
type Spec struct {
	Guarantee Guarantee
	Lane      string
	// Lease is how long a device may hold this command before it returns to the queue.
	// It is a work-time budget, not a deadline: an install heartbeats it with the
	// interim 'downloading'/'installing' acks both clients already send.
	Lease time.Duration
	// Deadline is how long the command stays worth delivering at all. Zero means it has
	// no deadline of its own (reboot: its lifecycle is owned by CompleteDeliveredReboots).
	Deadline time.Duration
	// MaxAttempts caps hand-outs. Zero means unlimited, which is today's behaviour and
	// stays the default — a device that is offline for an hour must not burn its attempts
	// on deliveries nobody could have received.
	MaxAttempts int
	// NeedsCaps are device capabilities required to run this at all. Checked at enqueue so
	// an impossible command is refused rather than answered 'unsupported' minutes later.
	NeedsCaps []string
	// LiveOnly: never put in a queue, never handed out over HTTP.
	LiveOnly bool
}

// PushLease is the lease taken when a command is written to a WebSocket. It is short on
// purpose: a push is unconfirmed — a half-open socket swallows the frame — so the command
// must become deliverable again quickly. An HTTP hand-out is confirmed by its own 200, so
// it takes the type's full lease.
const PushLease = 90 * time.Second

var specs = map[string]Spec{
	// ── installs: long, resumable, safe to repeat ──────────────────────────────
	"install_apk": {AtLeastOnce, LaneInstall, 30 * time.Minute, 2 * time.Hour, 0, nil, false},
	"app_update":  {AtLeastOnce, LaneInstall, 30 * time.Minute, 2 * time.Hour, 0, nil, false},
	"uninstall":   {AtLeastOnce, LaneInstall, 10 * time.Minute, time.Hour, 0, nil, false},

	// ── firmware: its own lane, and exempt from the device lane in both directions ──
	"ota": {AtLeastOnce, LaneFirmware, 2 * time.Hour, 6 * time.Hour, 0, []string{"ota"}, false},

	// ── one-shot device acts ───────────────────────────────────────────────────
	// Reboot completes on the next boot, not on an ack, so it carries no deadline and is
	// never speculatively re-sent: a surprise reboot in a restaurant is the worst thing
	// this system can do by accident.
	"reboot":   {AtMostOnce, LaneDevice, 10 * time.Minute, 0, 0, nil, false},
	"wipe":     {AtMostOnce, LaneDevice, 10 * time.Minute, time.Hour, 1, nil, false},
	"unenroll": {AtMostOnce, LaneDevice, 10 * time.Minute, time.Hour, 1, nil, false},

	// ── operator tools: short-lived, answer or nothing ─────────────────────────
	"shell":         {AtLeastOnce, LaneShell, 5 * time.Minute, 5 * time.Minute, 0, nil, false},
	"query":         {AtLeastOnce, LaneShell, 5 * time.Minute, 5 * time.Minute, 0, nil, false},
	"collect_logs":  {AtLeastOnce, LaneShell, 10 * time.Minute, 15 * time.Minute, 0, nil, false},
	"screenshot":    {AtLeastOnce, LaneShell, 5 * time.Minute, 5 * time.Minute, 0, nil, false},
	"mic_gain_read": {AtLeastOnce, LaneShell, 5 * time.Minute, 5 * time.Minute, 0, nil, false},
	"mic_gain_set":  {AtLeastOnce, LaneShell, 5 * time.Minute, 5 * time.Minute, 0, nil, false},
	"adb_tcp":       {AtLeastOnce, LaneShell, 5 * time.Minute, 10 * time.Minute, 0, nil, false},

	// ── state pushes ───────────────────────────────────────────────────────────
	"config": {AtLeastOnce, LaneDefault, 10 * time.Minute, time.Hour, 0, nil, false},
	// Needs the system partition, so the DPC agent can only refuse it.
	"update_splash": {AtLeastOnce, LaneDefault, 20 * time.Minute, time.Hour, 0, []string{"splash"}, false},

	// ── live sessions: a queue would be meaningless ────────────────────────────
	"start_capture": {BestEffortLive, LaneDefault, 0, 0, 0, nil, true},
	"stop_capture":  {BestEffortLive, LaneDefault, 0, 0, 0, nil, true},
}

// Default is what an unknown type gets: queued, repeatable, ten minutes to work. An
// unknown type is a type a newer dashboard knows and this server does not, so it must
// still be deliverable.
var Default = Spec{Guarantee: AtLeastOnce, Lane: LaneDefault, Lease: 10 * time.Minute, Deadline: time.Hour}

// For returns the spec for a stored command type.
func For(cmdType string) Spec {
	if s, ok := specs[cmdType]; ok {
		return s
	}
	return Default
}

// Known reports whether this type is in the registry (as opposed to taking Default).
func Known(cmdType string) bool {
	_, ok := specs[cmdType]
	return ok
}
