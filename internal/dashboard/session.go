package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	_ "image/gif"
	_ "image/jpeg"
	"log"
	"net/http"
	"time"

	"mdm/internal/db"

	"github.com/google/uuid"
)

// loginMaxFailures is the number of failed login attempts (per IP or per
// account) within the limiter window before further attempts are refused.
const loginMaxFailures = 8

// sessionIdleTimeout logs out a session that has gone this long without an
// authenticated request, independent of the absolute lifetime
// (cfg.SessionTimeout(), default 24h). Activity slides last_seen forward
// (touchSession).
const sessionIdleTimeout = 24 * time.Hour

// currentSession resolves the server-side session referenced by the request
// cookie, enforcing both the absolute expiry and the idle timeout. An invalid,
// expired, or idle-timed-out session is deleted and reported as absent. This
// does a primary-key lookup per call; the dashboard is low-traffic, so the few
// calls per request are acceptable.
// ctxSessionKey lets a handler inject a synthetic session for the duration of one
// request (see SneakPeek), so the session-derived chrome (role, name, access)
// renders without a real cookie/DB session. Only ever set server-side.
type ctxSessionKey struct{}

func (h *Handler) currentSession(r *http.Request) (*db.Session, bool) {
	if s, ok := r.Context().Value(ctxSessionKey{}).(*db.Session); ok {
		return s, true
	}
	cookie, err := h.store.Get(r, "mdm-session")
	if err != nil {
		return nil, false
	}
	sid, _ := cookie.Values["sid"].(string)
	if sid == "" {
		return nil, false
	}
	s, err := h.db.GetSession(r.Context(), sid)
	if err != nil {
		return nil, false
	}
	now := time.Now()
	if now.After(s.ExpiresAt) || now.Sub(s.LastSeen) > sessionIdleTimeout {
		_ = h.db.DeleteSession(r.Context(), sid)
		return nil, false
	}
	return s, true
}

// touchSession slides the session's last_seen forward so an active user is not
// idle-timed-out mid-use. Called once per authenticated request by the route
// guards. Best-effort; never blocks the request.
func (h *Handler) touchSession(r *http.Request) {
	cookie, err := h.store.Get(r, "mdm-session")
	if err != nil {
		return
	}
	if sid, _ := cookie.Values["sid"].(string); sid != "" {
		_ = h.db.TouchSession(r.Context(), sid)
	}
}

// newSessionID returns a 256-bit random opaque session identifier.
func newSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// startSession creates a server-side session row and writes its id into the
// signed cookie. userID is nil for the static env-configured admin.
func (h *Handler) startSession(w http.ResponseWriter, r *http.Request, userID *uuid.UUID, username, role string) error {
	sid, err := newSessionID()
	if err != nil {
		return err
	}
	if err := h.db.CreateSession(r.Context(), db.Session{
		ID:        sid,
		UserID:    userID,
		Username:  username,
		Role:      role,
		ExpiresAt: time.Now().Add(time.Duration(h.cfg.SessionTimeout()) * time.Second),
	}); err != nil {
		return err
	}
	cookie, _ := h.store.Get(r, "mdm-session")
	cookie.Values = map[interface{}]interface{}{"sid": sid}
	return cookie.Save(r, w)
}

func (h *Handler) isAdmin(r *http.Request) bool {
	s, ok := h.currentSession(r)
	return ok && s.Role == "admin"
}

// isAuthenticated is kept for compatibility; use isAdmin for admin-only checks.
func (h *Handler) isAuthenticated(r *http.Request) bool { return h.isAdmin(r) }

func (h *Handler) isLoggedIn(r *http.Request) bool {
	_, ok := h.currentSession(r)
	return ok
}

func (h *Handler) role(r *http.Request) string {
	if s, ok := h.currentSession(r); ok {
		return s.Role
	}
	return ""
}

func (h *Handler) currentUsername(r *http.Request) string {
	if s, ok := h.currentSession(r); ok {
		return s.Username
	}
	return ""
}

