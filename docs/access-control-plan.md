# Access control v2: plan

Status: proposal, 2026-09-07. Nothing here is implemented yet.

## 1. What exists today

- Roles (ceiling): admin > dev > user_manager > operator > viewer / owner. `roleLevels`, `commandRoles` in `internal/dashboard/handlers.go`.
- Per-user policy in `users.access` JSONB (`db.AccessPolicy`): `base` allow/deny, `hide_out_of_scope`, rules of `{effect, actions[], scope_type all|group|restaurant, scope_id}`.
- Engine: `internal/dashboard/access.go` (`access.can`, `filterDevices`, `visibleIDs`, `requireDeviceAction`, `requireFleetAction`, `enforceCommandTargets`). Policy cache 20 s per username.
- Editor: a card on the user profile page (`templates/profile.html`, `pf-rule` partial). Summary list at `/users/access`.

### Gaps found in the audit

| Area | Problem |
|---|---|
| Scope | No per-serial (device) scope. Only fleet / group / restaurant. |
| Shell | Hard role gate (admin only). Not grantable. Shell page has no per-device check. |
| Remote control | `GET /devices/{serial}/remote` is open to every operator, no per-device check. Ticket for the remote WebSocket is issued by that page, so the WS inherits the gap. |
| OTA | `POST /updates`, `POST /releases/{id}/deploy` are admin/dev only. Not grantable, no target scoping. |
| Visibility | `visibleIDs` is applied only to the device list and owner home. Unfiltered: overview, map + `map.json`, alerts, export, command history/detail, groups/restaurants detail + members, `devices/search`, `cmdk-index`, `events/devices` SSE, `alerts/events` SSE, every `/devices/{serial}/*` partial (chart-data, stats, vitals, packages, logcat live, screenshot, …). |
| Device page | Checks `view` in `DeviceDetail` only. Sub-routes reachable by URL. |
| Group commands | A group target is refused if any device is out of scope, instead of being narrowed. |
| Deletion | Rules point at group/restaurant ids as strings. Deleting the group leaves a dangling rule ("in a deleted group"). |
| Delegation | `user_manager` can grant anything below their level, including things they do not hold themselves. |
| Audit | Grant changes are logged as one summary string. Denied attempts are not logged. |
| Tests | No tests for `can()` or for route coverage. |

## 2. Model

### Principles

1. **Role is the ceiling, grants are the floor.** A role says what can ever be granted. A grant says where. Only super admin bypasses grants.

   | Role | Holds | ACL editable by |
   |---|---|---|
   | Super admin | everything, everywhere | nobody (no ACL) |
   | Dev | OTA + shell + every operator action | super admin |
   | Access admin | users + every operator action | super admin |
   | Operator | operator actions | super admin, access admin |
   | Viewer / owner | view (+ screenshot for viewer) | super admin, access admin |

   With no grants a dev, access admin or operator holds their full role everywhere. Grants narrow (deny) or, with base=deny, enumerate (allow) where. Example: an operator with base=deny and one allow rule "remote on devices A, B, C" can open remote control on those three and nothing else. A dev with a rule "deny ota except restaurant X" cannot deploy outside X.
2. **Deny wins.** Any matching deny beats any allow.
3. **OTA and shell are dev-only.** They are in the dev ceiling only and never appear in an operator or access admin grant, no matter who edits it. `remote` is the one sensitive action operators can be granted, and it is explicit-only: never implied by `base=allow` nor by "any action". It must be named in an allow rule.
4. **One choke point.** Every handler that touches a device or a fleet action goes through `access.can`. No handler checks `role ==` for device-level things.
5. **Hidden by default for restricted users.** If a user cannot `view` a device it does not appear anywhere: lists, map, alerts, search, SSE, exports, command history.
6. **Delegation is bounded.** A granter can only give actions and scopes they hold themselves. Admin is exempt.

### Storage: `access_grants` table (replaces the JSONB rules)

```sql
CREATE TABLE access_grants (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  effect        TEXT NOT NULL CHECK (effect IN ('allow','deny')),
  scope_type    TEXT NOT NULL CHECK (scope_type IN ('all','restaurant','group','device')),
  restaurant_id UUID REFERENCES restaurants(id) ON DELETE CASCADE,
  group_id      UUID REFERENCES groups(id)      ON DELETE CASCADE,
  device_id     UUID REFERENCES devices(id)     ON DELETE CASCADE,
  actions       TEXT[] NOT NULL,        -- action keys, or '{*}' (never expands to sensitive ones)
  note          TEXT NOT NULL DEFAULT '',
  expires_at    TIMESTAMPTZ,            -- NULL = permanent
  created_by    UUID REFERENCES users(id) ON DELETE SET NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK ((scope_type='all') = (restaurant_id IS NULL AND group_id IS NULL AND device_id IS NULL)),
  CHECK (scope_type<>'restaurant' OR restaurant_id IS NOT NULL),
  CHECK (scope_type<>'group'      OR group_id      IS NOT NULL),
  CHECK (scope_type<>'device'     OR device_id     IS NOT NULL)
);
CREATE INDEX ON access_grants (user_id);
CREATE INDEX ON access_grants (device_id) WHERE device_id IS NOT NULL;
```

