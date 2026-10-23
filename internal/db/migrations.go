package db


const migrationSQL = `
CREATE TABLE IF NOT EXISTS devices (
	id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	serial_number TEXT        NOT NULL UNIQUE,
	build_id      TEXT        NOT NULL DEFAULT '',
	latest_battery_pct SMALLINT    NOT NULL DEFAULT 0,
	latest_extra  JSONB       NOT NULL DEFAULT '{}',
	last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- checkins (the per-report snapshot history) was retired on 24 Sep 2026: device_samples
-- and device_state_events hold the history, and the device row holds the fingerprint the
-- store-or-coalesce decision needs. It is no longer created here, so a table dropped by
-- hand stays dropped.

CREATE TABLE IF NOT EXISTS groups (
	id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	name       TEXT NOT NULL UNIQUE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS device_groups (
	device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	group_id  UUID NOT NULL REFERENCES groups(id)  ON DELETE CASCADE,
	PRIMARY KEY (device_id, group_id)
);

CREATE TABLE IF NOT EXISTS commands (
	id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	type        TEXT NOT NULL DEFAULT 'install_apk',
	apk_url     TEXT NOT NULL,
	target_type TEXT NOT NULL,
	created_by  TEXT NOT NULL DEFAULT '',
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS command_targets (
	command_id UUID NOT NULL REFERENCES commands(id) ON DELETE CASCADE,
	target_id  UUID NOT NULL,
	PRIMARY KEY (command_id, target_id)
);

CREATE TABLE IF NOT EXISTS command_status (
	command_id UUID NOT NULL REFERENCES commands(id) ON DELETE CASCADE,
	device_id  UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	status     TEXT NOT NULL DEFAULT 'delivered',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (command_id, device_id)
);

CREATE TABLE IF NOT EXISTS apps (
	id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	name       TEXT NOT NULL,
	apk_url    TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE INDEX IF NOT EXISTS idx_devices_last_seen   ON devices(last_seen_at DESC);
CREATE INDEX IF NOT EXISTS idx_devices_serial_trgm ON devices USING GIN (serial_number gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_device_groups_group_id_device_id ON device_groups(group_id, device_id);

CREATE TABLE IF NOT EXISTS logcat_requests (
	id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	device_id  UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	level      TEXT NOT NULL DEFAULT 'W',
	lines      INT  NOT NULL DEFAULT 500,
	tag        TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL DEFAULT 'pending',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS logcat_results (
	id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	request_id UUID NOT NULL REFERENCES logcat_requests(id) ON DELETE CASCADE,
	device_id  UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	content    TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_logcat_requests_device_id ON logcat_requests(device_id);
CREATE INDEX IF NOT EXISTS idx_logcat_results_request_id ON logcat_results(request_id);

ALTER TABLE commands ADD COLUMN IF NOT EXISTS payload JSONB NOT NULL DEFAULT '{}';

CREATE TABLE IF NOT EXISTS command_results (
	id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	command_id UUID NOT NULL REFERENCES commands(id) ON DELETE CASCADE,
	device_id  UUID NOT NULL REFERENCES devices(id)  ON DELETE CASCADE,
	output     TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	UNIQUE(command_id, device_id)
);

CREATE INDEX IF NOT EXISTS idx_command_results_command_id ON command_results(command_id);

CREATE TABLE IF NOT EXISTS device_packages (
	device_id    UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	package_name TEXT NOT NULL,
	app_name     TEXT NOT NULL DEFAULT '',
	version_name TEXT NOT NULL DEFAULT '',
	updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (device_id, package_name)
);

CREATE INDEX IF NOT EXISTS idx_device_packages_device_id   ON device_packages(device_id);
CREATE INDEX IF NOT EXISTS idx_device_packages_package_name ON device_packages(package_name);

-- Shared package_name → launcher-icon index. Any device that reports an icon populates
-- it; every device's app list then resolves icons from here by package_name, so old
-- clients that never send icons still render them. PRIMARY KEY is the lookup index.
CREATE TABLE IF NOT EXISTS app_icons (
	package_name TEXT PRIMARY KEY,
	icon         TEXT NOT NULL,
	updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE device_packages ADD COLUMN IF NOT EXISTS app_name TEXT NOT NULL DEFAULT '';
ALTER TABLE device_packages ADD COLUMN IF NOT EXISTS is_system BOOLEAN NOT NULL DEFAULT FALSE;
-- is_system becomes nullable: NULL means the device's client is too old to report it,
-- so the admin override (app_system_overrides) decides the classification instead.
ALTER TABLE device_packages ALTER COLUMN is_system DROP NOT NULL;
ALTER TABLE device_packages ALTER COLUMN is_system DROP DEFAULT;

-- Admin-managed list of package names flagged as system apps. Used as the fallback
-- classification when no client has reported is_system for a package.
CREATE TABLE IF NOT EXISTS app_system_overrides (
	package_name TEXT PRIMARY KEY,
	created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE devices ADD COLUMN IF NOT EXISTS poll_interval_ms INTEGER NOT NULL DEFAULT 30000;
ALTER TABLE devices ADD COLUMN IF NOT EXISTS latest_battery_pct SMALLINT NOT NULL DEFAULT 0;
ALTER TABLE devices ADD COLUMN IF NOT EXISTS latest_extra JSONB NOT NULL DEFAULT '{}';

-- Battery wear: running total of cumulative percent discharged, never reset.
-- UpsertCheckin adds each downward step in battery level (charging/flat frames add 0).
-- Equivalent full cycles = discharge_total_pct / 100 (250 => 2.5 cycles).
ALTER TABLE devices ADD COLUMN IF NOT EXISTS discharge_total_pct BIGINT NOT NULL DEFAULT 0;
-- Guards the ONE-TIME historical backfill (BackfillDischargeCycles, run in the
-- background at startup — NOT here; see the checkins-backfill warning above).
-- Existing rows start false (need seeding from history); new devices default true
-- since UpsertCheckin maintains their counter from the first check-in.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS discharge_backfilled BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE devices ALTER COLUMN discharge_backfilled SET DEFAULT true;

-- NOTE: A one-time backfill of devices.latest_battery_pct / latest_extra from the
-- newest checkin per device used to live here (a DISTINCT ON over the whole
-- checkins table). It ran on EVERY startup inside this migration batch, doing a
-- full scan + sort of checkins — instant on a fresh DB but ~30 min and IO-bound
-- on production where checkins has millions of rows, blocking the server from
-- ever binding its port. It is also redundant: UpsertCheckin keeps both columns
-- current on every checkin. Removed. If a fresh backfill is ever needed again,
-- run it once manually, not from the recurring startup migration.

CREATE TABLE IF NOT EXISTS device_config (
	device_id      UUID    PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
	kiosk_enabled  BOOLEAN NOT NULL DEFAULT false,
	kiosk_package  TEXT    NOT NULL DEFAULT '',
	kiosk_features INTEGER NOT NULL DEFAULT 1,
	updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Offline kiosk-exit: a provisioned TOTP seed lets a technician leave kiosk lock
-- mode on-device without server access. Enabled by default (opt-out per device);
-- the seed is generated lazily on first read (see GetOrCreateDeviceConfig).
ALTER TABLE device_config ADD COLUMN IF NOT EXISTS offline_exit_enabled BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE device_config ADD COLUMN IF NOT EXISTS offline_exit_seed TEXT NOT NULL DEFAULT '';
ALTER TABLE device_config ADD COLUMN IF NOT EXISTS offline_exit_relock TEXT NOT NULL DEFAULT 'reboot';
-- Wireless-charging control: the client writes the customer_gpio line to enable/disable
-- charging on the pad. Default true (charging on); pushed to the device like kiosk config.
ALTER TABLE device_config ADD COLUMN IF NOT EXISTS wlc_charging_enabled BOOLEAN NOT NULL DEFAULT TRUE;
-- Screen brightness (21 Oct 2026): the level the client sets (1-255, NULL = not managed)
-- and whether it holds it there (QS slider greyed out, any change put back).
ALTER TABLE device_config ADD COLUMN IF NOT EXISTS screen_brightness SMALLINT;
ALTER TABLE device_config ADD COLUMN IF NOT EXISTS screen_brightness_locked BOOLEAN NOT NULL DEFAULT FALSE;
-- Who set a device's own level, and when (21 Oct: the device page says "set for this
-- device by …"). NULL level = the default (122, locked, on Tableside AI; unmanaged on others).
ALTER TABLE device_config ADD COLUMN IF NOT EXISTS screen_brightness_set_by TEXT NOT NULL DEFAULT '';
ALTER TABLE device_config ADD COLUMN IF NOT EXISTS screen_brightness_set_at TIMESTAMPTZ;

-- A named, standing kiosk-lock policy assigned to a restaurant/group/device/the whole
-- fleet — the Manage page's unit, not a one-shot command. Applying/editing/deleting a
-- policy resolves its target to the matching device IDs AT THAT MOMENT and writes
-- device_config directly (same mechanism as before policies existed); it is not a live
-- binding that tracks membership drift after the fact — a device added to a target group
-- later doesn't retroactively pick up the policy without a re-save. target_id holds a
-- restaurant_id or group_id depending on target_type; target_serial holds a device
-- serial for target_type='device'. Exactly one of them is set (or neither, for 'all').
CREATE TABLE IF NOT EXISTS kiosk_policies (
	id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	name          TEXT NOT NULL,
	kiosk_package TEXT NOT NULL,
	target_type   TEXT NOT NULL, -- 'all' | 'restaurant' | 'group' | 'device'
	target_id     UUID,
	target_serial TEXT NOT NULL DEFAULT '',
	created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS ota_packages (
	id              SERIAL      PRIMARY KEY,
	build_id        TEXT        NOT NULL UNIQUE,
	release_date    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	update_url      TEXT        NOT NULL,
	payload_offset  BIGINT      NOT NULL DEFAULT 0,
	payload_size    BIGINT      NOT NULL DEFAULT 0,
	payload_headers TEXT[]      NOT NULL DEFAULT '{}',
	created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE ota_packages ADD COLUMN IF NOT EXISTS type TEXT NOT NULL DEFAULT 'full';
-- wipe: installing the package factory-resets the device (built with --wipe; the zip says
-- ota-wipe=yes / POWERWASH=1). Read from the package itself when it is added, or from the
-- publisher; wipe_checked marks packages whose zip has been read (the startup backfill
-- reads the rest). A wipe package is never a silent fallback: see ResolveUpdateForDevice.
ALTER TABLE ota_packages ADD COLUMN IF NOT EXISTS wipe BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE ota_packages ADD COLUMN IF NOT EXISTS wipe_checked BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE ota_packages ADD COLUMN IF NOT EXISTS target_build_id TEXT NOT NULL DEFAULT '';
ALTER TABLE ota_packages ADD COLUMN IF NOT EXISTS source_build_id TEXT NOT NULL DEFAULT '';

-- Migrate existing build_id to target_build_id
UPDATE ota_packages SET target_build_id = build_id WHERE target_build_id = '';

-- Allow multiple packages with the same build_id
ALTER TABLE ota_packages DROP CONSTRAINT IF EXISTS ota_packages_build_id_key;

CREATE TABLE IF NOT EXISTS updates (
	id              SERIAL      PRIMARY KEY,
	ota_package_id  INTEGER     NOT NULL REFERENCES ota_packages(id) ON DELETE CASCADE,
	reboot_behavior TEXT        NOT NULL DEFAULT 'immediate',
	scheduled_time  TIMESTAMPTZ,
	created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE updates ADD COLUMN IF NOT EXISTS scheduled_time TIMESTAMPTZ;

ALTER TABLE groups  ADD COLUMN IF NOT EXISTS ota_package_id INTEGER REFERENCES ota_packages(id) ON DELETE SET NULL;
ALTER TABLE devices ADD COLUMN IF NOT EXISTS ota_package_id INTEGER REFERENCES ota_packages(id) ON DELETE SET NULL;

ALTER TABLE updates ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE updates ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS update_devices (
	update_id  INTEGER   NOT NULL REFERENCES updates(id) ON DELETE CASCADE,
	device_id  UUID      NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	status     TEXT      NOT NULL DEFAULT 'pending',
	PRIMARY KEY (update_id, device_id)
);

ALTER TABLE devices ADD COLUMN IF NOT EXISTS hidden BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS productions (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name           TEXT NOT NULL,
    product_code   TEXT NOT NULL,
    model_code     TEXT NOT NULL,
    variant        CHAR(1) NOT NULL DEFAULT '0',
    sku            CHAR(2) NOT NULL DEFAULT 'AA',
    batch          CHAR(2) NOT NULL,
    batch_month    INT NOT NULL,
    batch_year     INT NOT NULL,
    start_sequence INT NOT NULL,
    end_sequence   INT NOT NULL,
    notes          TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_productions_batch ON productions(batch);
CREATE INDEX IF NOT EXISTS idx_productions_prefix ON productions(product_code, model_code, variant, sku, batch);
-- The hardware model a production run built (a product key, as on the Hardware page);
-- '' = not linked.
ALTER TABLE productions ADD COLUMN IF NOT EXISTS model TEXT NOT NULL DEFAULT '';

ALTER TABLE ota_packages ADD COLUMN IF NOT EXISTS changelog TEXT NOT NULL DEFAULT '';
ALTER TABLE ota_packages ADD COLUMN IF NOT EXISTS status    TEXT NOT NULL DEFAULT 'active';

CREATE TABLE IF NOT EXISTS users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('viewer','support','operator','fleet_manager','fleet_admin','tester','user_manager','super_op','dev','admin','owner','staff')),
    first_name    TEXT NOT NULL DEFAULT '',
    last_name     TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS audit_log (
    id         BIGSERIAL PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    actor      TEXT NOT NULL DEFAULT '',
    action     TEXT NOT NULL,
    target     TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_log(created_at DESC);
-- The dock asks "which pages does this person open most" on every page load, which
-- is a filter on actor + action before it counts. Without this it is a scan of the
-- whole audit log, and that table only grows.
CREATE INDEX IF NOT EXISTS idx_audit_actor_action ON audit_log(actor, action, created_at DESC);

-- Which device an action was performed on, when it was performed on one. The target
-- column cannot answer this: it holds a serial for some actions, a comma-joined list
-- for bulk ones, a deployment id for others and a URL path for page views, so asking
-- "what happened to this device" meant matching text against a column that means a
-- different thing per action. ON DELETE SET NULL, not CASCADE: the log is the record
-- of what people did, and un-enrolling a device must not erase it.
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS device_id UUID REFERENCES devices(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_audit_device_created ON audit_log(device_id, created_at DESC) WHERE device_id IS NOT NULL;

-- Attach the history that can be attached. Most single-device actions already wrote
-- the serial into target, so those rows can be linked to their device exactly; bulk
-- actions wrote a comma-joined list and simply will not match, which is the right
-- outcome — a half-parsed list is worse than no link. Idempotent (only fills NULLs),
-- and the LIKE keeps it off the page-view rows that are most of the table.
UPDATE audit_log a SET device_id = d.id
FROM devices d
WHERE a.device_id IS NULL
  AND a.target = d.serial_number
  AND (a.action LIKE 'device.%' OR a.action LIKE 'deployment.%' OR a.action LIKE 'legacy_ota.%');

-- Per-device per-day rollup of checkin telemetry (Tier 1 descriptive analytics).
-- Populated by RollupDailyStats; queried for trends so we never scan raw checkins.
CREATE TABLE IF NOT EXISTS device_daily_stats (
    device_id      UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    day            DATE NOT NULL,
    checkin_count  INTEGER NOT NULL DEFAULT 0,
    battery_min    SMALLINT,
    battery_max    SMALLINT,
    battery_avg    REAL,
    temp_max       REAL,
    ram_pct_peak   SMALLINT,
    charging_frac  REAL,          -- fraction of checkins reporting charging=true
    online_minutes INTEGER NOT NULL DEFAULT 0, -- distinct minute buckets with a checkin
    build_id       TEXT NOT NULL DEFAULT '',    -- last build_id seen that day
    first_seen_at  TIMESTAMPTZ,
    last_seen_at   TIMESTAMPTZ,
    computed_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (device_id, day)
);
CREATE INDEX IF NOT EXISTS idx_device_daily_stats_day ON device_daily_stats(day DESC);

-- Alert rules: a rule type + JSONB params (thresholds), optionally scoped. The
-- evaluator (RunHousekeeping) reads enabled rules and fires alerts against them.
CREATE TABLE IF NOT EXISTS alert_rules (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    type       TEXT NOT NULL,                 -- overheating | temp_elevated | offline | storage_low | ...
    name       TEXT NOT NULL DEFAULT '',
    enabled    BOOLEAN NOT NULL DEFAULT true,
    params     JSONB NOT NULL DEFAULT '{}',   -- thresholds, e.g. {"temp_c":45}
    scope_type TEXT NOT NULL DEFAULT 'fleet', -- fleet | group | device
    scope_id   UUID,                          -- group_id or device_id when scoped
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Fired alert instances. At most one OPEN alert per (type, device) via the partial
-- unique index below, so re-evaluating a still-true condition does not duplicate.
CREATE TABLE IF NOT EXISTS alerts (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_id     UUID REFERENCES alert_rules(id) ON DELETE SET NULL,
    type        TEXT NOT NULL,
    device_id   UUID REFERENCES devices(id) ON DELETE CASCADE,
    severity    TEXT NOT NULL DEFAULT 'warning', -- info | warning | critical
    status      TEXT NOT NULL DEFAULT 'open',    -- open | acknowledged | resolved
    summary     TEXT NOT NULL DEFAULT '',
    detail      JSONB NOT NULL DEFAULT '{}',
    fired_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at TIMESTAMPTZ,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_alerts_open_unique ON alerts(type, device_id) WHERE status <> 'resolved';
CREATE INDEX IF NOT EXISTS idx_alerts_status ON alerts(status, fired_at DESC);
CREATE INDEX IF NOT EXISTS idx_alerts_device ON alerts(device_id);

-- AI analysis token usage, rolled up per day (one row per calendar day). Each AI
-- call increments calls and adds its input/output token counts.
CREATE TABLE IF NOT EXISTS ai_usage (
    day           DATE PRIMARY KEY,
    calls         INTEGER NOT NULL DEFAULT 0,
    input_tokens  BIGINT  NOT NULL DEFAULT 0,
    output_tokens BIGINT  NOT NULL DEFAULT 0
);

-- Learned WiFi access-point → location index. Populated from successful Google
-- Geolocation lookups (each contributing BSSID stamped with the scan's resolved
-- point). Lets the server locate a device from a WiFi scan that overlaps known
-- APs WITHOUT calling Google — the primary cost optimization. Entries expire by
-- seen_at (staleness), and hits counts how many times an AP helped serve a
-- lookup locally.
CREATE TABLE IF NOT EXISTS wifi_ap_locations (
    bssid      TEXT PRIMARY KEY,
    lat        DOUBLE PRECISION NOT NULL,
    lon        DOUBLE PRECISION NOT NULL,
    accuracy   DOUBLE PRECISION NOT NULL DEFAULT 0,
    hits       BIGINT      NOT NULL DEFAULT 0,
    seen_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_wifi_ap_seen ON wifi_ap_locations(seen_at DESC);

-- Cached AI summaries, one row per scope (e.g. 'fleet'). Regenerated periodically
-- by housekeeping so the dashboard can show it without an on-demand API call.
CREATE TABLE IF NOT EXISTS ai_summary (
    scope        TEXT PRIMARY KEY,
    summary      TEXT NOT NULL DEFAULT '',
    model        TEXT NOT NULL DEFAULT '',
    generated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Per-device AI analyses, kept as a short history so the device page can show
-- past readings alongside a fresh one.
CREATE TABLE IF NOT EXISTS device_ai_analyses (
    id         BIGSERIAL PRIMARY KEY,
    device_id  UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    model      TEXT NOT NULL DEFAULT '',
    text       TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_device_ai_analyses_device ON device_ai_analyses (device_id, created_at DESC);

-- One-time data migrations that must run exactly once (no versioned migration tool).
CREATE TABLE IF NOT EXISTS app_flags (
    flag       TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Offline alerts were de-prioritized: disable the existing offline rule once, so an
-- upgrade reflects the new default without clobbering it if the admin re-enables it.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM app_flags WHERE flag = 'offline_rule_disabled_v1') THEN
        UPDATE alert_rules SET enabled = false WHERE type = 'offline';
        INSERT INTO app_flags (flag) VALUES ('offline_rule_disabled_v1');
    END IF;
END $$;

-- (The legacy deployed flags on groups/devices/restaurants are dropped further down:
-- a device is now "deployed" iff it is assigned to a restaurant — see the DROP COLUMN
-- statements at the end of this migration.)

-- Server-side dashboard sessions (GB-04/GB-08). The cookie carries only the
-- opaque session id; identity, role and validity live here so logout and admin
-- revoke delete the row immediately and a single stolen cookie can be killed
-- without rotating the signing key for everyone. user_id is NULL for the static
-- env-configured admin; ON DELETE CASCADE revokes a DB user's sessions when the
-- user is removed.
CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    user_id    UUID REFERENCES users(id) ON DELETE CASCADE,
    username   TEXT NOT NULL,
    role       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

-- Per-device OTA detail for the deployment view: when the row last changed
-- state (to surface "last updated" and flag devices stalled mid-update) and the
-- error_code the device reported on failure (to show why it failed).
ALTER TABLE update_devices ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
ALTER TABLE update_devices ADD COLUMN IF NOT EXISTS error_code TEXT        NOT NULL DEFAULT '';
-- Update timing: started_at is stamped when the device begins downloading and
-- completed_at when it reports installed, so we can show per-device "time taken"
-- and the deployment's average. Both set once (never bumped by later transitions).
ALTER TABLE update_devices ADD COLUMN IF NOT EXISTS started_at   TIMESTAMPTZ;
ALTER TABLE update_devices ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ;

-- Releases group the packages for one target build version (the full image plus
-- its incrementals) under a single lifecycle (draft -> published).
-- Deploying a release lets the server pick the right artifact per device.
CREATE TABLE IF NOT EXISTS releases (
    id           SERIAL      PRIMARY KEY,
    version      TEXT        NOT NULL UNIQUE,
    name         TEXT        NOT NULL DEFAULT '',
    changelog    TEXT        NOT NULL DEFAULT '',
    status       TEXT        NOT NULL DEFAULT 'draft',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at TIMESTAMPTZ
);
ALTER TABLE ota_packages ADD COLUMN IF NOT EXISTS release_id INTEGER REFERENCES releases(id);
ALTER TABLE updates      ADD COLUMN IF NOT EXISTS release_id INTEGER REFERENCES releases(id);

-- Per-product releases: a release targets one hardware product (t7/kiosk18/22/27), so a
-- device is only ever offered/sent a release matching its own product (enforced in
-- ResolveUpdateForDevice). Existing releases predate multi-product and were all T7, so
-- backfill + default to t7 — consistent with the devices empty->t7 rule.
-- Identity is (version, product), NOT version alone: the same version string can exist
-- for different products (a t7 "2026.06.01" and a kiosk22 "2026.06.01" are two distinct
-- releases). This must run BEFORE the backfill seed below, whose ON CONFLICT targets the
-- composite index.
ALTER TABLE releases ADD COLUMN IF NOT EXISTS product TEXT NOT NULL DEFAULT 't7';
ALTER TABLE releases DROP CONSTRAINT IF EXISTS releases_version_key;
CREATE UNIQUE INDEX IF NOT EXISTS uq_releases_version_product ON releases(version, product);

-- Backfill one release per existing target build (falling back to the legacy
-- build_id when target_build_id is blank). Existing packages are marked published
-- so deployments already in flight keep resolving; then link packages and updates.
-- Only packages not yet linked: this runs on every start, and a linked package for a
-- non-t7 product would otherwise seed a phantom t7 twin of its release (product
-- defaults to t7 here, so the (version, product) conflict never fires).
INSERT INTO releases (version, status, changelog, created_at, published_at)
  SELECT COALESCE(NULLIF(target_build_id,''), build_id), 'published', MAX(changelog), MIN(created_at), MIN(created_at)
  FROM ota_packages
  WHERE release_id IS NULL
  GROUP BY COALESCE(NULLIF(target_build_id,''), build_id)
  ON CONFLICT (version, product) DO NOTHING;
UPDATE ota_packages p SET release_id = r.id
  FROM releases r
  WHERE r.version = COALESCE(NULLIF(p.target_build_id,''), p.build_id) AND p.release_id IS NULL;
UPDATE updates u SET release_id = p.release_id
  FROM ota_packages p WHERE p.id = u.ota_package_id AND u.release_id IS NULL;

-- Release deployments pick the per-device artifact at resolve time, so a
-- deployment no longer needs a single package. (DROP NOT NULL is idempotent.)
ALTER TABLE updates ALTER COLUMN ota_package_id DROP NOT NULL;

-- ── Tier 5: T7 alert-matrix (see docs/analytics-and-alerting.md §8) ─────────────

-- Per-restaurant service windows. Rules with active_window=service|overnight gate
-- against a group's open/close hours (local minutes past midnight) so alerts only
-- fire when they matter for that venue. A group with no row uses the fleet default
-- seeded below; overnight = the configured night window (defaults to the matrix's
-- 23:30–06:00). timezone falls back to each device's reported extra.timezone.
-- group_id is nullable: a NULL row is the single fleet-default window; non-NULL rows
-- are per-group overrides. Not a PK (PKs can't be NULL); uniqueness is enforced by the
-- two partial indexes below.
CREATE TABLE IF NOT EXISTS service_windows (
    group_id        UUID REFERENCES groups(id) ON DELETE CASCADE,
    open_min        INTEGER NOT NULL DEFAULT 420,   -- 07:00 local
    close_min       INTEGER NOT NULL DEFAULT 1380,  -- 23:00 local
    night_open_min  INTEGER NOT NULL DEFAULT 1410,  -- 23:30 local
    night_close_min INTEGER NOT NULL DEFAULT 360,   -- 06:00 local
    timezone        TEXT    NOT NULL DEFAULT '',     -- '' = use device-reported tz
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- one window per group, and at most one fleet-default (NULL) row.
CREATE UNIQUE INDEX IF NOT EXISTS idx_service_windows_group
    ON service_windows (group_id) WHERE group_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_service_windows_fleet_default
    ON service_windows ((1)) WHERE group_id IS NULL;
INSERT INTO service_windows (group_id) SELECT NULL
    WHERE NOT EXISTS (SELECT 1 FROM service_windows WHERE group_id IS NULL);

-- active_window on a rule: service | overnight | always. NULL/'' = always (back-compat).
ALTER TABLE alert_rules ADD COLUMN IF NOT EXISTS active_window TEXT NOT NULL DEFAULT '';

-- deployed_only on a rule: fire only for devices assigned to a restaurant. Defaults to
-- TRUE for every rule, new and existing: a bench/lab unit is expected to be switched
-- off, left on a charging pad or filling its disk, and nobody is on call for it. Before
-- this flag the deployed-only gate was implied by active_window, so the 15 "always"
-- rules fired fleet-wide and bench units produced 45% of all alerts. An operator can
-- turn it off per rule in Settings › Alerts for a genuinely fleet-wide hardware check.
ALTER TABLE alert_rules ADD COLUMN IF NOT EXISTS deployed_only BOOLEAN NOT NULL DEFAULT TRUE;

-- Daily-stats columns for the pad-utilisation and storage-trend rules (§8.1 #11,#12,#23).
ALTER TABLE device_daily_stats ADD COLUMN IF NOT EXISTS wlc_guest_frac      REAL;     -- fraction of checkins with a guest device on the pad (wlc_status=1)
ALTER TABLE device_daily_stats ADD COLUMN IF NOT EXISTS pad_readable        BOOLEAN;  -- pad was readable at all that day (any wlc_status >= 0)
ALTER TABLE device_daily_stats ADD COLUMN IF NOT EXISTS storage_free_last_gb REAL;    -- last storage_free_gb reading of the day, for the 24h-delta rule

-- Per-restaurant power/usage metrics. All are accumulated by RollupDailyStats using the
-- same time-weighted samples CTE as the columns above, so an adaptive poll interval does
-- not skew them.
--
-- NOTE on "wlc": wlc_status is the T7's OUTGOING guest pad (gpio27) — a customer's phone
-- sitting on our tablet — not the tablet being charged. So wlc_minutes is how long we
-- charged somebody else's phone, and wlc_drain_pct is what that cost our own battery.
ALTER TABLE device_daily_stats ADD COLUMN IF NOT EXISTS wlc_minutes         REAL;     -- minutes with a guest device on the pad
ALTER TABLE device_daily_stats ADD COLUMN IF NOT EXISTS wlc_drain_pct       REAL;     -- battery points lost while the pad was busy and we were NOT on mains
ALTER TABLE device_daily_stats ADD COLUMN IF NOT EXISTS wlc_drain_minutes   REAL;     -- minutes those drops were measured over (the drain-rate denominator)
ALTER TABLE device_daily_stats ADD COLUMN IF NOT EXISTS charge_connect_sum  REAL;     -- sum of battery % at each charger connect
ALTER TABLE device_daily_stats ADD COLUMN IF NOT EXISTS charge_connect_n    INTEGER;  -- how many connects (so a week is SUM/SUM, not an average of averages)
ALTER TABLE device_daily_stats ADD COLUMN IF NOT EXISTS charge_disconnect_sum REAL;   -- sum of battery % at each charger disconnect
ALTER TABLE device_daily_stats ADD COLUMN IF NOT EXISTS charge_disconnect_n INTEGER;
ALTER TABLE device_daily_stats ADD COLUMN IF NOT EXISTS online_minutes_open REAL;     -- online_minutes restricted to the site's opening hours

-- Standby: powered but nobody using it. NULL (not 0) on a day where no sample reported
-- screen_on, so a device on firmware that predates the field reads as "not measured"
-- rather than "never used" — the difference matters when the fleet is mid-rollout.
ALTER TABLE device_daily_stats ADD COLUMN IF NOT EXISTS screen_on_minutes REAL;

-- Alert channels: where fired alerts are routed. Each channel takes alerts at or
-- above min_severity; realtime channels POST immediately, digest channels batch into
-- the daily digest. active_window optionally restricts a channel to service/overnight.
-- The legacy single AlertWebhookURL setting is migrated into one channel in code.
CREATE TABLE IF NOT EXISTS alert_channels (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL DEFAULT '',
    kind          TEXT NOT NULL DEFAULT 'webhook',     -- webhook (Slack/Discord/Mattermost-compatible)
    url           TEXT NOT NULL DEFAULT '',
    min_severity  TEXT NOT NULL DEFAULT 'warning',     -- info | warning | critical
    mode          TEXT NOT NULL DEFAULT 'realtime',    -- realtime | digest
    active_window TEXT NOT NULL DEFAULT '',             -- '' = always | service | overnight
    enabled       BOOLEAN NOT NULL DEFAULT true,
    notify_resolve BOOLEAN NOT NULL DEFAULT true,       -- post a one-liner when an alert auto-resolves
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── Restaurants (venue object — see docs/release-versioning-and-restaurants.md) ──
-- A restaurant is a real venue a device physically lives in (1 device → at most 1
-- restaurant; NULL = lab/bench unit). Restaurants own the venue semantics that used to
-- be bolted onto free-form groups: the deployed flag, service windows, health and the
-- AI-report bucketing. Groups stay as the test team's free-form tags ("mic issue", …).
CREATE TABLE IF NOT EXISTS restaurants (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    address     TEXT NOT NULL DEFAULT '',
    latitude    DOUBLE PRECISION,
    longitude   DOUBLE PRECISION,
    timezone    TEXT NOT NULL DEFAULT '',       -- IANA tz / GMT offset; '' = use device-reported
    notes       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- A device lives in at most one restaurant; NULL = lab/bench unit.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS restaurant_id UUID REFERENCES restaurants(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_devices_restaurant ON devices(restaurant_id);

-- Peak-hour windows: discrete busy periods (e.g. lunch 12:00–14:00, dinner 19:00–21:00);
-- a rule with active_window='peak' fires only inside one. Unlike service_windows (a single
-- open/close span) a venue can have MANY peak rows; restaurant_id NULL = the fleet-default
-- set, inherited by any restaurant with no rows of its own. (Defined here, after the
-- restaurants table it references.)
CREATE TABLE IF NOT EXISTS peak_windows (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    restaurant_id UUID REFERENCES restaurants(id) ON DELETE CASCADE,  -- NULL = fleet default
    start_min     INTEGER NOT NULL,   -- local minutes past midnight
    end_min       INTEGER NOT NULL,   -- may wrap past midnight (end < start)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_peak_windows_restaurant ON peak_windows(restaurant_id);
-- Seed the fleet-default peak set once: lunch 12:00–14:00 and dinner 19:00–21:00.
INSERT INTO peak_windows (restaurant_id, start_min, end_min)
SELECT NULL::uuid, v.s, v.e FROM (VALUES (720,840),(1140,1260)) v(s,e)
WHERE NOT EXISTS (SELECT 1 FROM peak_windows WHERE restaurant_id IS NULL);

-- Device events: crash/ANR/tombstone entries (from the client's DropBox reader) and
-- reboots (detected from a changed per-boot id). Deduped by (device, kind, occurred_at)
-- so the client can safely re-report the last hour on every check-in.
CREATE TABLE IF NOT EXISTS device_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    device_id   UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL,                 -- DropBox crash tag, or 'reboot'
    summary     TEXT NOT NULL DEFAULT '',
    occurred_at TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_device_events_dedupe ON device_events(device_id, kind, occurred_at);
CREATE INDEX IF NOT EXISTS idx_device_events_device_time ON device_events(device_id, occurred_at DESC);
-- Sent later by a device that was offline when it happened (client 1.7.1): history only,
-- never an alert. Only these kinds are not crashes: 'reboot', 'kiosk_exit_offline',
-- 'offline_dropped' (every crash query excludes exactly those).
ALTER TABLE device_events ADD COLUMN IF NOT EXISTS late BOOLEAN NOT NULL DEFAULT false;
-- Last per-boot id seen, to detect reboots (a change = the device rebooted).
ALTER TABLE devices ADD COLUMN IF NOT EXISTS last_boot_id TEXT NOT NULL DEFAULT '';

-- Service windows move from group_id to restaurant_id. The fleet-default row has both
-- NULL; per-restaurant rows set restaurant_id (group_id stays NULL, now unused). Redefine
-- the fleet-default uniqueness to require BOTH NULL so per-restaurant rows don't collide.
ALTER TABLE service_windows ADD COLUMN IF NOT EXISTS restaurant_id UUID REFERENCES restaurants(id) ON DELETE CASCADE;
DROP INDEX IF EXISTS idx_service_windows_fleet_default;
CREATE UNIQUE INDEX IF NOT EXISTS idx_service_windows_fleet_default
    ON service_windows ((1)) WHERE group_id IS NULL AND restaurant_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_service_windows_restaurant
    ON service_windows (restaurant_id) WHERE restaurant_id IS NOT NULL;

-- Release tracking: hide irrelevant releases from the main list (reversible), and let a
-- hard delete cascade to the release's packages and deployments so it can be fully removed.
-- (DROP+ADD makes the on-delete behavior idempotent across restarts.)
ALTER TABLE releases ADD COLUMN IF NOT EXISTS hidden BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE ota_packages DROP CONSTRAINT IF EXISTS ota_packages_release_id_fkey;
ALTER TABLE ota_packages ADD CONSTRAINT ota_packages_release_id_fkey
    FOREIGN KEY (release_id) REFERENCES releases(id) ON DELETE CASCADE;
ALTER TABLE updates DROP CONSTRAINT IF EXISTS updates_release_id_fkey;
ALTER TABLE updates ADD CONSTRAINT updates_release_id_fkey
    FOREIGN KEY (release_id) REFERENCES releases(id) ON DELETE CASCADE;

-- Retire the deployed flag entirely: a device is "deployed" iff it is assigned to a
-- restaurant (devices.restaurant_id IS NOT NULL). No more per-device / per-group /
-- per-restaurant deployed booleans.
ALTER TABLE devices     DROP COLUMN IF EXISTS deployed;
ALTER TABLE groups      DROP COLUMN IF EXISTS deployed;
ALTER TABLE restaurants DROP COLUMN IF EXISTS deployed;

-- Versions seen on devices that ops has dismissed from the releases list (not worth
-- tracking as a managed release). Keyed by the reported version string; only affects
-- not-tracked versions — a managed release uses releases.hidden instead.
CREATE TABLE IF NOT EXISTS hidden_versions (
    version    TEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Manual drag order for the releases list, keyed by version string (covers both managed
-- releases and not-tracked device versions). Versions absent here sort alphabetically
-- after the positioned ones. Lower position = higher in the list.
CREATE TABLE IF NOT EXISTS version_order (
    version  TEXT PRIMARY KEY,
    position INTEGER NOT NULL
);

-- Test team (QA): widen the user role check to allow a 'operator' account. Drop-then-add
-- keeps it idempotent across restarts (the inline constraint is auto-named users_role_check).
-- NOTE: this re-ADD re-validates every existing row on every boot, so its role list
-- must contain EVERY role that can exist in the table — including ones added by later
-- statements (e.g. 'dev' below). A narrower list here fails validation against a row a
-- later statement legitimately allows, crashing the migration. Keep this the full set.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD  CONSTRAINT users_role_check CHECK (role IN ('viewer','support','operator','fleet_manager','fleet_admin','tester','user_manager','super_op','dev','admin','owner','staff'));

-- Test cases: a reusable library plus per-release cases. base=true cases are checked on
-- every release; base=false cases belong to one release (release_id set). active=false
-- archives a case without deleting its history.
CREATE TABLE IF NOT EXISTS test_cases (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title           TEXT NOT NULL,
    area            TEXT NOT NULL DEFAULT '',
    steps           TEXT NOT NULL DEFAULT '',
    expected_result TEXT NOT NULL DEFAULT '',
    base            BOOLEAN NOT NULL DEFAULT false,
    release_id      INTEGER REFERENCES releases(id) ON DELETE CASCADE,
    active          BOOLEAN NOT NULL DEFAULT true,
    created_by      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_test_cases_release ON test_cases(release_id);

-- Per-release test outcomes. The applicable checklist is computed (active base cases plus
-- this release's own cases); a row is written only once an operator records a status, so a
-- fresh release starts all-untested by absence.
CREATE TABLE IF NOT EXISTS release_test_results (
    release_id   INTEGER NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
    test_case_id UUID    NOT NULL REFERENCES test_cases(id) ON DELETE CASCADE,
    status       TEXT    NOT NULL DEFAULT 'untested', -- untested|pass|fail|blocked|skip
    notes        TEXT    NOT NULL DEFAULT '',
    tested_by    TEXT    NOT NULL DEFAULT '',
    tested_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (release_id, test_case_id)
);

-- When true, the release's QA checklist skips base (standard) cases and only its
-- own release-specific cases apply.
ALTER TABLE releases ADD COLUMN IF NOT EXISTS skip_base_tests BOOLEAN NOT NULL DEFAULT false;

-- 'dev' role: full operational access (releases, OTA, devices, …) but NOT settings
-- or user management; it is also the only role allowed to sign off a release.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD  CONSTRAINT users_role_check CHECK (role IN ('viewer','support','operator','fleet_manager','fleet_admin','tester','user_manager','super_op','dev','admin','owner','staff'));

-- Dev sign-off on a release ("smoke-tested by dev, OK for QA to pick up").
ALTER TABLE releases ADD COLUMN IF NOT EXISTS signed_off_by TEXT        NOT NULL DEFAULT '';
ALTER TABLE releases ADD COLUMN IF NOT EXISTS signed_off_at TIMESTAMPTZ;

-- Force the full OTA image for this device on its deployment, bypassing any
-- matching incremental. Set when an incremental fails (a block diff can't apply
-- to a source image that isn't byte-identical — update_engine error 29) or when
-- an operator retries a failed device, so the guaranteed-applicable full image
-- is served instead of re-attempting the same failing incremental.
ALTER TABLE update_devices ADD COLUMN IF NOT EXISTS force_full BOOLEAN NOT NULL DEFAULT false;
-- allow_wipe: this deployment may hand a device the release's wipe (factory-reset) full
-- image when it has no incremental for the device's build. Off unless asked for.
ALTER TABLE updates ADD COLUMN IF NOT EXISTS allow_wipe BOOLEAN NOT NULL DEFAULT false;

-- Per-channel alert-type allowlist. NULL = deliver all types (back-compat); a
-- non-empty array restricts the channel to those alert type keys.
ALTER TABLE alert_channels ADD COLUMN IF NOT EXISTS alert_types TEXT[];

-- When the reboot that applies an installed OTA was pushed to the device (manual
-- "Reboot to apply", bulk reboot-all, or the automatic/scheduled path). Shown on the
-- deployment page next to each installed device. Backfilled once from the reboot
-- command that followed the install for rows that predate the column.
ALTER TABLE update_devices ADD COLUMN IF NOT EXISTS reboot_sent_at TIMESTAMPTZ;

-- MDM OTA support per firmware build. Builds older than the client that could
-- actually apply an A/B update can be managed by the MDM but only update over the
-- legacy otautil path, so pushing them an MDM OTA just wastes a download. A row
-- here is an explicit answer for one (build, product); everything else falls back
-- to the per-product cutoff release in config. source records where the answer came
-- from: 'manual' (an admin said so), 'observed' (a device on that build completed an
-- MDM OTA — proof), 'reported' (the client advertised its capabilities).
CREATE TABLE IF NOT EXISTS ota_support (
	build_id   TEXT        NOT NULL,
	product    TEXT        NOT NULL,
	supported  BOOLEAN     NOT NULL,
	source     TEXT        NOT NULL DEFAULT 'manual',
	note       TEXT        NOT NULL DEFAULT '',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (build_id, product)
);
-- The build a device was on when the rollout targeted it, so a completed install is
-- evidence that THAT build can apply an MDM OTA (the device now reports the new one).
ALTER TABLE update_devices ADD COLUMN IF NOT EXISTS source_build_id TEXT NOT NULL DEFAULT '';
UPDATE update_devices ud SET reboot_sent_at = sub.at
FROM (
	SELECT ud2.update_id, ud2.device_id, MIN(c.created_at) AS at
	FROM update_devices ud2
	JOIN command_targets ct ON ct.target_id = ud2.device_id
	JOIN commands c ON c.id = ct.command_id AND c.type = 'reboot' AND c.target_type = 'devices'
	WHERE ud2.reboot_sent_at IS NULL
	  AND ud2.status IN ('reboot_sent', 'installed')
	  AND ud2.completed_at IS NOT NULL
	  AND c.created_at >= ud2.completed_at - INTERVAL '2 minutes'
	  AND c.created_at <= ud2.completed_at + INTERVAL '7 days'
	GROUP BY ud2.update_id, ud2.device_id
) sub
WHERE ud.update_id = sub.update_id AND ud.device_id = sub.device_id AND ud.reboot_sent_at IS NULL;

-- Friendly device names ("Table 4", "Bar", "Host stand") for the restaurant owner
-- view and anywhere a serial reads badly. Separate table so no device scan changes.
CREATE TABLE IF NOT EXISTS device_nicknames (
	device_id UUID PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
	name      TEXT NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Per-user access policy (operators: which actions on which device groups /
-- restaurants; viewers: which devices they see). See db.AccessPolicy.
ALTER TABLE users ADD COLUMN IF NOT EXISTS access JSONB NOT NULL DEFAULT '{}';

-- Access grants (v2). One row per (user, effect, scope, actions). Scope rows point
-- at real records so a deleted venue / group / device takes its grants with it.
-- Base + hide_out_of_scope stay in users.access; rules there are migrated once.
CREATE TABLE IF NOT EXISTS access_grants (
	id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	effect        TEXT NOT NULL CHECK (effect IN ('allow','deny')),
	scope_type    TEXT NOT NULL CHECK (scope_type IN ('all','restaurant','group','device')),
	restaurant_id UUID REFERENCES restaurants(id) ON DELETE CASCADE,
	group_id      UUID REFERENCES groups(id) ON DELETE CASCADE,
	device_id     UUID REFERENCES devices(id) ON DELETE CASCADE,
	actions       TEXT[] NOT NULL,
	note          TEXT NOT NULL DEFAULT '',
	expires_at    TIMESTAMPTZ,
	created_by    TEXT NOT NULL DEFAULT '',
	created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	CHECK ((scope_type = 'all') = (restaurant_id IS NULL AND group_id IS NULL AND device_id IS NULL)),
	CHECK (scope_type <> 'restaurant' OR restaurant_id IS NOT NULL),
	CHECK (scope_type <> 'group' OR group_id IS NOT NULL),
	CHECK (scope_type <> 'device' OR device_id IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS access_grants_user_idx ON access_grants (user_id);
CREATE INDEX IF NOT EXISTS access_grants_device_idx ON access_grants (device_id) WHERE device_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS access_grants_restaurant_idx ON access_grants (restaurant_id) WHERE restaurant_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS access_grants_group_idx ON access_grants (group_id) WHERE group_id IS NOT NULL;

-- Attribution normalisation. Early rows snapshotted the sender's DISPLAY NAME on
-- commands.created_by / audit_log.actor; everything now keys on the username, so
-- those rows never matched their account (a merged user showed 1 command sent
-- against 125 "command.send" audit rows). Re-point them wherever a current
-- account's display name matches. Idempotent: rows already holding a username
-- don't match a display name.
UPDATE commands c SET created_by = u.username FROM users u
WHERE c.created_by <> '' AND c.created_by <> u.username
  AND lower(c.created_by) = lower(trim(u.first_name || ' ' || u.last_name));
UPDATE audit_log a SET actor = u.username FROM users u
WHERE a.actor <> '' AND a.actor <> u.username
  AND lower(a.actor) = lower(trim(u.first_name || ' ' || u.last_name));
-- A merged account (Microsoft sign-in replacing an older login) inherits history
-- older than itself: its "joined" date becomes its earliest recorded activity.
UPDATE users u SET created_at = LEAST(u.created_at, s.first_at)
FROM (
	SELECT actor AS username, MIN(created_at) AS first_at FROM audit_log WHERE actor <> '' GROUP BY actor
	UNION ALL
	SELECT created_by, MIN(created_at) FROM commands WHERE created_by <> '' GROUP BY created_by
) s
WHERE s.username = u.username AND s.first_at < u.created_at;

-- One row per build change per device (from → to, when). Written at check-in when
-- the reported build differs from the stored one; backfilled from checkins by the
-- hourly housekeeping (newest day first). The device graph's build markers read
-- this instead of scanning check-in history.
CREATE TABLE IF NOT EXISTS device_build_history (
	device_id  UUID        NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	at         TIMESTAMPTZ NOT NULL,
	from_build TEXT        NOT NULL,
	to_build   TEXT        NOT NULL,
	PRIMARY KEY (device_id, at)
);

-- Retired alert types: battery-health (untrusted sysfs proxy), Wi-Fi disconnect
-- counting, and the app/kiosk behavioural rules. Purge any rows seeded before they
-- were removed from the code so existing DBs match a fresh seed. Idempotent.
DELETE FROM alerts WHERE type IN (
	'battery_health_decline','battery_health_low','battery_health_critical',
	'battery_cycles_high','wifi_disconnects','app_not_foreground','kiosk_disabled',
	'app_crash','app_anr',
	'no_overnight_charge','soc_low_service','soc_low_guest_charging',
	'overnight_not_charging','overnight_slow_charge','discharge_rate_idle','discharge_rate_active',
	'pad_disconnected','pad_unused','unexpected_reboot');
DELETE FROM alert_rules WHERE type IN (
	'battery_health_decline','battery_health_low','battery_health_critical',
	'battery_cycles_high','wifi_disconnects','app_not_foreground','kiosk_disabled',
	'app_crash','app_anr',
	'no_overnight_charge','soc_low_service','soc_low_guest_charging',
	'overnight_not_charging','overnight_slow_charge','discharge_rate_idle','discharge_rate_active',
	'pad_disconnected','pad_unused','unexpected_reboot');

-- Scrub removed alert types from per-channel allowlists so the "Alert types"
-- selector count reflects the lean set (a channel that had "select all" stored the
-- full old list). array_agg over the filtered elements yields NULL when nothing is
-- left, which the dispatcher treats as "all current types" — the intended default.
UPDATE alert_channels SET alert_types = (
	SELECT array_agg(t) FROM unnest(alert_types) AS t
	WHERE t NOT IN (
		'battery_health_decline','battery_health_low','battery_health_critical',
		'battery_cycles_high','wifi_disconnects','app_not_foreground','kiosk_disabled',
		'app_crash','app_anr',
		'no_overnight_charge','soc_low_service','soc_low_guest_charging',
		'overnight_not_charging','overnight_slow_charge','discharge_rate_idle','discharge_rate_active',
		'pad_disconnected','pad_unused','unexpected_reboot')
) WHERE alert_types IS NOT NULL;

-- offline is a fleet-wide device-health alert (fires on lab/bench units too), not a
-- deployed-only operational rule. It self-suppresses overnight via its own quiet
-- window (quiet_start/quiet_end), so the service window is unnecessary. Flip legacy
-- rows off the old service-window default so they stop being gated to deployed units.
UPDATE alert_rules SET active_window = 'always' WHERE type = 'offline' AND active_window = 'service';

-- Alert occurrence tracking: count re-fires of the same open (type, device) condition
-- instead of suppressing them silently, and record when the condition was last seen.
ALTER TABLE alerts ADD COLUMN IF NOT EXISTS occurrences INTEGER NOT NULL DEFAULT 1;
ALTER TABLE alerts ADD COLUMN IF NOT EXISTS last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- Episode folding. An alert used to be one row per episode: a condition that cleared and
-- returned produced a fresh row, so a device hovering on a threshold made dozens a day and
-- "Clear all" simply deleted rows the evaluator re-created within the minute.
--   cleared_at  the condition stopped holding at this time, but the row is not resolved
--               yet — it waits out the clear window, so a brief dip does not end an episode
--               (and a return simply clears the stamp again).
--   muted_until suppress this (type, device) until then. Set by flap detection, and by
--               "Clear all", which now means resolve-and-snooze rather than delete.
ALTER TABLE alerts ADD COLUMN IF NOT EXISTS cleared_at  TIMESTAMPTZ;
ALTER TABLE alerts ADD COLUMN IF NOT EXISTS muted_until TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_alerts_recent_by_device ON alerts (type, device_id, fired_at DESC);

-- QFIL flashing packages. Each release can carry one or more QFIL bundles
-- (Qualcomm Flash Image Loader packages used to flash a device from scratch over
-- USB). A dev or admin attaches them per release; the test team reads them off the
-- release page when flashing units. The bundle itself lives at an external URL
-- (like an OTA package), not in the DB.
CREATE TABLE IF NOT EXISTS qfil_packages (
    id          SERIAL      PRIMARY KEY,
    release_id  INTEGER     NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
    label       TEXT        NOT NULL DEFAULT '',
    url         TEXT        NOT NULL,
    notes       TEXT        NOT NULL DEFAULT '',
    added_by    TEXT        NOT NULL DEFAULT '',
    status      TEXT        NOT NULL DEFAULT 'active',   -- active|removed
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_qfil_packages_release ON qfil_packages(release_id);

-- Command recipes: saved action presets (type + payload + target) that the
-- Actions builder can replay in one click. target_serials / target_groups
-- snapshot the intended target so a recipe survives group/device churn as a
-- best-effort prefill (unknown serials are simply dropped when applied).
CREATE TABLE IF NOT EXISTS command_recipes (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name           TEXT NOT NULL,
    type           TEXT NOT NULL,
    apk_url        TEXT NOT NULL DEFAULT '',
    payload        JSONB NOT NULL DEFAULT '{}',
    target_type    TEXT NOT NULL DEFAULT 'all',
    target_serials TEXT[] NOT NULL DEFAULT '{}',
    target_groups  UUID[] NOT NULL DEFAULT '{}',
    created_by     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- scope target spec ({mode,id,status,battery,kiosk}) when target_type='scope', so a
-- scheduled recipe can re-resolve its scope-rail selection at run time.
ALTER TABLE command_recipes ADD COLUMN IF NOT EXISTS scope JSONB NOT NULL DEFAULT '{}';

-- Scheduled recipes: run a saved recipe on a cron; the target is re-resolved from the
-- recipe's spec each time it fires (so group/scope membership is current, not snapshot).
CREATE TABLE IF NOT EXISTS scheduled_recipes (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    recipe_id   UUID NOT NULL REFERENCES command_recipes(id) ON DELETE CASCADE,
    cron_expr   TEXT NOT NULL,
    enabled     BOOLEAN NOT NULL DEFAULT true,
    run_once    BOOLEAN NOT NULL DEFAULT false,  -- disable after the first successful run
    next_run_at TIMESTAMPTZ,                     -- NULL = not scheduled (paused/expired)
    last_run_at TIMESTAMPTZ,
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_scheduled_recipes_due ON scheduled_recipes(next_run_at) WHERE enabled;

-- Commands an operator has "cleared" from the Needs-attention triage list. This
-- only dismisses them from that view — the command and its delivery records are
-- untouched (it still appears in Completed / history). ON DELETE CASCADE keeps it
-- tidy if the command is ever actually deleted.
CREATE TABLE IF NOT EXISTS dismissed_commands (
    command_id   UUID PRIMARY KEY REFERENCES commands(id) ON DELETE CASCADE,
    dismissed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    dismissed_by TEXT NOT NULL DEFAULT ''
);

-- Performance indexes (2026 audit). All idempotent. These back hot filters/joins/
-- sorts that previously fell back to sequential scans on larger fleets.
CREATE INDEX IF NOT EXISTS idx_command_status_device_id ON command_status(device_id);
CREATE INDEX IF NOT EXISTS idx_devices_build_id         ON devices(build_id);
CREATE INDEX IF NOT EXISTS idx_device_groups_device_id  ON device_groups(device_id);
CREATE INDEX IF NOT EXISTS idx_device_config_device_id  ON device_config(device_id);
CREATE INDEX IF NOT EXISTS idx_ota_packages_release     ON ota_packages(release_id);
CREATE INDEX IF NOT EXISTS idx_updates_release          ON updates(release_id);
CREATE INDEX IF NOT EXISTS idx_update_devices_device_id ON update_devices(device_id);
CREATE INDEX IF NOT EXISTS idx_commands_created_at      ON commands(created_at DESC);
-- ListCommandsSince filters "type != 'update_splash' AND created_at >= ..." together;
-- the single-column indexes above/below can only be used one at a time (or bitmap-AND'd,
-- weaker than a composite). Covers that filter directly once the table is large.
CREATE INDEX IF NOT EXISTS idx_commands_created_at_type  ON commands(created_at DESC, type);
CREATE INDEX IF NOT EXISTS idx_alerts_fired_at          ON alerts(fired_at DESC);
CREATE INDEX IF NOT EXISTS idx_device_daily_stats_device_day ON device_daily_stats(device_id, day DESC);

-- Interim install progress: a device reports 'downloading' (with a percent) then
-- 'installing' before the terminal 'installed'/'failed'. progress is 0-100 while
-- downloading, NULL otherwise.
ALTER TABLE command_status ADD COLUMN IF NOT EXISTS progress SMALLINT;

-- received_at is stamped when the DEVICE confirms it received the command (a client
-- 'received' ack), as opposed to 'delivered' which only means the frame was enqueued
-- onto the socket. A non-NULL received_at means re-delivery/redrive must stop — the
-- device has it. NULL on old clients (no receipt ack) → the redrive falls back to its
-- best-effort staleness heuristic.
ALTER TABLE command_status ADD COLUMN IF NOT EXISTS received_at TIMESTAMPTZ;

-- Learned APK-URL → package-name map. Populated when a device acks an install and
-- reports which package the APK produced. Lets the server reconcile a pending
-- install against a device's reported package list (clears a stuck "installing" the
-- moment the app is actually present, even if the terminal ack was lost).
CREATE TABLE IF NOT EXISTS apk_packages (
	apk_url      TEXT PRIMARY KEY,
	package_name TEXT NOT NULL,
	updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Package name (e.g. com.example.app) an admin records when adding a repository
-- app. When set, it seeds apk_packages immediately so an install can skip devices
-- that already have the app — no need to wait for a first install to "learn" it.
ALTER TABLE apps ADD COLUMN IF NOT EXISTS package_name TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN IF NOT EXISTS s3_key TEXT NOT NULL DEFAULT '';
-- versionName parsed from the APK manifest at upload — shown in the App Library and
-- everywhere the app picker appears (Actions, device page) so an operator can tell
-- which build they're about to install. Re-uploading the same package still creates a
-- new row (no version-aware replace flow yet), so this alone doesn't dedup.
ALTER TABLE apps ADD COLUMN IF NOT EXISTS version_name TEXT NOT NULL DEFAULT '';

-- Freeform operator notes on a device (e.g. "cracked screen", "reserved for QA").
ALTER TABLE devices ADD COLUMN IF NOT EXISTS notes TEXT NOT NULL DEFAULT '';

-- Links an auto-triggered logcat capture to the alert that requested it (e.g. a
-- device_crash alert grabs the device's error logs). NULL for manual captures.
ALTER TABLE logcat_requests ADD COLUMN IF NOT EXISTS alert_id UUID;

-- Structured problem reports filed by operators against a release. A problem is
-- release-scoped (auto-links the build), optionally references a specific test
-- case and the device it was seen on, carries a severity + lifecycle status, and
-- holds freeform detail (repro steps, pasted logs). This is the tracked record
-- behind a QA "fail" so problems can be handed off, fixed and re-verified.
CREATE TABLE IF NOT EXISTS release_problems (
	id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	release_id   INTEGER NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
	test_case_id UUID REFERENCES test_cases(id) ON DELETE SET NULL,
	device_id    UUID REFERENCES devices(id) ON DELETE SET NULL,
	build_id     TEXT NOT NULL DEFAULT '',
	title        TEXT NOT NULL,
	description  TEXT NOT NULL DEFAULT '',
	severity     TEXT NOT NULL DEFAULT 'major',   -- blocker | major | minor
	status       TEXT NOT NULL DEFAULT 'open',    -- open | fixed | verified | wontfix
	reported_by  TEXT NOT NULL DEFAULT '',
	created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_release_problems_release ON release_problems(release_id);
-- How a release problem was filed: 'manual' (operator filed it) or 'qa' (auto-created
-- when a QA test case was marked failed). One 'qa' problem per (release, test_case).
-- (Must run AFTER the CREATE TABLE above — the migration executes as one ordered
-- script, so a fresh database has no release_problems table until this point.)
ALTER TABLE release_problems ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'manual';

-- Full DropBox crash/ANR/tombstone body (the stack trace) for a crash event, so the
-- crash alert and release page can show the real diagnostic, not just the headline.
ALTER TABLE device_events ADD COLUMN IF NOT EXISTS detail TEXT NOT NULL DEFAULT '';
-- The build the device was running when it reported the event, so a crash is
-- attributed to the build it happened on (not whatever the device later updates
-- to). CrashesOnBuild filters on this. One-time backfill: attribute existing crash
-- events from the nearest prior check-in. Self-limiting (only un-attributed rows).
ALTER TABLE device_events ADD COLUMN IF NOT EXISTS build_id TEXT NOT NULL DEFAULT '';
-- (Its one-time backfill from check-in history ran long ago and went with checkins.)

-- Release-problem continuity ("rides the release train"): a manual bug is a thread that
-- follows the releases forward until an operator verifies it fixed. fixed_in_release_id is
-- the build a dev claims the fix landed in; verified_in_release_id is the build an operator
-- confirmed it on. Origin stays in release_id ("reported in"). Prior/next ordering uses
-- releases.created_at chronology — the manual version_order table is display-only.
ALTER TABLE release_problems ADD COLUMN IF NOT EXISTS fixed_in_release_id    INTEGER REFERENCES releases(id) ON DELETE SET NULL;
ALTER TABLE release_problems ADD COLUMN IF NOT EXISTS verified_in_release_id INTEGER REFERENCES releases(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_release_problems_fixed_in ON release_problems(fixed_in_release_id);

-- Release lifecycle finish line: "testing done" moves a release from active to inactive.
-- A release is ACTIVE (under test — shown in the hub focus band) while testing_done_at is
-- NULL; marking testing complete stamps it, retiring the release from the active slot so
-- the next build takes over. This is the real end of the cycle — dev sign-off is only an
-- intermediate gate, not the finish.
ALTER TABLE releases ADD COLUMN IF NOT EXISTS testing_done_at TIMESTAMPTZ;
ALTER TABLE releases ADD COLUMN IF NOT EXISTS testing_done_by TEXT NOT NULL DEFAULT '';

-- Branch builds: an off-mainline, temporary test build forked from a release (like a git
-- branch off main). parent_release_id is what it forked from; is_branch marks it so it's
-- kept out of the mainline path — it never becomes the active release, never joins the
-- release train, and its problems are fully isolated (no carry-forward either direction,
-- absent from the cross-release board). It can still hold packages + deploy like any
-- release. Nested under its parent in the UI.
ALTER TABLE releases ADD COLUMN IF NOT EXISTS parent_release_id INTEGER REFERENCES releases(id) ON DELETE SET NULL;
ALTER TABLE releases ADD COLUMN IF NOT EXISTS is_branch BOOLEAN NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS idx_releases_parent ON releases(parent_release_id);

-- Merge a validated branch back onto the main line. Merging creates a NEW mainline release
-- (the next version) that carries the branch's delta; the branch is then closed out.
-- merged_from_release_id is set on the new mainline node (the branch it absorbed);
-- merged_into_release_id is set on the branch (where it landed). A branch with
-- merged_into_release_id set is terminal ("merged") and offers no further merge.
-- Dev releases: a build being worked on is visible only to devs and admins, so a
-- half-finished release never shows up on an operator's Updates page or in a push
-- picker. New releases created from the UI start dev and are published to everyone
-- by turning the flag off. The column defaults to false so existing releases — and
-- any release auto-created from a build a device reports — stay visible as before.
ALTER TABLE releases ADD COLUMN IF NOT EXISTS is_dev BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE releases ADD COLUMN IF NOT EXISTS merged_from_release_id INTEGER REFERENCES releases(id) ON DELETE SET NULL;
ALTER TABLE releases ADD COLUMN IF NOT EXISTS merged_into_release_id INTEGER REFERENCES releases(id) ON DELETE SET NULL;
ALTER TABLE releases ADD COLUMN IF NOT EXISTS merged_at TIMESTAMPTZ;
ALTER TABLE releases ADD COLUMN IF NOT EXISTS merged_by TEXT NOT NULL DEFAULT '';

-- A test case can be tied to the release problem it verifies. When a dev marks a
-- carried-over bug "fixed here", a "Verify fix: …" case is added to the release's QA
-- checklist (problem_id set); passing that case verifies the linked problem. One such
-- case per (release, problem).
ALTER TABLE test_cases ADD COLUMN IF NOT EXISTS problem_id UUID REFERENCES release_problems(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_test_cases_problem ON test_cases(problem_id);

-- Retire the offline_long alert: it duplicated the offline alert on every offline
-- device. Drop its rule and resolve any open ones so they clear from the inbox.
DELETE FROM alert_rules WHERE type = 'offline_long';
UPDATE alerts SET status = 'resolved', resolved_at = NOW(), updated_at = NOW()
    WHERE type = 'offline_long' AND status <> 'resolved';

-- Perf: a fingerprint of the device's installed-app set. Lets UpsertDevicePackages
-- skip the (expensive) full delete+reinsert on the >99% of check-ins where the app
-- list is unchanged.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS packages_hash TEXT;

-- Perf: indexes for hot query paths found in the scale audit.
-- commands.type is filtered on every check-in (pending-command / OTA / install checks).
CREATE INDEX IF NOT EXISTS idx_commands_type ON commands(type);
-- command_targets is probed by target_id (device) alone; the PK leads with command_id.
CREATE INDEX IF NOT EXISTS idx_command_targets_target ON command_targets(target_id);
-- Retention prunes DELETE these by created_at; without an index they full-scan a wide table.
CREATE INDEX IF NOT EXISTS idx_logcat_results_created_at ON logcat_results(created_at);
CREATE INDEX IF NOT EXISTS idx_logcat_requests_created_at ON logcat_requests(created_at);
-- NOTE: JSONB expression indexes for the device-list RAM%/temperature sorts were
-- considered and rejected — at ~900 devices the planner picks a seq-scan+sort anyway
-- (measured), and the index would have to be maintained on latest_extra on EVERY
-- check-in. Revisit only if the fleet grows past several thousand devices.

-- Battery-cycle accuracy: per-day TRUE discharge (sum of battery DROPS only, ignoring
-- charge climbs), so cycles reflect actual wear rather than the day's SoC range (which
-- counted charging as wear). NULL on pre-existing days → the cycle recompute falls back
-- to the old max-min estimate for history and uses this exact value going forward.
ALTER TABLE device_daily_stats ADD COLUMN IF NOT EXISTS discharge_pct REAL;
-- The old range-based cumulative, kept as a separate "legacy" figure during the
-- transition so operators can compare the corrected number against the prior estimate.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS discharge_legacy_pct BIGINT NOT NULL DEFAULT 0;

-- Multi-product support: the hardware category the device reports on check-in
-- (e.g. 't7', 'kiosk27'). Empty means the device predates the field / didn't report;
-- capability resolution treats empty as the default product (T7). Drives capability
-- gating (wlc/charging) and dashboard grouping/filtering. See internal/product.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS product TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_devices_product ON devices(product);

-- (Historical) Operator role retired: folded any remaining operator accounts down
-- to viewer, back when 'operator' meant the OLD role being retired here. That
-- one-time job finished long ago. The statement is now REMOVED — 'operator' was
-- later reused as the renamed 'tester' role (see the tester->operator migration
-- below), and this unconditional UPDATE was unconditionally re-running on every
-- boot, silently demoting every real operator account back to viewer on each
-- redeploy. Do not re-add a bare "role='operator' -> role='viewer'" statement.

-- Device diagnostics catalog: admin-curated read-only device queries surfaced as
-- friendly "retrieve property" buttons (e.g. getprop ro.build.id, dumpsys battery).
-- Each runs as an ordinary shell command whose text comes only from this catalog,
-- so operators can run vetted queries without raw shell access.
CREATE TABLE IF NOT EXISTS device_queries (
	id          SERIAL PRIMARY KEY,
	label       TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	command     TEXT NOT NULL,
	category    TEXT NOT NULL DEFAULT 'General',
	sort        INTEGER NOT NULL DEFAULT 0,
	enabled     BOOLEAN NOT NULL DEFAULT TRUE,
	created_by  TEXT NOT NULL DEFAULT '',
	created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Seed a starter set, but only while the catalog is empty, so an admin's later
-- edits/deletes are never undone by a redeploy.
INSERT INTO device_queries (label, description, command, category, sort)
SELECT * FROM (VALUES
	('Build ID',        'Full build fingerprint id',        'getprop ro.build.id',                'System',  10),
	('Android version', 'Android release version',          'getprop ro.build.version.release',   'System',  20),
	('Model',           'Hardware model name',              'getprop ro.product.model',           'System',  30),
	('Serial number',   'Hardware serial',                  'getprop ro.serialno',                'System',  40),
	('Battery',         'Battery service dump',             'dumpsys battery',                    'Power',   50),
	('Storage (data)',  'Free space on /data',              'df /data',                           'Storage', 60),
	('Uptime',          'How long the device has been up',  'uptime',                             'System',  70),
	('Wi-Fi',           'Current Wi-Fi connection',         'dumpsys wifi | grep -i "mWifiInfo"', 'Network', 80)
) AS v(label, description, command, category, sort)
WHERE NOT EXISTS (SELECT 1 FROM device_queries);

-- Self-service sign-up + password reset: email identity/verification on users,
-- plus a generic single-use token table shared by both flows.
ALTER TABLE users ADD COLUMN IF NOT EXISTS email TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verified_at TIMESTAMPTZ;
CREATE UNIQUE INDEX IF NOT EXISTS users_email_unique ON users (lower(email)) WHERE email IS NOT NULL;

-- Profile pictures: a small PNG stored inline (≤128px, a few KB); avatar_updated_at
-- doubles as the cache-busting version in avatar URLs.
ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar BYTEA;
ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar_updated_at TIMESTAMPTZ;

-- Device page view per user (16 Oct): '' is the classic page (the default), 'new' the
-- revamped one. Set from the page's own switch and kept across sessions.
ALTER TABLE users ADD COLUMN IF NOT EXISTS device_view TEXT NOT NULL DEFAULT '';

-- (Historical) The dev role was once retired here with "DELETE FROM users WHERE
-- role = 'dev'". Dev is a real role again (operator + releases + OTA), and this
-- script runs on every boot, so that statement would have erased every dev
-- account at each restart. Removed; nothing runs here any more.

-- NOTE: 'operator' is included here (unlike the original historical wording) even
-- though this statement's own job (dropping dev accounts) has nothing to do with
-- it — this ALTER re-validates every existing row on every boot, so, per the same
-- rule noted above, its list must contain every role that legitimately exists in
-- the table by this point, including 'operator' rows that persist from the
-- tester->operator rename further down. Omitting it here breaks every redeploy.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD  CONSTRAINT users_role_check CHECK (role IN ('viewer','support','operator','fleet_manager','fleet_admin','tester','user_manager','super_op','dev','admin','owner','staff'));

CREATE TABLE IF NOT EXISTS user_tokens (
    token      TEXT PRIMARY KEY,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    purpose    TEXT NOT NULL CHECK (purpose IN ('verify','reset')),
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS user_tokens_user_id_idx ON user_tokens (user_id);

-- "Tester" role renamed to "operator": there's no separate testing workflow left
-- in MDM, and "operator" is already the term used everywhere else (canOperate,
-- requireOperatorOrAdmin) for this exact permission tier, so the role label was
-- just out of sync with the concept. Existing tester accounts keep their
-- permissions, just relabeled. The constraint must allow BOTH values while the
-- UPDATE is converting rows — the currently-active constraint at this point in
-- the script is (viewer,tester), which would reject the UPDATE outright.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD  CONSTRAINT users_role_check CHECK (role IN ('viewer','support','operator','fleet_manager','fleet_admin','tester','user_manager','super_op','dev','admin','owner','staff'));
UPDATE users SET role = 'operator' WHERE role = 'tester';
-- Role ladder: admin (super admin) → dev → user_manager → operator → viewer.
-- Admins and devs may be real accounts too (e.g. Microsoft sign-in), not only
-- the env login. This is the effective constraint; keep it last.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD  CONSTRAINT users_role_check CHECK (role IN ('viewer','support','operator','fleet_manager','fleet_admin','tester','user_manager','super_op','dev','admin','owner','staff'));

-- First/last name, settable at sign-up and editable by an admin afterward so the
-- dashboard and audit trail can show a real name instead of a bare email address.
ALTER TABLE users ADD COLUMN IF NOT EXISTS first_name TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS last_name  TEXT NOT NULL DEFAULT '';

-- Attribution: which user pushed a given command, shown on the Actions page and
-- a device's queue/history. A snapshot of the display name/email at creation time
-- (not a user_id FK) so it survives the user account later being renamed or deleted.
-- Empty for system/scheduler-initiated commands (auto-reboot, redrive).
ALTER TABLE commands ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT '';

-- Per-user dashboard layouts (widget order / hidden widgets / chosen preset), keyed
-- by username + page so a user's Overview arrangement follows them across devices.
-- Keyed by username rather than users.id because the built-in admin session has no
-- users row. Absent row = the page's default layout.
CREATE TABLE IF NOT EXISTS user_layouts (
    username   TEXT NOT NULL,
    page       TEXT NOT NULL,
    layout     JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (username, page)
);

-- Enrollment profiles: a named, revocable "enr_..." token that a provisioning QR (or the
-- onboarding screen) carries. A device exchanges it once at POST /api/v1/enroll for its own
-- per-device API key; the profile can auto-join enrolled devices to a group.
CREATE TABLE IF NOT EXISTS enrollment_profiles (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name         TEXT NOT NULL,
    token        TEXT NOT NULL UNIQUE,
    group_id     UUID REFERENCES groups(id) ON DELETE SET NULL,
    notes        TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at   TIMESTAMPTZ,
    enroll_count INT NOT NULL DEFAULT 0
);

-- Per-device credential issued at enrollment (SHA-256 hex of the "dvk_..." key). Devices
-- presenting it are identity-bound: the server derives the acting device from the key and
-- rejects mismatched client-supplied serials. NULL = legacy shared-key device.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS device_key_hash TEXT;
-- When an admin last reset the device's own key (device-key plan): set until it
-- registers a new one, so a device that never does can be flagged.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS key_reset_at TIMESTAMPTZ;
-- Where the device last used its own key, and when it was last let back in after losing
-- it (30 Sep: an OTA left three devices unable to read their key and locked out).
ALTER TABLE devices ADD COLUMN IF NOT EXISTS key_last_ip TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN IF NOT EXISTS key_auto_reset_at TIMESTAMPTZ;
-- The chip's own serial (Qualcomm soc0/serial_number, as hex), which a reflash cannot change.
-- hardware_serials is the map from it to every AIO serial it has been seen under, so a device
-- whose AIO serial was corrupted can still be recognised.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS hardware_serial TEXT;
CREATE INDEX IF NOT EXISTS idx_devices_hardware_serial ON devices(hardware_serial) WHERE hardware_serial IS NOT NULL;
CREATE TABLE IF NOT EXISTS hardware_serials (
    hardware_serial TEXT        NOT NULL,
    serial_number   TEXT        NOT NULL,
    first_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (hardware_serial, serial_number)
);
CREATE INDEX IF NOT EXISTS idx_hardware_serials_serial ON hardware_serials(serial_number);
ALTER TABLE devices ADD COLUMN IF NOT EXISTS enrolled_via UUID;
CREATE UNIQUE INDEX IF NOT EXISTS idx_devices_key_hash ON devices(device_key_hash) WHERE device_key_hash IS NOT NULL;

-- Kiosk modes: 'app' (single locked app — the classic kiosk_enabled/kiosk_package
-- pair), 'multi' (locked app plus kiosk_packages also allowed in lock-task), or
-- 'browser' (locked browser showing kiosk_url, navigation limited to the
-- kiosk_url_allow prefixes). The JSONB columns are arrays of strings.
ALTER TABLE device_config ADD COLUMN IF NOT EXISTS kiosk_mode TEXT NOT NULL DEFAULT 'app';
ALTER TABLE device_config ADD COLUMN IF NOT EXISTS kiosk_packages JSONB NOT NULL DEFAULT '[]';
ALTER TABLE device_config ADD COLUMN IF NOT EXISTS kiosk_url TEXT NOT NULL DEFAULT '';
ALTER TABLE device_config ADD COLUMN IF NOT EXISTS kiosk_url_allow JSONB NOT NULL DEFAULT '[]';

-- Circular geofences, evaluated server-side against the GPS fix agents report in
-- latest_extra (location_lat/location_lon, sent only while fleet policy
-- location_enabled is on). No membership table: inside/outside is computed live.
CREATE TABLE IF NOT EXISTS geofences (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL,
    lat        DOUBLE PRECISION NOT NULL,
    lon        DOUBLE PRECISION NOT NULL,
    radius_m   INT NOT NULL DEFAULT 200,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The first compliance rules (fleet-wide, evaluated on page load) were replaced on 15 Oct
-- by compliance_baseline / compliance_overrides / compliance_results below.
DROP TABLE IF EXISTS compliance_rules;

-- Compliance (static/compliance-plan.html; rules editor B1, picked 15 Oct). The fleet
-- baseline is one row per check (a check with no row uses its registry default);
-- overrides are ordered and the first that covers a device and names a check wins for
-- that check, as lock rules do. Results hold each device's current state per check and
-- change only when the state does; every change is an event, which is what history,
-- alerts and the venue report read.
CREATE TABLE IF NOT EXISTS compliance_baseline (
    check_id   TEXT PRIMARY KEY,
    level      TEXT NOT NULL,               -- required | recommended | off
    param      INT  NOT NULL DEFAULT 0,     -- the check's threshold (hours, days); 0 = its default
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- auto_fix (15 Oct): act on a failure where the check has a fix (see the registry).
ALTER TABLE compliance_baseline ADD COLUMN IF NOT EXISTS auto_fix BOOLEAN;
CREATE TABLE IF NOT EXISTS compliance_overrides (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    position   INT  NOT NULL DEFAULT 0,
    name       TEXT NOT NULL DEFAULT '',
    scope_type TEXT NOT NULL,               -- venue | group | role | unassigned | lab
    scope_id   UUID,                        -- the venue or group
    scope_key  TEXT NOT NULL DEFAULT '',    -- the role (device class)
    cap        TEXT NOT NULL DEFAULT '',    -- '' | recommended | off: a ceiling on every check
    checks     JSONB NOT NULL DEFAULT '{}', -- check id -> {"level": …, "param": …}
    created_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS compliance_results (
    device_id  UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    check_id   TEXT NOT NULL,
    state      TEXT NOT NULL,               -- pass | fail | unknown | na
    level      TEXT NOT NULL,               -- the level that applied
    detail     TEXT NOT NULL DEFAULT '',
    since      TIMESTAMPTZ NOT NULL DEFAULT NOW(), -- when the state last changed
    checked_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (device_id, check_id)
);
CREATE TABLE IF NOT EXISTS compliance_events (
    device_id  UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    check_id   TEXT NOT NULL,
    from_state TEXT NOT NULL,               -- '' for a device's first result
    to_state   TEXT NOT NULL,
    level      TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_compliance_events_device_at ON compliance_events(device_id, at DESC);
CREATE INDEX IF NOT EXISTS idx_compliance_events_at ON compliance_events(at);

-- Client-reported "has a launcher (app-drawer) entry" flag per package. NULL when
-- the client is too old to report it; the dashboard's app lists default to
-- launchable apps only (falling back to the full list when nothing reports the flag).
ALTER TABLE device_packages ADD COLUMN IF NOT EXISTS launchable BOOLEAN;

-- Mixed fleet (firmware devices + DPC-managed outsourced devices). Facts that used
-- to be re-derived from latest_extra on every request are persisted once at ingest:
--   agent_kind        'firmware' (system-app client, shared key) | 'dpc' (Device-Owner
--                     agent, per-device key). Set by EnrollDevice / UpsertCheckin.
--   device_class      form factor: tablet | panel | kiosk | mpos | pos | other. Empty =
--                     derive from product (firmware) or not set yet (DPC). Comes from the
--                     enrollment profile, overridable per device.
--   capabilities      JSON array of what the agent can do (extra.capabilities on a
--                     keyframe). Firmware devices leave it empty and get product defaults.
--   capabilities_degraded  same shape, features that work with limits.
--   enrollment_status auto (firmware first check-in) | enrolled (via profile) |
--                     retired | wiped.
--   enrolled_at / key_rotated_at / onboarded_at  lifecycle timestamps; onboarded_at is
--                     NULL while the device sits in the onboarding inbox.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS agent_kind TEXT NOT NULL DEFAULT 'firmware';
ALTER TABLE devices ADD COLUMN IF NOT EXISTS device_class TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN IF NOT EXISTS capabilities JSONB NOT NULL DEFAULT '[]';
ALTER TABLE devices ADD COLUMN IF NOT EXISTS capabilities_degraded JSONB NOT NULL DEFAULT '[]';
ALTER TABLE devices ADD COLUMN IF NOT EXISTS enrollment_status TEXT NOT NULL DEFAULT 'auto';
ALTER TABLE devices ADD COLUMN IF NOT EXISTS enrolled_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
ALTER TABLE devices ADD COLUMN IF NOT EXISTS key_rotated_at TIMESTAMPTZ;
ALTER TABLE devices ADD COLUMN IF NOT EXISTS onboarded_at TIMESTAMPTZ;
-- Custody: which MDM a device currently reports to. A device is moved between servers
-- by pointing persist.sys.mdm.url somewhere else — from the device page, by hand over
-- adb, or by flashing an image whose build.prop already carries the property — and the
-- server it left has no way to tell that from an outage. These columns hold the answer,
-- and are cleared the moment the device checks in here again.
--   custody_server   the peer's name ('' = ours)
--   custody_url      the peer's base URL, for the link out
--   custody_seen_at  the peer's last_seen for it; what makes this a fact, not a guess
--   custody_source   how we learned: move | peer | sweep | client
--   custody_set_at   when this server recorded it
ALTER TABLE devices ADD COLUMN IF NOT EXISTS custody_server TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN IF NOT EXISTS custody_url TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN IF NOT EXISTS custody_seen_at TIMESTAMPTZ;
ALTER TABLE devices ADD COLUMN IF NOT EXISTS custody_source TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN IF NOT EXISTS custody_set_at TIMESTAMPTZ;

-- The last stored report's fingerprint, for UpsertCheckin's store-or-coalesce decision.
-- It used to be read back from the newest checkins row; that table is being retired.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS hist_at         TIMESTAMPTZ;
ALTER TABLE devices ADD COLUMN IF NOT EXISTS hist_battery    SMALLINT;
ALTER TABLE devices ADD COLUMN IF NOT EXISTS hist_build      TEXT;
ALTER TABLE devices ADD COLUMN IF NOT EXISTS hist_state_hash TEXT;

-- Check-ins refused for a corrupted serial (api.validSerial): who, from where, how often.
-- Nothing else of theirs is stored; this keeps such a tablet visible on the dashboard.
CREATE TABLE IF NOT EXISTS refused_checkins (
	serial    TEXT        NOT NULL,
	remote_ip TEXT        NOT NULL,
	build_id  TEXT        NOT NULL DEFAULT '',
	first_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	last_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	attempts  BIGINT      NOT NULL DEFAULT 1,
	PRIMARY KEY (serial, remote_ip)
);
-- The public address a device last reached the server from (NoteDeviceRemoteIP): places a
-- refused tablet by the devices already checking in from the same network.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS last_remote_ip TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_devices_last_remote_ip ON devices(last_remote_ip) WHERE last_remote_ip <> '';
CREATE INDEX IF NOT EXISTS idx_devices_custody ON devices(custody_server) WHERE custody_server <> '';

-- The peer outbox: an arrival is announced to every peer, and the announcement has to
-- survive the peer being down, a deploy, or the process dying mid-send. Rows are
-- claimed by the worker loop, retried with backoff, and dropped once delivered.
CREATE TABLE IF NOT EXISTS peer_outbox (
    id              BIGSERIAL PRIMARY KEY,
    peer            TEXT NOT NULL,
    serial          TEXT NOT NULL,
    payload         JSONB NOT NULL,
    attempts        INT NOT NULL DEFAULT 0,
    last_error      TEXT NOT NULL DEFAULT '',
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_peer_outbox_due ON peer_outbox(next_attempt_at);
-- One pending announcement per (peer, serial): a device that checks in twice while the
-- peer is unreachable should not queue the same news twice.
CREATE UNIQUE INDEX IF NOT EXISTS idx_peer_outbox_unique ON peer_outbox(peer, serial);

CREATE INDEX IF NOT EXISTS idx_devices_agent_kind ON devices(agent_kind);
CREATE INDEX IF NOT EXISTS idx_devices_onboarding ON devices(enrolled_at) WHERE onboarded_at IS NULL;

-- One-time backfill for rows that predate the columns: kind from the last keyframe,
-- status from the enrollment credential, and every existing device counts as already
-- onboarded so the upgrade does not dump the whole fleet into the inbox.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM app_flags WHERE flag = 'mixed_fleet_v1') THEN
        UPDATE devices SET agent_kind = 'dpc' WHERE latest_extra->>'agent_type' = 'dpc';
        UPDATE devices SET capabilities = latest_extra->'capabilities'
            WHERE jsonb_typeof(latest_extra->'capabilities') = 'array';
        UPDATE devices SET enrollment_status = 'enrolled' WHERE enrolled_via IS NOT NULL;
        UPDATE devices SET enrolled_at = created_at, onboarded_at = created_at;
        INSERT INTO app_flags (flag) VALUES ('mixed_fleet_v1');
    END IF;
END $$;

-- Enrollment profiles carry the intent for a batch of devices: what they are (class),
-- where they go (site), and how long / how many the token is good for.
ALTER TABLE enrollment_profiles ADD COLUMN IF NOT EXISTS device_class TEXT NOT NULL DEFAULT '';
ALTER TABLE enrollment_profiles ADD COLUMN IF NOT EXISTS restaurant_id UUID REFERENCES restaurants(id) ON DELETE SET NULL;
ALTER TABLE enrollment_profiles ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
ALTER TABLE enrollment_profiles ADD COLUMN IF NOT EXISTS max_enrolls INT;
ALTER TABLE enrollment_profiles ADD COLUMN IF NOT EXISTS last_enrolled_at TIMESTAMPTZ;

-- Who did it: the desktop enroll app mints a token per signed-in user, and the device records that
-- user's username when it enrolls through it (most recent enrollment wins; '' = not known).
ALTER TABLE enrollment_profiles ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN IF NOT EXISTS enrolled_by TEXT NOT NULL DEFAULT '';

-- Super op: an access admin who may also push firmware updates (deploy releases,
-- add targets, retry / cancel) but not manage release packages. Effective role
-- constraint; keep it last.
-- The role shipped briefly as 'ota_admin'; remap before the constraint so an
-- account created under that name doesn't block the migration.
UPDATE users SET role = 'super_op' WHERE role = 'ota_admin';
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD  CONSTRAINT users_role_check CHECK (role IN ('viewer','support','operator','fleet_manager','fleet_admin','tester','user_manager','super_op','dev','admin','owner','staff'));

-- 2026-10-07: the 'dev' role is retired. Its release / shell / sign-off rights are
-- the super admin's now; the accounts that held it become super ops (operator + OTA
-- + remote + access rules). Remap before the constraint drops the value.
UPDATE users SET role = 'super_op' WHERE role = 'dev';
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD  CONSTRAINT users_role_check CHECK (role IN ('viewer','support','operator','fleet_manager','fleet_admin','tester','user_manager','super_op','admin','owner','staff'));

-- The one adb key pair the firmware trusts (2026-10-07, for the AIO Enroll Android app).
-- One row; the private key is sealed with FLEET_ADB_KEY_SECRET before it gets here. The
-- version lives in its own one-row table so it keeps rising across a delete + re-upload.
CREATE TABLE IF NOT EXISTS fleet_adb_key (
    id                 INT PRIMARY KEY CHECK (id = 1),
    version            INT NOT NULL,
    fingerprint        TEXT NOT NULL,
    public_key         TEXT NOT NULL,
    private_key_sealed TEXT NOT NULL,
    uploaded_by        TEXT NOT NULL DEFAULT '',
    uploaded_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS fleet_adb_key_seq (id INT PRIMARY KEY CHECK (id = 1), last_version INT NOT NULL DEFAULT 0);
INSERT INTO fleet_adb_key_seq (id, last_version) VALUES (1, 0) ON CONFLICT (id) DO NOTHING;

-- One key per vendor (2026-10-09). A key baked into a vendor's image cannot be changed
-- without a firmware release, so one key shared by every vendor means one leak reopens the
-- whole fleet; a key per vendor keeps a leak to that vendor's units. 'default' is the key
-- that was the only one before this table existed, and is still what the firmware trusts.
-- The version counter stays global (fleet_adb_key_seq): the apps re-fetch the whole set
-- when it moves, so any upload tells every app that something changed.
CREATE TABLE IF NOT EXISTS fleet_adb_keys (
    label              TEXT PRIMARY KEY,
    version            INT NOT NULL,
    fingerprint        TEXT NOT NULL,
    public_key         TEXT NOT NULL,
    private_key_sealed TEXT NOT NULL,
    uploaded_by        TEXT NOT NULL DEFAULT '',
    uploaded_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- Carry the single old row over. ON CONFLICT DO NOTHING so a later deploy cannot put a
-- stale copy back over a key uploaded since; fleet_adb_key itself is left alone, unread.
INSERT INTO fleet_adb_keys (label, version, fingerprint, public_key, private_key_sealed, uploaded_by, uploaded_at)
SELECT 'default', version, fingerprint, public_key, private_key_sealed, uploaded_by, uploaded_at
FROM fleet_adb_key WHERE id = 1
ON CONFLICT (label) DO NOTHING;

-- Scout enrolment (plan: "a T7 enrols the other devices in its restaurant"). A firmware
-- device (the scout) sweeps its restaurant's Wi-Fi on :5555 with the fleet adb key and
-- reports every device that answers as a sighting; an operator (or, later, the auto-enrol
-- switch) turns a sighting into an enrolled standard-client device. One row per
-- (restaurant, serial): the latest scan overwrites the last.
CREATE TABLE IF NOT EXISTS sightings (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    restaurant_id    UUID NOT NULL REFERENCES restaurants(id) ON DELETE CASCADE,
    scout_serial     TEXT NOT NULL DEFAULT '',   -- the firmware device that saw it
    host             TEXT NOT NULL DEFAULT '',    -- last address on the venue's Wi-Fi
    port             INT  NOT NULL DEFAULT 5555,
    serial           TEXT NOT NULL,
    manufacturer     TEXT NOT NULL DEFAULT '',
    model            TEXT NOT NULL DEFAULT '',
    android          TEXT NOT NULL DEFAULT '',
    owner_pkg        TEXT NOT NULL DEFAULT '',
    owner_ours       BOOLEAN NOT NULL DEFAULT FALSE,
    accounts         INT  NOT NULL DEFAULT 0,
    users            INT  NOT NULL DEFAULT 1,
    dpc_version      TEXT NOT NULL DEFAULT '',
    firmware_version TEXT NOT NULL DEFAULT '',
    class_guess      TEXT NOT NULL DEFAULT '',
    -- ready | blocked | ours | enrolling | failed | enrolled
    state            TEXT NOT NULL DEFAULT 'ready',
    reason           TEXT NOT NULL DEFAULT '',    -- why blocked / why a job failed
    step             INT  NOT NULL DEFAULT 0,     -- 1..8 while enrolling
    approved_by      TEXT NOT NULL DEFAULT '',
    approved_at      TIMESTAMPTZ,
    job_id           TEXT NOT NULL DEFAULT '',    -- the net_enroll job currently running
    first_seen       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (restaurant_id, serial)
);
CREATE INDEX IF NOT EXISTS idx_sightings_restaurant ON sightings(restaurant_id);
CREATE INDEX IF NOT EXISTS idx_sightings_job ON sightings(job_id) WHERE job_id <> '';

-- A device that answers adb but refuses the fleet key is still a device worth showing:
-- adb devices calls it "unauthorized" and the Enroll app lists it, because someone at
-- the device can tap Allow and then it enrols like any other. Nothing about it can be
-- read without auth, so its row has no serial and is keyed on the address instead. That
-- needs the one-row-per-serial rule to apply only to rows that have a serial, so the
-- table constraint is replaced by two partial unique indexes.
ALTER TABLE sightings DROP CONSTRAINT IF EXISTS sightings_restaurant_id_serial_key;
CREATE UNIQUE INDEX IF NOT EXISTS sightings_rest_serial
    ON sightings(restaurant_id, serial) WHERE serial <> '';
CREATE UNIQUE INDEX IF NOT EXISTS sightings_rest_host_noserial
    ON sightings(restaurant_id, host) WHERE serial = '';
-- Which of our adb keys the device accepted (2026-10-09). A device trusts the key that was
-- in its image, so this says which vendor's build it is running — and, when a key has to be
-- rotated, exactly which devices are affected. '' = unknown (a scout older than vendor keys).
ALTER TABLE sightings ADD COLUMN IF NOT EXISTS key_label TEXT NOT NULL DEFAULT '';

-- Who handed a device back, and when (2026-10-09). The mirror of enrolled_by/enrolled_at:
-- an unenroll is a person's decision and the Enrollment page shows it next to the
-- enrolments, so a device leaving the fleet is as accountable as one joining it.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS unenrolled_by TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN IF NOT EXISTS unenrolled_at TIMESTAMPTZ;

-- Catch up devices that reported an unenroll the server did not act on: one that acked
-- before this was built, or whose status update was lost to a restart mid-ack. The command
-- result is the durable record of what the device did, so it — not the ack arriving at the
-- right moment — decides. Idempotent and filtered, so it costs nothing on later starts.
UPDATE devices d SET enrollment_status = 'unenrolled',
       unenrolled_at = COALESCE(d.unenrolled_at, cs.updated_at),
       unenrolled_by = CASE WHEN d.unenrolled_by = '' THEN COALESCE(c.created_by, '') ELSE d.unenrolled_by END
FROM command_status cs JOIN commands c ON c.id = cs.command_id
WHERE cs.device_id = d.id AND c.type = 'unenroll'
  AND cs.status IN ('completed', 'installed')
  AND d.enrollment_status <> 'unenrolled';

-- Which adb key a device is known to take (2026-10-09), by serial, whoever found out: the
-- Enroll apps when they connect, a scout when it scans, the device's own client when it
-- reads /adb_keys. Kept by serial rather than on the device row because it is usually
-- learned before the device is enrolled and has a row at all. It answers the question a
-- leaked key forces: which devices must be reflashed.
CREATE TABLE IF NOT EXISTS device_adb_key (
    serial  TEXT PRIMARY KEY,
    label   TEXT NOT NULL,
    source  TEXT NOT NULL DEFAULT '',   -- enroll-app | scout | client
    seen_by TEXT NOT NULL DEFAULT '',   -- the operator, when a person was involved
    seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_device_adb_key_label ON device_adb_key(label);

-- Per-venue scout controls. scan_enabled gates the scheduled sweep; auto_enroll (phase 3,
-- super admin, off by default) lets the scout enrol classifiable devices with no approval.
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS scan_enabled BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS auto_enroll  BOOLEAN NOT NULL DEFAULT FALSE;

-- The scout that enrolled a device over the Wi-Fi (its serial), for "enrolled via T7 …".
-- Distinct from enrolled_via (an enrollment-profile id, the QR/token path).
ALTER TABLE devices ADD COLUMN IF NOT EXISTS enrolled_via_serial TEXT NOT NULL DEFAULT '';

-- idx_checkins_device_id was a strict prefix of idx_checkins_device_created_at, so the
-- planner never had a reason to choose it: measured over 6.5 hours of live traffic the
-- composite took 29,440 scans and this one took none, while both were written on every
-- insert. 177 MB back on the live box. Dropping an unused index is metadata work, so
-- the exclusive lock it takes is momentary — but it is still a lock, hence last, after
-- everything else has succeeded.

-- Telemetry, stored by shape rather than as one snapshot per report.
--
-- A checkins row carries every field the device sent, so a wireless-charging flip
-- costs ~665 bytes of which ~55% never changed from the row before it. These two
-- tables split that: what CHANGED, and what is a NUMBER OVER TIME. Measured over 600
-- consecutive rows of the busiest device, seventeen of twenty-three keys never changed
-- once, and the five that genuinely move fit in fixed columns.
--
-- Nothing reads these yet; they are written alongside checkins so the two can be
-- compared before any reader moves over.

-- One row per real state change. Nothing is written while a state holds, so an idle
-- device costs nothing at all.
-- Battery data from the firmware client (1.10.1+, T7): every battery broadcast and screen
-- edge the device logged, sent in batches to /api/v1/battery-log, and the client's own
-- summary of each constant-current charge climb (extra.battery_cc). Raw material for a
-- battery health model; nothing reads them for alerts yet. t is the device's own clock
-- (part of the key, so a resent batch dedupes exactly); t_fix_ms is how far that clock
-- was from the server's when the batch arrived.
CREATE TABLE IF NOT EXISTS battery_samples (
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    t         TIMESTAMPTZ NOT NULL,
    up_s      BIGINT NOT NULL,
    ev        CHAR(1) NOT NULL,
    mv        INT,
    pct       SMALLINT,
    status    SMALLINT,
    plug      SMALLINT,
    temp_c    REAL,
    screen    BOOLEAN,
    ichg_ma   INT,
    iin_ma    INT,
    flap      BOOLEAN,
    wlc       SMALLINT,
    wlc_on    SMALLINT,
    t_fix_ms  BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (device_id, t, up_s, ev)
);
CREATE INDEX IF NOT EXISTS idx_battery_samples_t ON battery_samples(t);
CREATE TABLE IF NOT EXISTS battery_climbs (
    device_id   UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    at          TIMESTAMPTZ NOT NULL,        -- battery_cc.at, the device's clock
    payload     JSONB NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (device_id, at)
);

CREATE TABLE IF NOT EXISTS device_state_events (
    device_id UUID        NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    key       TEXT        NOT NULL,
    from_val  TEXT        NOT NULL DEFAULT '',
    to_val    TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (device_id, key, at)
);
CREATE INDEX IF NOT EXISTS idx_state_events_device_at ON device_state_events(device_id, at DESC);

-- The numeric series, in fixed columns instead of jsonb. Scaled integers rather than
-- floats: temperature in tenths of a degree and storage in tenths of a GB are finer
-- than the sensors are accurate, and smallint is 2 bytes against 8 for a float8 and
-- ~20 for the same number as jsonb text.
--
-- The primary key is (device_id, at) — the order every reader wants, so it needs no
-- second index, and unlike checkins' random uuid key every insert lands at the right
-- edge instead of dirtying a random page.
CREATE TABLE IF NOT EXISTS device_samples (
    device_id        UUID        NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    battery_pct      SMALLINT,
    temp_dc          SMALLINT,   -- superseded by temp_c; see below
    wifi_rssi        SMALLINT,
    ram_pct          SMALLINT,   -- superseded by ram_used_mb/ram_total_mb
    storage_free_dgb SMALLINT,   -- superseded by storage_free_gb
    PRIMARY KEY (device_id, at)
);

-- The first cut stored scaled integers (tenths of a degree, whole percent). That was
-- smaller but it made the chart and the CSV export disagree: the CSV prints the value
-- the device sent (28.700000762939453 — a Java float through JSON) while the chart
-- would have plotted 28.7, and anyone dividing the CSV's ram_used_mb by ram_total_mb
-- got 55.3% where the chart said 55%. Two surfaces quoting the same reading must not
-- differ, so the columns hold the reading itself:
--   double precision round-trips ANY json number exactly. real was tried first and is
--   lossless only by accident of the current firmware, whose readings originate as a
--   Java float32: a client sending a plain 31.4 came back as 31.399999618530273
--   used/total stay as reported, so a percentage is computed the same way everywhere
-- Six extra bytes a row against a 665-byte snapshot, and no disagreement to explain.
ALTER TABLE device_samples ADD COLUMN IF NOT EXISTS temp_c          DOUBLE PRECISION;
ALTER TABLE device_samples ADD COLUMN IF NOT EXISTS ram_used_mb     INTEGER;
ALTER TABLE device_samples ADD COLUMN IF NOT EXISTS ram_total_mb    INTEGER;
ALTER TABLE device_samples ADD COLUMN IF NOT EXISTS storage_free_gb DOUBLE PRECISION;
-- SoC temperature from battery-less TV boxes (extra.cpu_temp_c). Its own column, not
-- temp_c: temp_c feeds the overheating rule at 45 °C, and an SoC idles above that.
ALTER TABLE device_samples ADD COLUMN IF NOT EXISTS cpu_temp_c      DOUBLE PRECISION;
-- Readings the device kept while it couldn't reach the MDM and sent when it could
-- (client 1.7.0, 1 Oct). History only: nothing alerts on them; the charts shade them.
ALTER TABLE device_samples ADD COLUMN IF NOT EXISTS late            BOOLEAN NOT NULL DEFAULT false;
-- Nothing ever read the scaled columns; they existed for part of one afternoon.
ALTER TABLE device_samples DROP COLUMN IF EXISTS temp_dc;
ALTER TABLE device_samples DROP COLUMN IF EXISTS ram_pct;
ALTER TABLE device_samples DROP COLUMN IF EXISTS storage_free_dgb;

-- The primary key answers "this device, this time range". Fleet-wide rules ask the
-- other question — "every device, these last twenty minutes" — and with only the
-- composite key that scanned the whole table: 244ms against 4ms for the same window
-- off checkins, which has its own created_at index.
--
-- BRIN rather than btree because samples are appended in time order, so one min/max
-- per block range is enough to skip everything outside the window. 32kB against the
-- hundreds of megabytes a btree over millions of rows would cost, and the same query
-- comes back in 5ms.
CREATE INDEX IF NOT EXISTS idx_device_samples_at ON device_samples USING BRIN (at);

-- checkins.extra stopped being written once every reader moved to the shaped tables.
-- The column stays: rows written before that still hold their snapshots, and the
-- fallback paths read them for any window the shaped tables do not reach. New rows
-- carry '{}'.
--
-- state_hash replaces the one use the snapshot still had on the write path: deciding
-- whether anything non-volatile changed since the last STORED row, which is what keeps
-- a device reporting every few seconds from storing a row every few seconds. It holds
-- an md5 of exactly the projection that comparison used to make.

-- checkins.id is not a key anyone uses. Nothing in the database references it (no
-- foreign key points at checkins), no query looks a check-in up by it, and its scan
-- counter sat at 0 for weeks while the composite index beside it served 17,425. What
-- it cost was 756 MB, and a random page dirtied on every single insert: the id is a
-- random uuid, so each row lands at a random position in the index on a table where
-- everything else appends at the end. That is the worst shape of index for an
-- append-only log.
--
-- The column stays, default and all — the public API echoes it — it simply stops being
-- indexed and enforced-unique. Collisions on random uuids are not a practical concern,
-- and nothing would notice one if it happened, because nothing reads the column.

-- Keep monthly partitions created ahead of the present, so a check-in can never be
-- rejected for want of a partition. Guarded on relkind = 'p': until checkins is
-- actually converted (tools/partition-checkins.sql) this is a cheap no-op, and it
-- starts maintaining itself the moment the conversion lands, with nothing to remember
-- and no second deploy.

-- Saved fleet views. The Fleet page has fifteen filter dimensions plus a sort;
-- the combination an operator rebuilds every morning had to be rebuilt every
-- morning, or bookmarked as a URL nobody else could discover. A view is just
-- that query string with a name on it. Per-user: sharing is a later decision,
-- and a shared-by-default list fills with other people's experiments.
CREATE TABLE IF NOT EXISTS fleet_views (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    query      TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, name)
);
CREATE INDEX IF NOT EXISTS idx_fleet_views_user ON fleet_views(user_id, created_at);

-- Fast temperature: a device switched on here sends its temperature every few seconds
-- (client 1.5.0+, firmware v2.1.023+ where healthd smooths the reading), and every one
-- of those readings is kept in device_temp_fast for a day. device_samples keeps its one
-- row a minute and is not touched. Switching on always carries an end time, so a test
-- left running stops by itself.
CREATE TABLE IF NOT EXISTS temp_fast_devices (
    device_id    UUID        PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    interval_sec INT         NOT NULL,
    until        TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS device_temp_fast (
    device_id UUID             NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    at        TIMESTAMPTZ      NOT NULL DEFAULT NOW(),
    temp_c    DOUBLE PRECISION NOT NULL,
    uptime_s  BIGINT,
    PRIMARY KEY (device_id, at)
);

-- Problems (30 Sep alert review). A problem is the set of alerts open on one device,
-- worked as one: an owner, an acknowledgement with a note, a snooze (muted_until) and a
-- reason when it is resolved by hand. notified_at marks the alerts a channel was told
-- about, so a device already paged is not paged again for each new symptom, and
-- resolve_notified closes that loop with one "resolved" message.
ALTER TABLE alerts ADD COLUMN IF NOT EXISTS assignee         TEXT NOT NULL DEFAULT '';
ALTER TABLE alerts ADD COLUMN IF NOT EXISTS note             TEXT NOT NULL DEFAULT '';
ALTER TABLE alerts ADD COLUMN IF NOT EXISTS resolve_reason   TEXT NOT NULL DEFAULT '';
ALTER TABLE alerts ADD COLUMN IF NOT EXISTS notified_at      TIMESTAMPTZ;
ALTER TABLE alerts ADD COLUMN IF NOT EXISTS resolve_notified BOOLEAN NOT NULL DEFAULT false;
-- One row per morning digest sent, keyed by the fleet's local date, so a restart does
-- not send the same morning twice.
-- Kiosk rules (30 Sep): policies are ordered (lowest priority number wins where two
-- cover one device), can switch on the offline exit code, and are enforced
-- continuously. device_config.kiosk_rule records which rule set a device's kiosk, and
-- kiosk_override marks a device someone changed by hand while a rule covers it.
ALTER TABLE kiosk_policies ADD COLUMN IF NOT EXISTS priority     INT     NOT NULL DEFAULT 0;
ALTER TABLE kiosk_policies ADD COLUMN IF NOT EXISTS offline_exit BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE device_config  ADD COLUMN IF NOT EXISTS kiosk_rule     UUID;
ALTER TABLE device_config  ADD COLUMN IF NOT EXISTS kiosk_override BOOLEAN NOT NULL DEFAULT false;
-- Taken out of kiosk on site (exit PIN or code on the device), 30 Sep: the device stays
-- out of kiosk until someone locks it again or says to leave it; what it was locked to
-- and by which rule is kept for "Lock again".
ALTER TABLE device_config  ADD COLUMN IF NOT EXISTS kiosk_exited_at      TIMESTAMPTZ;
ALTER TABLE device_config  ADD COLUMN IF NOT EXISTS kiosk_exited_package TEXT NOT NULL DEFAULT '';
ALTER TABLE device_config  ADD COLUMN IF NOT EXISTS kiosk_exited_rule    UUID;

-- Access profiles (30 Sep): named, reusable "what they can do" sets — the second
-- question of the access editor. A grant made from a profile carries its id, so editing
-- the profile updates everyone on it.
CREATE TABLE IF NOT EXISTS access_profiles (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    actions     TEXT[] NOT NULL DEFAULT '{}',
    builtin     BOOLEAN NOT NULL DEFAULT false,
    position    INT NOT NULL DEFAULT 0,
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
INSERT INTO access_profiles (name, description, actions, builtin, position) VALUES
  ('Look only', 'See devices and alerts. No commands.', '{view}', true, 0),
  ('Look and fix', 'Screenshot, restart, reload the app, kiosk, run diagnostics, collect logs, notes and the queue.', '{view,screenshot,reboot,app_control,query,logcat,kiosk,notes,queue}', true, 1),
  ('Manage apps too', 'Everything in "Look and fix", plus install and remove apps.', '{view,screenshot,reboot,app_control,query,logcat,kiosk,notes,queue,install_apk,uninstall}', true, 2)
ON CONFLICT (name) DO NOTHING;
ALTER TABLE access_grants ADD COLUMN IF NOT EXISTS profile_id UUID REFERENCES access_profiles(id) ON DELETE SET NULL;

CREATE TABLE IF NOT EXISTS alert_digests (
    day     DATE        PRIMARY KEY,
    sent_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lines   INT         NOT NULL DEFAULT 0
);

-- Guest info (1 Oct): what a guest at the table sees on a T7 — the venue's guest
-- Wi-Fi (security as the Wi-Fi QR format spells it: WPA, WEP or nopass) and the
-- guest ordering app when it is not the default, plus the table the tablet sits on.
-- Sent to the firmware client as config.guest (see db.GuestInfo). Guest Wi-Fi and the
-- guest app were removed on 19 Oct 2026: these four columns stay, unread, so an older
-- image still runs after a rollback.
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS guest_wifi_ssid     TEXT NOT NULL DEFAULT '';
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS guest_wifi_password TEXT NOT NULL DEFAULT '';
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS guest_wifi_security TEXT NOT NULL DEFAULT 'WPA';
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS guest_app_package   TEXT NOT NULL DEFAULT '';
ALTER TABLE devices     ADD COLUMN IF NOT EXISTS table_label         TEXT NOT NULL DEFAULT '';

-- Command leases and history (9 Oct). A lease says which device holds a command and until
-- when, so it returns to the queue on its own instead of needing the 90s re-push sweep,
-- the per-minute re-flush and a stalled-install job to notice. The event log is
-- append-only and read by nobody on the delivery path: command_status stays the live
-- state, so history can never fail a delivery. See db/command_lease.go.
-- Percent is too coarse to watch: one percent of a two-gigabyte image is twenty
-- megabytes, so a download that is working looks frozen for many minutes. Devices that
-- report bytes get a real figure on the page; older clients keep the percent.
ALTER TABLE command_status ADD COLUMN IF NOT EXISTS bytes_done  BIGINT NOT NULL DEFAULT 0;
ALTER TABLE command_status ADD COLUMN IF NOT EXISTS bytes_total BIGINT NOT NULL DEFAULT 0;
ALTER TABLE command_status ADD COLUMN IF NOT EXISTS lease_id         UUID;
ALTER TABLE command_status ADD COLUMN IF NOT EXISTS lease_expires_at TIMESTAMPTZ;
ALTER TABLE command_status ADD COLUMN IF NOT EXISTS attempts         INT NOT NULL DEFAULT 0;

-- Per-command policy, stamped from internal/command at creation, so delivery and expiry
-- read a column instead of re-deciding in SQL. max_attempts 0 = unlimited, which is the
-- behaviour that existed before this and stays the default.
ALTER TABLE commands ADD COLUMN IF NOT EXISTS deadline_at  TIMESTAMPTZ;
ALTER TABLE commands ADD COLUMN IF NOT EXISTS max_attempts INT  NOT NULL DEFAULT 0;
ALTER TABLE commands ADD COLUMN IF NOT EXISTS lane         TEXT NOT NULL DEFAULT 'default';
ALTER TABLE commands ADD COLUMN IF NOT EXISTS guarantee    TEXT NOT NULL DEFAULT 'at_least_once';

CREATE TABLE IF NOT EXISTS command_events (
  id         BIGSERIAL PRIMARY KEY,
  command_id UUID NOT NULL,
  device_id  UUID,
  at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  kind       TEXT NOT NULL,
  detail     JSONB NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS command_events_cmd_idx ON command_events (command_id, at);

-- How this device can be commanded right now (ws | http | none), and the last time a
-- command actually completed for it — proof of control rather than proof of a socket.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS command_channel       TEXT NOT NULL DEFAULT 'none';
ALTER TABLE devices ADD COLUMN IF NOT EXISTS last_round_trip_ok_at TIMESTAMPTZ;

-- No command channel (9 Oct): when a device last started checking in over HTTP while
-- holding no WebSocket. The firmware client takes commands over the socket, so a device
-- in that state reports telemetry, looks healthy and cannot be controlled at all. NULL
-- means it has a socket (or has never been seen without one). See db.MarkWSMissing.
ALTER TABLE devices     ADD COLUMN IF NOT EXISTS ws_missing_since    TIMESTAMPTZ;

-- Command secrets (17 Oct): the fields of a payload that must not be stored with the
-- command — a wifi_config's password. commands.payload is what every view, list, export
-- and API listing shows, so the password lives here instead and is joined back in only
-- on the way to the device. Deleted when every named device has finished with the
-- command, and swept once the command is past delivering (PruneCommandSecrets).
CREATE TABLE IF NOT EXISTS command_secrets (
	command_id UUID PRIMARY KEY REFERENCES commands(id) ON DELETE CASCADE,
	secret     JSONB       NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Venue Wi-Fi (W1, picked 17 Oct): the networks every device at a venue should have, so a
-- router or password change is prepared from the dashboard before it happens.
-- The password is sealed by the server (venue_wifi.go) before it is stored; version moves
-- on every change so the reconciler knows which devices still have the old one.
CREATE TABLE IF NOT EXISTS venue_wifi_networks (
	id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	restaurant_id UUID        NOT NULL REFERENCES restaurants(id) ON DELETE CASCADE,
	ssid          TEXT        NOT NULL,
	security      TEXT        NOT NULL,
	hidden        BOOLEAN     NOT NULL DEFAULT FALSE,
	secret        BYTEA,
	version       INT         NOT NULL DEFAULT 1,
	created_by    TEXT        NOT NULL DEFAULT '',
	created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	removed_at    TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_venue_wifi_live ON venue_wifi_networks(restaurant_id, lower(ssid)) WHERE removed_at IS NULL;
-- One row per wifi_config the reconciler queued for a device: which network, which
-- version, add or remove, and the command that carries it (its status is the outcome).
CREATE TABLE IF NOT EXISTS venue_wifi_pushes (
	network_id UUID        NOT NULL REFERENCES venue_wifi_networks(id) ON DELETE CASCADE,
	device_id  UUID        NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	version    INT         NOT NULL,
	action     TEXT        NOT NULL,
	command_id UUID        REFERENCES commands(id) ON DELETE SET NULL,
	at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (network_id, device_id, version, action)
);

-- Venue contact email (17 Oct, compliance report R1): where the monthly device-security
-- report goes, besides the owner-role users whose access covers the venue. '' = none.
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS contact_email TEXT NOT NULL DEFAULT '';

-- Venue icons (I1, 19 Oct): a 128px PNG fetched from the venue's website (or uploaded),
-- stored here so no page loads anything from the restaurant's site. icon_at versions the
-- image URL; NULL = no icon (the venue shows its initial).
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS icon BYTEA;
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS icon_source TEXT NOT NULL DEFAULT '';
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS icon_at TIMESTAMPTZ;

-- One row per venue per month the compliance report was emailed, so the monthly job
-- sends it once however many times it runs (RunComplianceReports).
CREATE TABLE IF NOT EXISTS compliance_report_sends (
    restaurant_id UUID NOT NULL REFERENCES restaurants(id) ON DELETE CASCADE,
    month         TEXT NOT NULL,                 -- YYYY-MM, the month reported on
    sent_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    recipients    TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (restaurant_id, month)
);

-- Lost-device reporting (18 Oct; A+B+C+D). A lost session is one "marked lost" on the
-- dashboard: the beacon the server handed the device for it, until it is lifted. Devices
-- at the same venue listen for the active beacons and report what they hear
-- (ble_sightings). device_trail is where a device was: Wi-Fi scans it kept while out of
-- reach (C, geolocated later and only for lost or departed devices) and live fixes while
-- lost. self_lock_minutes is the venue's opt-in for D (0 = off).
CREATE TABLE IF NOT EXISTS lost_sessions (
	id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	device_id  UUID        NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	beacon     TEXT,
	command_id UUID        REFERENCES commands(id) ON DELETE SET NULL,
	started_by TEXT        NOT NULL DEFAULT '',
	started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	ended_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_lost_sessions_active ON lost_sessions(device_id) WHERE ended_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_lost_sessions_beacon ON lost_sessions(beacon) WHERE beacon IS NOT NULL;
CREATE TABLE IF NOT EXISTS ble_sightings (
	session_id UUID        NOT NULL REFERENCES lost_sessions(id) ON DELETE CASCADE,
	heard_by   UUID        NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	rssi       SMALLINT    NOT NULL,
	at         TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (session_id, heard_by, at)
);
CREATE INDEX IF NOT EXISTS idx_ble_sightings_at ON ble_sightings(session_id, at DESC);
CREATE TABLE IF NOT EXISTS device_trail (
	id        BIGSERIAL PRIMARY KEY,
	device_id UUID        NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	at        TIMESTAMPTZ NOT NULL,
	source    TEXT        NOT NULL,          -- scan (kept offline) | live (lost, online)
	scan      JSONB,                         -- the Wi-Fi APs it saw, for source = scan
	lat       DOUBLE PRECISION,
	lon       DOUBLE PRECISION,
	accuracy  DOUBLE PRECISION,
	resolved_at TIMESTAMPTZ,                 -- tried (lat stays NULL when it could not be placed)
	UNIQUE (device_id, at, source)
);
CREATE INDEX IF NOT EXISTS idx_device_trail_dev_at ON device_trail(device_id, at DESC);
CREATE INDEX IF NOT EXISTS idx_device_trail_pending ON device_trail(device_id) WHERE resolved_at IS NULL;
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS self_lock_minutes INT NOT NULL DEFAULT 0;
-- Device lists per user (20 Oct): '' is the table (the default), 'cards' the two-line
-- cards from before the table. Set from Fleet's Table/Cards switch, kept across sessions.
ALTER TABLE users ADD COLUMN IF NOT EXISTS fleet_view TEXT NOT NULL DEFAULT '';
-- Staff PIN per venue (20 Oct): what staff type on a locked tablet to unlock the host app
-- (staff mode). '' = the client's built-in default. Sent as config offline_exit.pin.
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS staff_pin TEXT NOT NULL DEFAULT '';
-- When devices move, per venue (22 Oct): when this venue's devices take a new prod firmware
-- and MDM client updates by themselves. update_mode '' = the fleet window (Lineup),
-- 'closed' = outside the venue's service hours, 'own' = update_window_start/end. A pause
-- sits over the mode (Resume brings the mode back); update_paused_until NULL = until resumed.
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS update_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS update_window_start TEXT NOT NULL DEFAULT '';
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS update_window_end TEXT NOT NULL DEFAULT '';
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS update_paused BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS update_paused_until TIMESTAMPTZ;
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS update_set_by TEXT NOT NULL DEFAULT '';
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS update_set_at TIMESTAMPTZ;
-- Auto-enrol per venue (22 Oct): a device the venue's scout finds ready (trusts the fleet
-- adb key, no owner, no account) is enrolled without anyone pressing Enrol. Off by default.
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS auto_enroll BOOLEAN NOT NULL DEFAULT FALSE;
-- Lost-device pause per venue (22 Oct): while it holds, no left_venue alerts and no
-- self-lock here (self_lock_minutes is kept for when it ends). lost_paused_until NULL with
-- lost_paused = until resumed.
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS lost_paused BOOLEAN NOT NULL DEFAULT FALSE;
-- Host app channel per venue (V1, 23 Oct): '' = prod; else the variant suffix every
-- product's app lands as here (uatv2 → aio.app.nugget.uatv2).
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS host_app_channel TEXT NOT NULL DEFAULT '';
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS lost_paused_until TIMESTAMPTZ;
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS lost_set_by TEXT NOT NULL DEFAULT '';
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS lost_set_at TIMESTAMPTZ;
-- Per-app usage (A1, 22 Oct): what each app (and system service) cost on a device, one row
-- per device, package and hour (kept 14 days) and per day (kept 90 days). Memory and CPU
-- are averages of the reports in the bucket (sum / samples); the rest add up.
CREATE TABLE IF NOT EXISTS app_usage (
    device_id   UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    bucket      TIMESTAMPTZ NOT NULL,
    daily       BOOLEAN NOT NULL DEFAULT FALSE,
    package     TEXT NOT NULL,
    system      BOOLEAN NOT NULL DEFAULT FALSE,
    fg_ms       BIGINT NOT NULL DEFAULT 0,
    launches    INT NOT NULL DEFAULT 0,
    rx_bytes    BIGINT NOT NULL DEFAULT 0,
    tx_bytes    BIGINT NOT NULL DEFAULT 0,
    pss_sum_mb  DOUBLE PRECISION NOT NULL DEFAULT 0,
    pss_max_mb  INT NOT NULL DEFAULT 0,
    pss_n       INT NOT NULL DEFAULT 0,
    cpu_sum_pct DOUBLE PRECISION NOT NULL DEFAULT 0,
    cpu_n       INT NOT NULL DEFAULT 0,
    power_mah   DOUBLE PRECISION NOT NULL DEFAULT 0,
    PRIMARY KEY (device_id, daily, bucket, package)
);

-- AIO backend (23 Oct): a copy of the restaurant backend's restaurants and devices, per
-- environment (prod / stage), refreshed by the sync job; pages read this, never Metabase.
CREATE TABLE IF NOT EXISTS backend_restaurants (
    env           TEXT NOT NULL,
    id            BIGINT NOT NULL,
    name          TEXT NOT NULL DEFAULT '',
    business_id   BIGINT NOT NULL DEFAULT 0,
    business_name TEXT NOT NULL DEFAULT '',
    address       TEXT NOT NULL DEFAULT '',
    city          TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL DEFAULT '',
    zip           TEXT NOT NULL DEFAULT '',
    latitude      DOUBLE PRECISION,
    longitude     DOUBLE PRECISION,
    timezone      TEXT NOT NULL DEFAULT '',
    phone         TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (env, id)
);
CREATE TABLE IF NOT EXISTS backend_devices (
    env           TEXT NOT NULL,
    id            BIGINT NOT NULL,
    restaurant_id BIGINT NOT NULL DEFAULT 0,
    device_type   TEXT NOT NULL DEFAULT '',
    serial        TEXT NOT NULL DEFAULT '',
    ip            TEXT NOT NULL DEFAULT '',
    onboarded     BOOLEAN NOT NULL DEFAULT FALSE,
    revoked       BOOLEAN NOT NULL DEFAULT FALSE,
    updated       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (env, id)
);
CREATE INDEX IF NOT EXISTS backend_devices_serial ON backend_devices (upper(serial));
CREATE INDEX IF NOT EXISTS backend_devices_restaurant ON backend_devices (env, restaurant_id);
CREATE TABLE IF NOT EXISTS backend_sync (
    env          TEXT PRIMARY KEY,
    synced_at    TIMESTAMPTZ,
    attempted_at TIMESTAMPTZ,
    error        TEXT NOT NULL DEFAULT '',
    restaurants  INT NOT NULL DEFAULT 0,
    devices      INT NOT NULL DEFAULT 0,
    requested    BOOLEAN NOT NULL DEFAULT FALSE
);
-- Hours per weekday (H1, 23 Oct): [{closed, open, close} × Mon…Sun]; NULL = the same
-- window every day. open_min/close_min then hold the week's widest window.
ALTER TABLE service_windows ADD COLUMN IF NOT EXISTS week JSONB;
-- The backend's hours for linked restaurants, copied by the sync (P2).
CREATE TABLE IF NOT EXISTS backend_hours (
    env           TEXT NOT NULL,
    restaurant_id BIGINT NOT NULL,
    week          JSONB,
    synced_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (env, restaurant_id)
);
-- Venue fields that follow the AIO backend (B1): address, pin, timezone, hours. A field
-- leaves the list when someone edits it in the MDM ("Set here").
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS backend_follow TEXT[] NOT NULL DEFAULT '{}';
-- Backend restaurants someone chose to ignore on the Venues page (test restaurants) (M2).
CREATE TABLE IF NOT EXISTS backend_ignores (
    env           TEXT NOT NULL,
    restaurant_id BIGINT NOT NULL,
    ignored_by    TEXT NOT NULL DEFAULT '',
    ignored_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (env, restaurant_id)
);
-- A venue's backend restaurant (P1): '' / NULL = not linked. A person confirms a link.
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS backend_env TEXT NOT NULL DEFAULT '';
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS backend_restaurant_id BIGINT;
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS backend_linked_by TEXT NOT NULL DEFAULT '';
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS backend_linked_at TIMESTAMPTZ;

-- Role ladder (20 Oct): viewer → support → operator → fleet_manager → fleet_admin
-- ("Admin") → admin (super admin), and customers owner / staff. Super ops and access
-- admins become fleet managers (super ops push firmware only where a rule says so now).
-- Sessions carry the role too, so open sessions move with their user.
UPDATE users    SET role = 'fleet_manager' WHERE role IN ('super_op', 'user_manager');
UPDATE sessions SET role = 'fleet_manager' WHERE role IN ('super_op', 'user_manager');
-- The effective role constraint: exactly the roles in roleDefs (dashboard/roles.go).
-- Keep it last; TestRoleConstraintCoversEveryRole checks it.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD  CONSTRAINT users_role_check CHECK (role IN ('viewer','support','operator','fleet_manager','fleet_admin','admin','owner','staff'));

`
