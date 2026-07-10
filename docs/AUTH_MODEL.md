# AIO MDM — Authentication & Privilege Model

**Audience:** penetration-testing / security review team
**Companion doc:** [`API_REFERENCE.md`](./API_REFERENCE.md) · [`openapi.yaml`](./openapi.yaml)

This describes the auth model **as implemented**, including where controls are intentionally weak or absent, so it can be probed directly.

---

## 1. Trust boundaries — the five surfaces

| # | Surface | Credential | Where enforced | Granularity |
|---|---|---|---|---|
| 1 | Device API | `X-API-Key: <DEVICE_API_KEY>` | `middleware.DeviceAPIKeyAuth` | Flat — one shared key for the whole fleet |
| 2 | Admin REST API | `X-API-Key: <ADMIN_API_KEY>` | `middleware.AdminAPIKeyAuth` | Flat — one key, full admin power |
| 3 | Remote-control WS | Single-use token (query string) | in `apiHandler.ConnectRemote`, **no middleware** | Per-session token, device-bound |
| 4 | Dashboard | Cookie session (`mdm-session`) | `requireAuth` + role wrappers | **RBAC — 6 roles** (§4) |
| 5 | Public | none | — | `/health`, `/static`, `/splash-img`, `/login` |

Key facts to internalise:

- **The two API keys are shared secrets, not per-principal identities.** There is no per-device credential and no per-admin identity on the REST API. Anyone with `DEVICE_API_KEY` can impersonate any device (subject to `serial` validation); anyone with `ADMIN_API_KEY` has unrestricted admin REST access, including fleet-wide shell command execution.
- **RBAC exists only on the dashboard** (surface 4), not on the REST API (surfaces 1–2). Do not assume the dashboard's role checks protect the underlying data — the same mutations are reachable via the admin key with no role.
- Both API-key classes deliberately return an **identical `{"error":"unauthorized"}`** body so a caller cannot tell which key they got wrong (referred to in code as control F-07).

---

## 2. Credentials & configuration

| Env var | Purpose | Notes |
|---|---|---|
| `DEVICE_API_KEY` | Device API shared key | **required** |
| `ADMIN_API_KEY` | Admin REST shared key | **required** |
| `DASHBOARD_USER` | Dashboard bootstrap username | default `admin` |
| `DASHBOARD_PASSWORD` | Dashboard bootstrap password | **required**; plaintext env compare (see §5) |
| `SESSION_SECRET` | Cookie signing key | **falls back to `DEVICE_API_KEY`** if unset; must be ≥ 32 bytes or the server refuses to start |
| `COOKIE_SECURE` | Toggles the `Secure` cookie flag | `Secure` is on unless `COOKIE_SECURE == "false"` |
| `CONFIG_PATH` | Display/config JSON path | — |

> ⚠ **`SESSION_SECRET` defaulting to `DEVICE_API_KEY`** couples two secrets: a leaked device key also lets an attacker forge dashboard session cookies. Recommend an independent `SESSION_SECRET` in production and confirm which is set in the target env.

---

## 3. API-key auth mechanics (surfaces 1 & 2)

Implemented in `internal/middleware/auth.go`:

- Header `X-API-Key` compared to the configured key with **`crypto/subtle.ConstantTimeCompare`** (timing-safe).
- **Per-source-IP failure throttle:** `apiMaxFailPerMin = 30` failures per rolling minute → `429 {"error":"rate limited"}` with a `Retry-After` header. Successful requests are never counted; only failures.
- Both `DeviceAPIKeyAuth` and `AdminAPIKeyAuth` wrap the same `APIKeyAuth` and return identical `401` bodies.

> ⚠ **Rate-limit source = `ratelimit.ClientIP(r)`**, which honours `X-Forwarded-For`. If the server is reachable without a trusted reverse proxy that overwrites that header, an attacker can rotate `X-Forwarded-For` to defeat both the 30/min API throttle and the 8/15min login throttle (§5). Validate the deployment's proxy chain.

---

## 4. Dashboard RBAC (surface 4)

Roles (session `Role` field): **admin, dev, operator, tester, viewer** (plus a "strict admin" distinction applied to settings/user-management). Route guards in `internal/dashboard/handlers.go`:

| Wrapper | Allowed roles | Governs |
|---|---|---|
| `requireAuth` | any logged-in user | Read views: fleet, device detail, alerts, command history, exports |
| `requireOperatorOrAdmin` | admin, dev, operator, tester | Ack/resolve alerts, edit notes, bulk ops |
| `requireAdmin` | admin, dev | Device commands, hide/unhide, releases, OTA |
| `requireAdminOrTester` | admin, dev, tester | Create groups/restaurants, assign devices, kiosk config |
| `requireStrictAdmin` | **admin only** | Settings, **user management / role changes**, boot logo, changelog |
| `requireDev` | dev | Release sign-off |
| `requireTester` | tester | Record QA test results |