`users.access` keeps only `{base, hide_out_of_scope}`. Existing rules are migrated once at startup (group/restaurant ids that no longer exist are dropped and logged).

Why a table: foreign keys clean up on delete, reverse lookups ("who can shell this device") are one query, expiry is a column, every row has an author.

### Action catalogue

| Key | Level | Group | Sensitive |
|---|---|---|---|
| view | device | Visibility | |
| screenshot, install_apk, uninstall, reboot, query, logcat | device | Commands | |
| kiosk, notes, queue | device | Device | |
| remote | device | Sessions | yes (explicit-only) |
| shell | device | Sessions | dev ceiling only |
| ota | device | Updates | dev ceiling only |
| alerts, groups, qa, deploy_cancel | fleet | Fleet | |

`ota` on a device means: may include this device as a target of a deployment. `ota` with scope `all` means any device. Creating releases, uploading packages, publishing stay admin/dev.

Role ceilings (what may appear in a grant for that role):

- viewer: view, screenshot
- owner: view
- operator, access admin (`user_manager`): every device action incl. remote, plus fleet actions; never shell / ota
- dev: operator ceiling + shell + ota (scope-able: all devices by default, or a restaurant / group / list of serials)
- super admin: not grant-driven

### Evaluation (`can(action, device)`)

```
if role == admin                  -> true
if action not in ceiling(role)    -> false      (shell/ota for anyone but dev)
rules = grants for user, not expired, matching action (or '*' when action != remote)
if any rule matches scope with effect deny  -> false
if any rule matches scope with effect allow -> true
if action == remote               -> false      (explicit-only)
if role is viewer and action == view -> true
return base != deny
```

Scope match for a device: `all` always; `restaurant` when device.restaurant_id equals; `group` when device is a member; `device` when ids equal. Fleet actions match only `all` rules.

