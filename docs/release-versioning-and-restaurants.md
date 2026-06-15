# Plan — "Release version" naming + Restaurant object

> **Status:** Design / plan (pre-implementation). Two related changes to MDM:
> (1) couple a device's reported build to a **release** and call it the **release version**
> in the UI; (2) introduce a first-class **restaurant** object so "where a device lives" is
> no longer overloaded onto free-form groups.
>
> **Decisions locked (2026-06-15, with product):**
> 1. **Rename depth:** UI labels only → "Release version", plus a *derived* device→release
>    link. **No DB column rename, no API/JSON key change, no client rebuild.** `build_id`
>    stays the wire/column name; we relabel it everywhere users see it.
> 2. **Restaurant model:** new flat `restaurants` table. `devices.restaurant_id` nullable
>    (NULL = lab/bench unit). Chain/brand parent deferred (add `chain_id` later, no lock-in).
> 3. **Venue semantics:** **service windows + `deployed` + health/AI bucketing move onto
>    restaurants.** **OTA-by-group stays on groups** (test cohorts). **Groups become pure
>    free-form tags** — which is all the test team uses them for today ("mic issue",
>    "battery cycles").

---

## Part 1 — Build ID → "Release version" (couple build & release)

### Background (what exists today)
- A device reports `build_id` (free string): `devices.build_id`, copied into `checkins.build_id`
  and `device_daily_stats.build_id`. Set on every checkin from `checkinRequest.BuildID`.
- A **`releases`** table already exists: `version` (UNIQUE), `name`, `changelog`,
  `status` (draft/published/yanked). It groups OTA packages; `ota_packages.release_id` and
  `releases.version == target_build_id` tie an image to a release.
- **The gap:** a device's reported `build_id` has **no link** to `releases`. We never tell the
  operator "this device is running release *X* (published / yanked / unknown)".

### The change (non-breaking)
**A. Derive the link (read-side only — no schema change).**
- A device's release = `releases WHERE version = devices.build_id`. The helper
  `GetReleaseByVersion(version)` already exists (`db.go:4435`) — reuse it.
- On device detail and device list, resolve the device's `build_id` to its release and show
  release **name + status**. If no match → render **"Unmanaged / unknown release"** (a useful
  signal: the device is on a build MDM doesn't know about — overlaps matrix alert #25).
- Optionally add a read-only computed field `release_status` to the device JSON used by the
  dashboard (derived in the query via `LEFT JOIN releases r ON r.version = d.build_id`). No new
  column, no migration.

**B. Relabel the UI everywhere "Build ID" appears → "Release version".**
- Templates to touch (string/label only): `device.html` ("Build ID" rows + eyebrow + the
  checkin modal label), `devices.html` (column header + filter label), `overview.html`,
  `export.html`, `group_detail.html`, `group_form.html`, `settings.html`, and the OTA/release
  pages (`releases.html`, `release_detail.html`, `production_detail.html`,
  `deployment_detail.html`) for consistency.
- **Keep technical OTA package internals** (`target_build_id` / `source_build_id`) labeled as
  build identifiers where they refer to image artifacts — those are genuinely build-level. The
  *device-facing* and *release-facing* term becomes "Release version". Add a one-line tooltip
  on first use: "Release version — the build the device is running."

**C. Explicitly out of scope (per decision 1):** renaming the `build_id` DB columns, the
admin API JSON keys, or the client field. The client keeps sending `build_id`. This keeps the
change zero-risk and avoids an AOSP coordination.

### Touch list (Part 1)
- `internal/dashboard/handlers.go` — add the `LEFT JOIN releases` for release name/status in
  the device list + detail view models (read-only).
- `templates/*.html` — label rename (mechanical, ~15 files; only the listed user-facing ones).
- Optional: a small `releaseForBuild(build_id)` view helper to render
  name/status/"unmanaged" consistently.
- **No** `db.go` schema change, **no** `migrationSQL` edit, **no** client change.

### Verification
- Seed a device whose `build_id` matches a published release → detail shows "Release version:
  <name> (published)". Seed one with a bogus build → "Unmanaged / unknown release". Confirm no
  template references the literal "Build ID" for the device-facing field.

---

## Part 2 — Restaurant object

### Why a new object (not a typed group)
A device physically sits in **exactly one** venue (1-to-many), but `device_groups` is
many-to-many and groups are used purely as overlapping test tags. Forcing "venue" into groups
would (a) allow a device in two "restaurant" groups and (b) keep piling venue semantics onto a
bag-of-tags. A dedicated 1:N object is the clean model. Groups stay untouched for the test team.

### Schema (append to `migrationSQL`, idempotent — never reorder existing statements)
```sql
CREATE TABLE IF NOT EXISTS restaurants (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    address     TEXT NOT NULL DEFAULT '',
    latitude    DOUBLE PRECISION,
    longitude   DOUBLE PRECISION,
    timezone    TEXT NOT NULL DEFAULT '',     -- IANA tz; '' = fall back to device-reported
    deployed    BOOLEAN NOT NULL DEFAULT false,-- whole venue is live (devices inherit)
    -- chain_id  UUID  -- deferred; add later when per-brand rollups are needed
    notes       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- device -> at most one restaurant; NULL = lab/bench unit.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS restaurant_id
    UUID REFERENCES restaurants(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_devices_restaurant ON devices(restaurant_id);

-- service windows move from group_id to restaurant_id (keep the fleet-default NULL row).
ALTER TABLE service_windows ADD COLUMN IF NOT EXISTS restaurant_id
    UUID REFERENCES restaurants(id) ON DELETE CASCADE;
CREATE UNIQUE INDEX IF NOT EXISTS idx_service_windows_restaurant
    ON service_windows (restaurant_id) WHERE restaurant_id IS NOT NULL;
```
Notes:
- `service_windows.group_id` is **left in place** but deprecated; resolution switches to
  `restaurant_id`. Since today's groups are test tags, there are no real per-group windows to
  migrate (only the fleet-default `group_id IS NULL` row, which is unchanged).
- No data migration of groups → restaurants (groups were never venues).

### "Deployed" — re-home the effective-deployed computation
Today (`db.go:757–761`): `deployed_effective = COALESCE(device.deployed, bool_or(group.deployed over device_groups), false)`.
Change to restaurant-based:
```sql
COALESCE(d.deployed, (SELECT r.deployed FROM restaurants r WHERE r.id = d.restaurant_id), false)
```
- Keep `devices.deployed` as the per-device override (unchanged semantics).
- Replace `SetGroupDeployed` with `SetRestaurantDeployed`; remove the group `deployed` path
  from the dashboard (the group deploy toggle in `handlers.go:2745` and `group` deployed UI).
- `groups.deployed` column can stay (harmless) or be dropped later; stop reading it.

### Service-window resolution — re-point to restaurant
- `GetGroupServiceWindow(groupID)` → `GetRestaurantServiceWindow(restaurantID)`.
- The evaluator resolves each device's window via its `restaurant_id` (falling back to the
  fleet-default NULL row), instead of via group membership. `GetFleetServiceWindow` /
  `inActiveWindow` / `inMinWindow` are unchanged — only the lookup key changes.