// auditAs is h.audit with an explicit actor, for the sign-up/verify-email flows
// where the action happens before any session exists — h.audit's session-derived
// actor would otherwise fall back to "unknown".
func (h *Handler) auditAs(r *http.Request, actor, action, target, detail string) {
	if actor == "" {
		actor = "unknown"
	}
	if err := h.db.InsertAudit(r.Context(), actor, action, target, detail); err != nil {
		log.Printf("[audit] insert failed: %v", err)
	}
}

// currentDisplayName is currentUsername, but prefers the user's first/last name
// (set at sign-up or by an admin) over their bare username/email when one is on
// file — shown top-right in the dashboard chrome and snapshotted onto commands
// and audit entries so "who did this" reads as a name where possible.
func (h *Handler) currentDisplayName(r *http.Request) string {
	s, ok := h.currentSession(r)
	if !ok {
		return ""
	}
	if s.UserID != nil {
		if u, err := h.db.GetUser(r.Context(), *s.UserID); err == nil && u != nil {
			return u.DisplayName()
		}
	}
	if s.Username == "admin" {
		return builtinAdminDisplayName
	}
	return s.Username
}

// builtinAdminDisplayName is shown for the config-defined "admin" login, which has
// no users row (and so no first/last name) to draw a display name from.
const builtinAdminDisplayName = "Ali"

// audit records an admin action (best-effort; never blocks the request).
//
// The actor is the user's stable username (their login identity), not their
// display name — a display name is editable from the Users page, and a
// snapshotted name would freeze at whatever it was on the day of the action.
// Storing the username lets every render path resolve it to the user's
// CURRENT display name (see actorDisplayNames), so a rename shows up on
// every past action too, not just new ones.
// boolWord renders a toggle's new state as the word an activity line reads with:
// "enabled" / "disabled", not "true" / "false".
func boolWord(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}

// auditDev records an action against a device. Same as audit, but the entry can then
// be found by device rather than by hoping `target` holds a serial for this particular
// action — it holds a comma-joined list for bulk actions and a deployment id for
// others, which is why the device page could not show its own history before.
func (h *Handler) auditDev(r *http.Request, action string, deviceID uuid.UUID, target, detail string) {
	actor := h.currentUsername(r)
	if actor == "" {
		actor = "unknown"
	}
	if err := h.db.InsertAuditDevice(r.Context(), actor, action, target, detail, deviceID); err != nil {
		log.Printf("[audit] insert failed: %v", err)
	}
}

func (h *Handler) audit(r *http.Request, action, target, detail string) {
	actor := h.currentUsername(r)
	if actor == "" {
		actor = "unknown"
	}
	if err := h.db.InsertAudit(r.Context(), actor, action, target, detail); err != nil {
		log.Printf("[audit] insert failed: %v", err)
	}
}

// actorDisplayNames maps every user's stable username to their current display
// name, for resolving the username snapshotted onto commands/audit_log rows
// (see audit and the CreateCommandBy call sites) back to a human name at
// render time — live, so a Users-page rename is reflected on every past
// action instead of being frozen at whatever the name was when it was logged.
func (h *Handler) actorDisplayNames(ctx context.Context) map[string]string {
	users, err := h.db.ListUsers(ctx)
	if err != nil {
		return nil
	}
	m := make(map[string]string, len(users))
	for _, u := range users {
		m[u.Username] = u.DisplayName()
	}
	return m
}

// resolveCommandActors rewrites each command's CreatedBy (a snapshotted
// username, or "" for system-initiated) to the user's current display name,
// in place. Falls back to leaving CreatedBy as-is when it doesn't match a
// known username (a pre-rename-fix row already carrying a snapshotted display
// name, or a non-account actor like "Scheduled recipe: ...").
func (h *Handler) resolveCommandActors(ctx context.Context, cmds []db.Command) {
	m := h.actorDisplayNames(ctx)
	if m == nil {
		return
	}
	for i := range cmds {
		if dn, ok := m[cmds[i].CreatedBy]; ok {
			cmds[i].CreatedBy = dn
		}
	}
}
