# UX Polish Audit — Dashboard "blink / not-production-feel"

Branch: `ux-polish-audit`. Date: 2026-07-10. Status: **audit only, no code changed.**

## The complaint

> "The MDM does not feel a polished production product. For example, when updating
> things on the screen I can see the whole page blinking and refreshing."

## Root cause (one sentence)

A prior refactor killed literal *browser* reloads by making the whole app `hx-boost`ed,
but **mutations still POST → `http.Redirect` → htmx re-GETs the page and swaps the entire
`<main>` via `outerHTML`** — and a homegrown skeleton/watchdog layer on top *blanks* `<main>`
at 120 ms and re-injects a skeleton on those redirect-follows. So every action repaints the
whole content area (content → blank → skeleton → content, scroll-jumped to top, no transition).
That sequence reads exactly as "the whole page blinking and refreshing."

The good news: **the fix backbone already exists and is proven** in the releases + fleet
surfaces (`hxReq`, `hxTriggerEvents`, `hxToast`, the SSE `patchRow` path, the `-oob` fragment
template, the `deployment_detail` diff-guard). Almost nothing new needs to be built — the work
is applying existing patterns consistently.

All line references are `server/…`.

---

## 1. Mutation blink — the core complaint (CRITICAL)

Every `<body>` swap is `hx-target="main" hx-swap="outerHTML"` (`templates/layout.html:144`).
Census of `internal/dashboard/handlers.go` (~9k lines):

| Response pattern | Count | Meaning |
|---|---|---|
| `http.Redirect` | 143 | **blink** — full `<main>` repaint (≈40–50 are frequent user mutations) |
| Fragment render in a mutation handler | ~12 | good pattern |
| `204 No Content` | 3 | good pattern, barely used |
| `HX-Trigger` header helper (`hxTriggerEvents`) | exists, **called by 0 handlers** | infra built, unused |
| `hx-swap-oob` in handlers | 0 | only in the `release-oob` template |

**Key finding:** the top offenders *already publish* the matching hub event
(`PublishDeviceUpdate` / `PublishAlertUpdate`) and then redirect *on top of it* — doubling the
work and causing the blink. The cheapest possible fix for those is: return `204` +
`HX-Trigger` instead of the redirect, and let the existing SSE row-patch repaint just the
affected node.

### Top offenders (already publish an event → fix = 204 + trigger)

| Surface | Handler | file:line | Fix |
|---|---|---|---|
| Alert ack/resolve (single) | `setAlertStatus` | handlers.go:3195 | 204; `AlertEvents` SSE already refreshes counts/list |
| Alert bulk ack/resolve/clear | `AlertBulk` / `bulkAlertStatus` / `AlertClearAll` | 3172 / 3228 / 3217 | 204 |
| Device kiosk toggle | `DeviceKioskUpdate` | 10388 | 204 + `device-updated` (device.html already listens) |
| Device poll interval | `DeviceSetPollInterval` | 10330 | 204 + `device-updated` |
| Device → restaurant assign | `DeviceSetRestaurant` | 4602 | 204 + `device-updated` |
| Group add/remove device | `GroupAddDevice` / `GroupRemoveDevice` | 4631 / 4740 | return changed `<tr>` (+ oob count) |
| Bulk assign restaurant | `BulkAssignRestaurant` | 4900 | 204; `patchRow` moves each row live |
| Bulk hide/unhide | `BulkHideDevices` / `BulkUnhideDevices` | 4857 / 4877 | 204; `patchRow` removes hidden cards |
| Single hide/unhide/clear-OTA | `DeviceHide` / `DeviceUnhide` / `DeviceClearOTA` | 4806 / 4822 / 4837 | 204 |
| Bulk kiosk update | `BulkKioskUpdate` | 4977 | 204 |

### Second tier (no live event today → return a fragment, model on `writeReleaseQAResponse`)

- **Settings** — ~30 redirects to `/settings` (`SettingsToggleLegacyCheckin`:8865,
  `SettingsSetCommandExpiry`:8929, `SettingsSetCheckinInterval`:10056, AI/webhook/retention…).
  A toggle full-swapping `/settings` is a very visible blink.
- **Users CRUD** — `UserCreate`:10688, `UserSetRole`:10721, delete:10747 → return the `<tr>`.
- **Productions** — `ProductionDelete`:10611, create:10524.