Auth-failure behaviour:
- **Not logged in →** `302` redirect to `/login` (no body disclosure).
- **Logged in but wrong role →** `403 Forbidden` (plaintext). No `401`, so "no session" and "insufficient privilege" are not distinguished.

Pen-test focus: vertical privilege escalation between roles (e.g. operator reaching `requireAdmin` mutations), and whether the same effect is reachable unprotected via the admin REST key.

---

## 5. Session & login

**Session cookie** (`gorilla/sessions.CookieStore`):
- Name `mdm-session`; `HttpOnly: true`; `SameSite: Lax`; `Path: /`; `Secure: true` unless `COOKIE_SECURE=false`.
- Signed with `SESSION_SECRET`. **Sessions are also stored server-side in the DB** (not self-contained JWTs) — the cookie carries a session ID; validity, role, and expiry are looked up server-side and can be revoked by deleting the row.
- **Dual timeout:** absolute expiry `ExpiresAt` (default `SessionTimeout()` = 86400 s / 24 h) **and** idle timeout (`sessionIdleTimeout = 24 h` since `LastSeen`). Every authenticated request calls `touchSession` to slide the idle window; expired sessions are deleted on next access.

**Login flow** (`POST /login`):
- Env credentials checked with **`subtle.ConstantTimeCompare`** on both username and password.
- Fallback to DB users via **bcrypt** (`bcrypt.CompareHashAndPassword`).
- **Rate limit:** `loginMaxFailures = 8` within a `15 * time.Minute` window, keyed by **both** source IP **and** account (`"u:"+username`) — hitting the threshold on *either* blocks (`429` + `Retry-After`). Both counters reset on successful login. No permanent lockout; the window simply expires.

---

## 6. CSRF

- **No CSRF tokens exist.** Search for `csrf` returns none.
- Mitigation is **same-origin enforcement** (`enforceSameOrigin`) wrapping every dashboard `POST`/`DELETE`:
  1. `Sec-Fetch-Site` must be empty, `same-origin`, `same-site`, or `none` — a positive cross-site signal is rejected.
  2. `Origin` (when present) must match configured `publicOrigins` or the request `Host`.
  - Legacy clients sending **neither** header are **allowed** (fail-open for old browsers).
- Secondary defence: `SameSite=Lax` on the session cookie.
- The REST API (`/api/v1/*`) is intentionally **not** wrapped — it authenticates with a header key that browsers don't auto-attach, so it isn't CSRF-able.

Pen-test focus: the fail-open path when both fetch-metadata and `Origin` are absent, and whether any state-changing action is a `GET`.

---

## 7. Security headers (`middleware.SecurityHeaders`)

Applied to all responses (WebSocket upgrades bypass this to avoid breaking the handshake):

| Header | Value |
|---|---|
| `Strict-Transport-Security` | `max-age=63072000; includeSubDomains; preload` (2 years) |
| `X-Content-Type-Options` | `nosniff` |
| `X-Frame-Options` | `DENY` |
| `Referrer-Policy` | `strict-origin-when-cross-origin` |
| `Permissions-Policy` | `geolocation=(), camera=(), microphone=(), usb=(), payment=()` |
| `Content-Security-Policy` | `default-src 'self'`; `script-src 'self' 'unsafe-inline'`; `style-src 'self' 'unsafe-inline'`; `img-src 'self' data:`; `font-src 'self'`; `connect-src 'self'`; `frame-ancestors 'none'`; `base-uri 'self'`; `form-action 'self'`; `object-src 'none'` |
| `Cache-Control` | `no-store` (dynamic responses; static/splash override to immutable) |

> ⚠ **CSP allows `'unsafe-inline'` for scripts and styles.** Inline scripts are used pending a nonce refactor — this weakens XSS containment. Any reflected/stored XSS in the dashboard is not mitigated by CSP today.

---

## 8. Summary of known-weak / by-design items to probe

1. **Shared API keys, no per-principal identity** (surfaces 1–2); admin key = fleet-wide shell RCE.
2. **`SESSION_SECRET` may equal `DEVICE_API_KEY`** — cookie-forgery risk if the device key leaks.
3. **`X-Forwarded-For`-based rate limiting** — spoofable without a trusted proxy; defeats API + login throttles.
4. **`/api/v1/remote/{serial}`** — only key-free endpoint; token entropy/lifetime/binding/leakage.
5. **No CSRF tokens**; fail-open when fetch-metadata + `Origin` both absent.
6. **CSP `unsafe-inline`** — XSS not contained.
7. **Dashboard RBAC vs API flatness** — confirm sensitive dashboard-gated actions aren't trivially reachable via the admin key without a role.
8. **`/splash-img/` unauthenticated** — UUID predictability / IDOR.
9. **WS `default` frame → shell manager** — verify device isolation on the shell channel.
