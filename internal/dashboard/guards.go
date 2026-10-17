package dashboard

import (
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"net/url"
)

func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.isLoggedIn(r) {
			redirectLogin(w, r)
			return
		}
		if h.maintenanceGate(w, r) {
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

// requireReleaseAdmin guards the release / OTA / deployment / QA-catalog routes:
// super admin only since the dev role was retired (2026-10-07).
func (h *Handler) requireReleaseAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			redirectLogin(w, r)
			return
		}
		if s.Role != "admin" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if h.maintenanceGate(w, r) {
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

func (h *Handler) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// An anonymous caller is redirected to login like any other protected
		// route, so admin-only paths don't stand out with a 403 (F-07). A
		// logged-in but non-admin user gets a genuine 403.
		s, ok := h.currentSession(r)
		if !ok {
			redirectLogin(w, r)
			return
		}
		if s.Role != "admin" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if h.maintenanceGate(w, r) {
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

// requireAdminOrOperator extends requireAdmin (admin/dev) to also allow operators, for
// the fleet-organisation tasks the test team owns: creating groups and restaurants
// and setting locked mode. Viewers and operators still can't reach these.
func (h *Handler) requireAdminOrOperator(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			redirectLogin(w, r)
			return
		}
		if !roleCanOperate(s.Role) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if h.maintenanceGate(w, r) {
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

// requireStrictAdmin guards the two areas a dev must never reach: settings and
// user management. Only the env-configured "admin" passes.
// requireUserManager guards the Users pages: admin or user_manager. Per-target
// elevation checks (who may edit whom) live in the handlers via mayManageUser.
// requireOTA guards firmware pushes: admin, dev, or the super op (who may deploy
// releases but not manage the release packages themselves).
// roleAboveOperator is true for the roles ranked above operator in roleLevels:
// access admin, super op, dev and admin.
func roleAboveOperator(role string) bool {
	return roleLevels[role] > roleLevels["operator"]
}

// roleCanAppLibrary: the app library (browse, upload, register) is open to the
// roles that push software: admin, dev and super op. Removing an APK is admin-only.
func roleCanAppLibrary(role string) bool {
	return role == "admin" || role == "super_op"
}

// requireAppLibrary guards the app library page and its upload endpoints.
func (h *Handler) requireAppLibrary(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			redirectLogin(w, r)
			return
		}
		if !roleCanAppLibrary(s.Role) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (h *Handler) requireOTA(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			redirectLogin(w, r)
			return
		}
		if !roleCanOTA(s.Role) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// requireAccountAdmin guards account creation / deletion / merge: admin or access
// admin. A super op may open the Users pages and edit rules but not add accounts.
func (h *Handler) requireAccountAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			redirectLogin(w, r)
			return
		}
		if !roleCreatesUsers(s.Role) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (h *Handler) requireUserManager(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			redirectLogin(w, r)
			return
		}
		if !roleManagesUsers(s.Role) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if h.maintenanceGate(w, r) {
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

func (h *Handler) requireStrictAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			redirectLogin(w, r)
			return
		}
		if s.Role != "admin" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if h.maintenanceGate(w, r) {
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

// requireDev gates the release sign-off ("tested at our end") to the super admin —
// the dev role it was named for was folded into super op on 2026-10-07.
func (h *Handler) requireDev(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			redirectLogin(w, r)
			return
		}
		if s.Role != "admin" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if h.maintenanceGate(w, r) {
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

func (h *Handler) requireOperatorOrAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			redirectLogin(w, r)
			return
		}
		if !roleCanOperate(s.Role) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if h.maintenanceGate(w, r) {
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

// requireOperatorRole gates QA result-marking to the operator role only. Managing test
// cases stays admin-only; recording pass/fail is the operator's job and nobody
// else's (viewer/operator/admin all see results read-only).
func (h *Handler) requireOperatorRole(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			redirectLogin(w, r)
			return
		}
		if !roleIsOperatorLike(s.Role) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if h.maintenanceGate(w, r) {
			return
		}
		h.touchSession(r)
		next(w, r)
	}
}

// enforceSameOrigin wraps a state-changing dashboard handler and rejects
// requests that originate cross-site, mitigating CSRF (GB-03). Dashboard auth
// is a cookie the browser attaches automatically, so a malicious page could
// otherwise drive any POST (e.g. issuing a device command or logging the user
// out). The device/admin JSON API under /api/v1/* is authenticated by an
// X-API-Key header rather than an ambient cookie, so it is not CSRF-able and is
// intentionally not wrapped.
//
// The check uses two independent signals and blocks only when one positively
// indicates a cross-origin request, so legacy clients that send neither header
// still work:
//   - Sec-Fetch-Site (fetch metadata): must be same-origin, same-site, or none
//     when present. same-site is accepted because the deployment is reachable
//     over more than one subdomain of the same site; a real cross-subdomain
//     forgery is still caught by the Origin check below, which requires the
//     Origin host to match publicOrigin (or the request Host). Only a positive
//     cross-site signal is rejected here.
//   - Origin: must match publicOrigin (or the request Host) when present.
func (h *Handler) enforceSameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "same-origin", "same-site", "none":
			// allowed, or header absent — fall through to the Origin check
		default:
			http.Error(w, "cross-site request blocked", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !h.originAllowed(origin, r) {
			http.Error(w, "cross-origin request blocked", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// originAllowed reports whether an Origin header value is this site. When
// publicOrigin is configured it must match exactly; otherwise the Origin's host
// must equal the request Host (works behind a TLS-terminating proxy).
func (h *Handler) originAllowed(origin string, r *http.Request) bool {
	if len(h.publicOrigins) > 0 {
		for _, o := range h.publicOrigins {
			if origin == o {
				return true
			}
		}
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}