### Reuse (already in the tree — do **not** rebuild)
`hxReq(r)` (handlers.go:70) · `hxTriggerEvents` (74, currently unused) · `hxToast` (82) ·
`writeReleaseQAResponse` + `release-oob` (6465, the canonical "changed row + OOB fragments"
example) · `deviceToRowJSON` + `FleetEvents`/`patchRow` (2506; devices.html:1104) ·
`DeviceNotesUpdate` (10335, the one device handler that already returns a fragment on
`HX-Request` and falls back to redirect for no-JS).

---

## 2. Navigation plumbing — extra flashes on top of §1 (`templates/layout.html`)

The custom "no-reload navigation" layer (≈ lines 40–140, 630–710) adds its own flashes:

1. **[CRITICAL] Skeleton fires on post-mutation redirects.** The `htmx:beforeRequest` handler
   (112–133) arms a 120 ms timer for *any* GET targeting `main`. htmx's redirect-follow GET
   matches, so on a ~150–400 ms mutation you get: content → `main.innerHTML=''`
   (**unconditional blank**, line 124) → skeleton → `scrollTo(0,0)` → real content. This is the
   mechanism behind the user's exact words. Fix: don't skeletonize redirect-follows; raise the
   threshold to ~300–500 ms; never clear `main` unless it's already empty/stale.
2. **[HIGH] No transition on the `<main>` swap.** Zero View Transitions, zero `transition:true`,
   zero `.htmx-swapping` CSS anywhere. Full-region `outerHTML` is an instant hard cut. Fix:
   `htmx.config.globalViewTransitions=true` (cross-fade) and/or fragment-swap per §1.
3. **[HIGH] `mdmNavFail` → `window.location.href`** on `htmx:abort/responseError/sendError/timeout`
   (106–111, 136–138). The `_navActive` guard is a single shared boolean; fast successive
   navigation (click A then B) can fire A's `htmx:abort` and hard-reload to a stale/other page.
   Fix: key the guard to a specific request id; treat a superseded nav as not-a-failure.
4. **[MEDIUM] Two overlapping hard-reload watchdogs** (8 s at line 132; a second 6 s one at
   689–706) will reload a slow-but-fine request (big fleet page, cold DB). Fix: consolidate,
   raise thresholds, only reload on genuine error not elapsed time.
5. **[MEDIUM] `scrollTo(0,0)` on skeleton inject** (126) jumps the viewport to top even for an
   in-place action performed while scrolled down. Fix: don't scroll-reset on redirect-follows.
6. **[LOW] Progress bar**: CSS keyframe width vs JS width fight (style.css:2946; layout.html
   663–680), and it flashes on *every* request incl. background polls. Fix: one driver; exclude
   polls.
7. **[OK] Theme FOUC** is correctly handled pre-paint (14–25) — no action.

---

## 3. Live-update flicker (SSE → refetch)

The live path is SSE → `CustomEvent` on body → `hx-trigger` refetch (not blind polling; the 6
`every Ns` triggers are stream-drop fallbacks and are fine). **Root enabler of flicker: server-
rendered `{{timeSince}}` inside refetched fragments** makes every response non-idempotent, so
the region swaps on every tick even when nothing changed. Only `deployment_detail.html:234-287`
applies the house anti-flicker guard (beforeSwap diff-guard + client-side relative-time tick);
nothing else got it.

| # | Region | file:line | Severity | Issue / fix |
|---|---|---|---|---|
| 1 | Overview "Open alerts" card | overview.html:249, 317-320 | **HIGH** | Refetches `innerHTML` + **re-runs masonry relayout** on *any* fleet-wide alert change; `{{timeSince}}` rows never byte-match. Likely the most-visible blink. Fix: diff-guard / targeted OOB delta; client-side time. |
| 2 | Device page: 4 fragments on one event | device.html:1448-1456 (stats/vitals/commands/apps) | **HIGH** | All 4 refetch+wholesale-swap per `device-updated`; embedded `{{timeSince}}` → repaint every event (~15×/min on a flapping charger); resets scroll/collapses app list. Fix: patch hero vitals from the JSON payload already sent (`deviceEventPayload`, handlers.go:2555) like the fleet `patchRow`; diff-guard the rest. |
| 3 | Command detail deliveries | command_detail.html:7-13 | MEDIUM | `outerHTML` swap of full list with churning per-row times; scroll reset. Fix: diff-guard + extend existing `data-since-ms` tick to rows. |
| 4 | Logcat entries panel | logcat.html:83-89 | MEDIUM | `outerHTML` of whole panel; open `<details>` collapse. Fix: append-only / diff-guard. |
| 5 | `ai_report_card` | ai_report_card.html:22-25 | LOW | Refetches a large hourly-changing card on every alert-count change. Fix: drop `mdm:alerts-update` from its trigger. |