### Health & AI bucketing — by restaurant
- `GetGroupHealth` / `GetGroupDailyStats` → `GetRestaurantHealth` / `GetRestaurantDailyStats`,
  grouping by `devices.restaurant_id` instead of `device_groups`. `computeScore` is reused
  as-is.
- `/fleet-health` ranks **restaurants** worst-first (lab units with no restaurant are excluded
  or shown in a separate "lab" bucket — matches the existing lab-vs-deployed framing).
- `internal/ai/ai.go`: the persona/prompt already says "each **group** is flagged deployed or
  lab" — update `AnalyzeFleet` to take restaurant health and say "restaurant" (the prose
  already talks about restaurants; the data source just moves from groups to restaurants).
  `DeploymentCounts` switches its deployed/lab tally to the restaurant-based effective-deployed.

### DB layer (new/changed functions in `db.go`)
- New: `CreateRestaurant`, `ListRestaurants`, `GetRestaurant`, `UpdateRestaurant`,
  `DeleteRestaurant`, `AssignDeviceToRestaurant(serial, restaurantID *uuid.UUID)`,
  `ListRestaurantDevices`, `GetRestaurantServiceWindow`, `SetRestaurantServiceWindow`,
  `SetRestaurantDeployed`, `GetRestaurantHealth`, `GetRestaurantDailyStats`.
- Changed: effective-deployed SQL in the device queries; evaluator window/deployed resolution;
  `DeploymentCounts`.

### API + routes (`cmd/server/main.go` — centralized registration)
- **Admin REST** (`adminAuth`): `GET/POST /api/v1/restaurants`, `GET/PATCH/DELETE
  /api/v1/restaurants/{id}`, `POST /api/v1/restaurants/{id}/devices` (assign),
  `DELETE /api/v1/restaurants/{id}/devices/{serial}` (unassign).
- **Dashboard:** `/restaurants` (list), `/restaurants/{id}` (detail: members, health, trends,
  service-window editor, deployed toggle), restaurant create/edit form. Nav entry.
- Publish the matching `ws.Hub` events on mutate (device reassignment, deployed change) so the
  dashboard refreshes — per the repo convention.

### Templates
- New: `restaurants.html`, `restaurant_detail.html`, `restaurant_form.html`.
- Update: `device.html` (show + assign restaurant), `devices.html` (restaurant column +
  filter), `settings.html` (service windows now per restaurant; remove per-group window UI),
  `fleet-health` template (by restaurant). The old `group_detail.html` deployed/window/health
  bits are removed (groups become tag-only).

### Migration / back-compat strategy
1. Add `restaurants` + `devices.restaurant_id` + `service_windows.restaurant_id` (all
   idempotent, additive).
2. Switch reads (deployed, windows, health, AI) to restaurant; leave `groups.deployed` and
   `service_windows.group_id` columns in place but unread (drop in a later cleanup once
   confirmed safe).