Grant editing follows `mayManageUser`: super admin edits anyone's ACL, access admin edits operators / viewers / owners only. The save also rejects any action outside the target's ceiling (an access admin cannot smuggle `shell` into an operator's grant). Grants of the delegating user are checked on save: for non-admin granters every (action, scope) in the new grant must be covered by the granter's own effective access (scope containment: a device scope is covered by any rule covering that device; a group/restaurant scope by an `all` rule or the same group/restaurant).

## 3. Enforcement sweep

### Device routes

Add one wrapper and use it on every `/devices/{serial}/...` route:

```go
func (h *Handler) deviceRoute(action string, next func(w, r, *db.Device)) http.HandlerFunc
```

It loads the device, runs `access.can(action, dev.ID)`, returns 404 (not 403) when the user may not `view` it so hidden devices do not leak by URL, 403 for a held-but-not-granted action. Map of route → action:

- detail, history, chart-data, events, stats, vitals, panel, battery.csv, daily-stats, packages, apps-list, ota-progress, commands-status, alerts-panel, queue, installs, ws-status, presence-stream, ai-analyses → `view`
- logcat/live, logcat/stream → `logcat`
- remote → `remote` (page and ticket issue; `ConnectRemote` validates the ticket is bound to that device and user)
- shell page, shell WS, shell command creation → `shell`
- notes, kiosk, queue, offline-code → existing keys

### List and aggregate routes

Every query that returns devices takes `OnlyIDs` from `visibleIDs()`: device list, search, cmdk index, overview, fleet-health, map + map.json, alerts (+ newest, recent, events SSE, alerts-by-restaurant), export (+ visualize), groups and restaurants (list, detail, members, daily-stats), command list/history/detail/status/events (targets and results filtered; a command with zero visible targets is 404), `events/devices` SSE, wrapped, schedules.

`visibleIDs` becomes a cached per-request set, computed once from the scopes map.

### Commands

- `commandRoles` becomes the role ceiling. Policy then decides per device.
- Group and restaurant targets are narrowed to allowed devices and stored as device targets when narrowed (with an audit line), instead of refused.
- Actions page: every action pill and target row rendered, disabled with the `role-locked` treatment when not allowed for the current selection. Impact endpoint returns allowed/blocked counts.

### OTA

- `POST /updates` and `POST /releases/{id}/deploy` stay on `requireReleaseAdmin` (admin/dev) and additionally filter targets by the dev's `ota` grants. Zero allowed targets → 403.
- New deployment page: device picker shows only devices the dev holds `ota` on.
- Cancel / retry / reboot on a deployment: `ota` on the deployment's devices (retry per device) or `deploy_cancel` fleet action.

### Remote and shell

- Settings kill switches (`RemoteEnabled`, `ShellEnabled`) stay above everything.
- Shell: route guards stay admin/dev; page, WS upgrade and `POST /commands` type shell additionally check the dev's `shell` grant on the device. Shell UI stays hidden for every other role (unchanged).
- Remote: page checks `remote`; ticket carries user id + device id + 60 s expiry; `ConnectRemote` rejects a ticket for another device.

### API key

Admin API key stays unrestricted (it is an admin credential). Out of scope for this plan; noted so nobody expects grants to bind it.

### Cache

Policy cache keyed by user id, TTL 20 s, invalidated on: grant save, role change, user delete, and (because scopes are per request already) nothing else. Group/restaurant membership changes take effect on the next request.

## 4. Editor UI

New page `GET /users/{id}/access` (link from profile and from the Users list). Access admin only (`requireUserManager` + `mayManageUser`).

Layout, top to bottom:

1. **Header**: user bubble, role chip, one sentence ceiling ("Operator: device actions and remote control, per rule." / "Dev: everything an operator has, plus OTA and shell, per rule.").
2. **Base**: allow-then-exclude / deny-then-include, hide out-of-scope toggle. Hidden for viewer and owner (fixed).
3. **Grants table**: one row per grant. Columns: Effect · Scope · Actions · Note · Expires · Added by. Row actions: edit, remove. Sensitive actions show an amber chip.
4. **Add grant** drawer:
   - Scope picker: one search box across restaurants, groups and serials (typeahead via a new `GET /users/access/scope-search?q=`). Chips for the chosen entries. Pasting a list of serials adds many device chips at once. Unknown serials are rejected with the offending values listed.
   - Effect: allow / deny.
   - Actions: grouped checkboxes (Visibility, Commands, Device, Sessions, Updates, Fleet). Only actions inside the target's ceiling are shown (shell / OTA appear for devs only). Items above the granter's own access are disabled with a reason. Remote, shell and OTA require a confirm checkbox "I understand this gives a remote / shell session on N devices".
   - Note (why), optional expiry (date-time, presets 1 day / 1 week / 30 days).
   - Saving with several scope chips creates one grant per chip.
5. **Preview**: "Check a device" input. Type a serial: shows every action as allowed / denied and the rule that decided it. This is the thing that makes the page trustworthy.
6. **Effective summary**: computed sentence list ("Can reboot and screenshot in venue Pier 9 and group Bar tablets. Can open remote control on 3 devices. Cannot see anything else.").

Users list gets a small "Access" column (full / custom / n grants, amber dot when a sensitive grant exists).

Device page (admin and access admins): a "Who has access" panel listing users with a grant covering this device and which actions.

`/users/access` overview becomes a matrix: users × action groups, cells show scope count, click to open.

## 5. Audit and logging

- `user.access.grant` / `user.access.revoke` / `user.access.edit` audit rows with the full grant as detail, actor recorded.
- Every 403 from the policy logs `[access] denied user=… action=… device=… rule=…` and is written to `audit_log` as `access.denied` (rate-limited per user per minute so a scripted loop cannot flood it).
- Grants that expire are swept by the existing cron and logged as `user.access.expired`.

## 6. Tests

- `access_test.go`: table-driven cases for `can()` covering every rule in section 2 (deny wins, remote not implied by `*`, shell/ota refused for non-dev even with an explicit allow row, expiry, ceiling, viewer base, owner base).
- Delegation tests: user_manager cannot grant what they lack.
- Route inventory test: walks `RegisterRoutes`, asserts every `/devices/{serial}` route is registered through `deviceRoute`, every list route calls `visibleIDs`. Fails the build when a new route skips the guard.
- HTTP matrix test on the test instance: for each of {viewer, operator with device grant, operator with restaurant grant, operator with deny, user_manager, admin} hit each route and compare status codes with a golden table. Run via `go test ./internal/dashboard -run Matrix` against a seeded DB.

## 7. Phases

| Phase | Deliverable | Depends on |
|---|---|---|
| 1 | Table, migration from JSONB, engine rewrite, unit tests | |
| 2 | `deviceRoute` wrapper + visibility sweep over every list/SSE/export route | 1 |
| 3 | Shell, remote, OTA grantable; ticket binding; commands narrowing | 2 |
| 4 | Editor page with scope search, preview, delegation checks | 1 |
| 5 | Audit rows, denied logging, expiry sweep, "who has access" panel, matrix page | 3, 4 |
| 6 | Route inventory + HTTP matrix tests, `docs/AUTH_MODEL.md` rewrite | 5 |

Each phase ships on `stage`, then `main`, on its own.

## 8. Decisions (settled 2026-09-07)

1. Hierarchy: super admin → dev (OTA + shell + operator) → access admin (users + operator) → operator (ACL-scoped) → viewer / owner.
2. OTA and shell exist only in the dev ceiling. A dev's OTA / shell can be scoped by rule (all devices, a venue, a group, serials). Never grantable to operators or access admins.
3. Operators get scoped rules for everything else, remote control included. Remote is explicit-only.
4. Delegation bounded to the granter's own access; super admin exempt. Access admin edits operators / viewers / owners; super admin edits devs and access admins.
5. "Any action" never includes remote, shell or OTA.
6. Temporary grants (expiry) in phase 1.
7. A device the user may not view answers 404.