Reference model to copy: `devices.html:1100-1155` (`patchRow`) surgically updates only changed
nodes and preserves selection. Also: SSE-driven refetches are **not** gated on `document.hidden`
(the `every Ns` pollers are) — they repaint background tabs; worth gating.

---

## 4. Visual polish & consistency

The toolkit is already built (theme, design tokens, toasts, skeletons, submit-spinner, focus
ring). The unpolished *feel* is **coverage + consistency** — some surfaces are lovingly animated
while shared primitives hard-cut.

| # | Item | file:line | Severity | Fix |
|---|---|---|---|---|
| 1 | `<main>` nav is a hard cut (dup of §2.2) | layout.html:144 | HIGH | swap/settle fade or View Transitions, behind `prefers-reduced-motion` |
| 2 | Base modal hard-appears, **no box-shadow**, smallest radius | style.css:1850-1873 | HIGH | fade backdrop, rise+scale box (reuse `ov-rise`), add `--shadow-pop`, bigger radius |
| 3 | Buttons have no press feedback | style.css:1570 | MED | `:active{transform:translateY(1px)/scale(.98)}` on the button family |
| 4 | Most htmx buttons show no pending state (spinner only wired to real form submits; `hx-indicator` used in 1 template) | layout.html:723; style.css:2998 | MED | generic `.btn.htmx-request` spinner rule |
| 5 | Toast system solid but used in ~2 templates | layout.html:925-982 | MED | emit `HX-Trigger` toasts from mutating handlers (client plumbing exists) |
| 6 | Global `:focus{outline:none}` + several components strip the ring too | style.css:206 | MED | ensure every interactive control keeps a `:focus-visible` ring |
| 7 | Radius scale defined (`--r-1..4`) but sheet uses raw 4–16px + two pill values (99/999) | style.css:99 | MED | collapse to tokens; one pill radius |
| 8 | Durations/shadows hardcoded, bypass `--dur*`/`--ease`/shadow tokens | style.css:103, ~16 sites | LOW | route through tokens; 2–3 elevation tokens |
| 9 | Heavy inline styles (settings 133, release_workspace 60, commands 46) | — | LOW | migrate to classes as areas are touched |

---

## Proposed remediation roadmap

Ordered by perceived-quality-per-effort. Each phase is independently shippable + verifiable
(server on throwaway pg + headless chromium — see the verify harness).

- **Phase 1 — Kill the mutation blink (biggest win).** Convert the §1 top-tier offenders
  (alerts, device toggles, bulk device ops) from redirect → `204 + hxTriggerEvents` using the
  existing hub events + SSE row-patch. Wire `hxTriggerEvents`/`hxToast` (already written, unused).
- **Phase 2 — Fix the nav plumbing flashes.** Don't skeletonize redirect-follows + don't
  unconditionally clear `main` + don't `scrollTo(0,0)` on them (§2.1, §2.5); add View
  Transitions / a settle fade to the `main` swap (§2.2 / §4.1); consolidate & de-fang the two
  hard-reload watchdogs and the `htmx:abort` reload (§2.3, §2.4).
- **Phase 3 — Stop live-update flicker.** Standardize on client-side relative time (kill
  `{{timeSince}}` in refetched fragments) + apply the `deployment_detail` diff-guard to overview
  alerts, device page, command detail, logcat (§3). Patch device hero from the existing JSON
  payload instead of refetching.
- **Phase 4 — Second-tier mutations → fragments.** Settings toggles, users, productions return
  targeted fragments (model on `writeReleaseQAResponse`) (§1 second tier).
- **Phase 5 — Visual consistency pass.** Base modal, button press + pending states, toast
  coverage, focus-ring audit, radius/duration/shadow token consolidation (§4).

**Quick wins (small diffs, high visibility):** Phase 1 alert-ack + device-toggle 204s;
§2.1 skeleton guard; §4.1 main-swap fade; §4.2 modal polish; §3.1 overview-alerts diff-guard.