3. No automatic group→restaurant data move (groups were never venues). Ops creates restaurants
   and assigns devices via the new UI/API.

---

## Sequencing (both parts)

1. **Part 1 (release-version)** — small, non-breaking, ship first: read-side join + label
   rename + verify.
2. **Part 2 schema + DB layer** — restaurants table, `restaurant_id`, re-pointed
   deployed/window/health functions.
3. **Part 2 API + dashboard** — CRUD, assignment UI, nav, fleet-health by restaurant.
4. **Cleanup** — drop unread `groups.deployed` / `service_windows.group_id` after a release of
   bake time; revisit `chain_id` if per-brand rollups are requested.

## Open items to confirm during build
- Restaurant `name` uniqueness — recommend **not** globally unique (chains reuse names); rely
  on id. Add a soft uniqueness check in the UI if desired.
- Whether `/fleet-health` hides lab (no-restaurant) units entirely or shows a separate bucket.
- Whether to keep `groups.deployed` readable during transition (recommend: stop reading it
  immediately, drop the column later).

---

## Implementation status

### Release management audit (Part 1 — "check everything") — done 2026-06-15
The system already **is** a proper release-management system with changelog:
- `releases` table: `version` (unique), `name`, `changelog`, `status` (draft → published →
  yanked, `published_at` stamped on first publish).
- Per-release OTA packages (full + incrementals), per-release deployments (reboot behavior,
  scheduling, targets, retry/cancel/add-targets), per-device artifact resolution, RBAC.
- Changelog is captured on the create form and rendered on the release detail page.

**Gaps found — both now closed (see Part 1 below):**
1. ~~`name`/`changelog` set at create time only — no edit route/form.~~ **Done:** `POST
   /updates/{id}/meta` (`ReleaseEditMeta` → `SetReleaseMeta`) + edit form on the release page.
2. ~~device→release coupling + "Build ID → Release version" relabel not implemented.~~ **Done.**

### Part 1 (Release version rename + coupling) — IMPLEMENTED 2026-06-15
Non-breaking: no DB column rename, no API/JSON key change, no client change (`build_id`
stays the wire/column name). Build + gofmt clean; all templates parse.

- **Coupling (read-side):** device detail resolves its reported build to a known release via
  the existing `GetReleaseByVersion` (`release.version == device.build_id`), showing the
  release name + lifecycle status, or an **`unmanaged`** badge when no release matches.
- **Relabel:** device-facing "Build ID" → "Release version" across device detail/history/
  modal, the devices list, group/restaurant/production member tables, overview, settings, and
  export. OTA package target/source build ids (image artifacts) keep their build wording.
- **Editable changelog:** name + changelog editable after creation on the release detail page.

### Part 2 (Restaurant object) — IMPLEMENTED 2026-06-15
Branch work landed in `server/`. Build + vet + gofmt clean; all templates parse. Not yet
run against a live DB here (no Docker/psql in this env) — migrations reviewed by hand.

- **Schema** (`migrationSQL`, idempotent): `restaurants` table; `devices.restaurant_id`
  (nullable, `ON DELETE SET NULL`); `service_windows.restaurant_id` with the fleet-default
  unique index redefined to `group_id IS NULL AND restaurant_id IS NULL` + a per-restaurant
  unique index.
- **Re-pointed venue semantics to restaurants:** `GetDevice` deployed_effective,
  `DeploymentCounts`, `effectiveWindows`, `deployedDeviceSet` now use
  `COALESCE(device.deployed, restaurant.deployed, false)` / `devices.restaurant_id`.
  `ServiceWindow.GroupID` → `RestaurantID`; `GetGroupServiceWindow` → `GetRestaurantServiceWindow`.
- **New DB layer:** Restaurant struct + CRUD, `AssignDeviceToRestaurant`,
  `ListRestaurantDevices`, `SetRestaurantDeployed`, `GetRestaurantServiceWindow`,
  `GetRestaurantHealth`, `GetRestaurantDailyStats`.
- **Handlers/routes:** `/restaurants` list, detail, new/edit form, create/update/delete,
  deploy toggle, assign/remove device, per-restaurant service window, daily-stats JSON;
  `POST /devices/{serial}/restaurant`. Nav entry + `ActivePage`.
- **Re-pointed surfaces:** Fleet Health page + AI fleet report + Overview pulse now use
  restaurant health; AI prose says "restaurant". Settings "Service hours" lists restaurants.
- **Groups reverted to pure tags:** removed the group deploy toggle (handler + route + UI).
  `groups.deployed` / `service_windows.group_id` columns kept but unread (drop later).
- **Device detail:** shows + assigns its restaurant; deployment wording now "inherit from
  restaurant".

**Deferred (follow-ups):** admin REST API routes for restaurants (dashboard fully manages
them today); cleanup `DELETE` of stale per-group service-window rows; the Part-1 release
rename/coupling; release changelog-edit route.
</content>
