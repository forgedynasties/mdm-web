package dashboard

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"log"
	"net/http"
	"strings"
	"time"

	"mdm/internal/mailer"
	"mdm/internal/ratelimit"

	"golang.org/x/crypto/bcrypt"
)

func (h *Handler) LoginPage(w http.ResponseWriter, r *http.Request) {
	if h.isLoggedIn(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	h.tmpl.ExecuteTemplate(w, "login.html", map[string]any{"Brand": h.cfg.CustomBrand(), "AssetVer": h.assetVer})
}

func (h *Handler) LoginSubmit(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	user := r.FormValue("username")
	pass := r.FormValue("password")

	// Refuse further attempts once either the source IP or the targeted account
	// has accumulated too many recent failures (F-01).
	ip := ratelimit.ClientIP(r)
	userKey := "u:" + user
	for _, key := range []string{ip, userKey} {
		if n, retry := h.loginFails.Count(key); n >= loginMaxFailures {
			mins := int(retry.Minutes()) + 1
			w.WriteHeader(http.StatusTooManyRequests)
			h.tmpl.ExecuteTemplate(w, "login.html", map[string]any{
				"Error": fmt.Sprintf("Too many failed attempts. Try again in %d minute(s).", mins),
				"Brand": h.cfg.CustomBrand(),
			})
			return
		}
	}

	loginOK := func() {
		h.loginFails.Reset(ip)
		h.loginFails.Reset(userKey)
	}

	// Accounts are DB users only; there is no built-in login in the environment.
	dbUser, err := h.db.GetUserByUsername(r.Context(), user)
	if err == nil {
		if bcrypt.CompareHashAndPassword([]byte(dbUser.PasswordHash), []byte(pass)) == nil {
			// Self-signup accounts start with email_verified_at NULL until the emailed
			// link is clicked; admin-created accounts are pre-verified at creation time
			// (or have no email at all), so this never blocks them.
			if dbUser.Email != nil && dbUser.EmailVerifiedAt == nil {
				h.tmpl.ExecuteTemplate(w, "login.html", map[string]any{
					"Error": "Please verify your email before signing in — check your inbox for the link.",
					"Brand": h.cfg.CustomBrand(),
				})
				return
			}
			loginOK()
			uid := dbUser.ID
			if err := h.startSession(w, r, &uid, dbUser.Username, dbUser.Role); err != nil {
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	// Failed attempt — count it against both the IP and the account.
	h.loginFails.Hit(ip)
	h.loginFails.Hit(userKey)
	h.tmpl.ExecuteTemplate(w, "login.html", map[string]any{"Error": "Invalid credentials", "Brand": h.cfg.CustomBrand()})
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	// Delete the server-side session so the cookie is dead immediately, not just
	// dropped by the browser (GB-04).
	session, _ := h.store.Get(r, "mdm-session")
	if sid, _ := session.Values["sid"].(string); sid != "" {
		_ = h.db.DeleteSession(r.Context(), sid)
	}
	session.Options.MaxAge = -1
	session.Save(r, w)
	http.Redirect(w, r, "/", http.StatusFound) // the public landing page
}

// newToken returns a random, unguessable, URL-safe token for the email-verify and
// password-reset links (32 bytes of entropy, hex-encoded).
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// signupEmailDomain restricts self-service sign-up to the internal team.
const signupEmailDomain = "@aioapp.com"

func (h *Handler) SignupPage(w http.ResponseWriter, r *http.Request) {
	if h.isLoggedIn(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	h.tmpl.ExecuteTemplate(w, "signup.html", map[string]any{"Brand": h.cfg.CustomBrand(), "AssetVer": h.assetVer})
}

// SignupSubmit creates a new viewer-role account gated on an @aioapp.com email and
// emails a verify link via Resend. The account can't log in until that link is
// clicked (see LoginSubmit's EmailVerifiedAt check).
func (h *Handler) SignupSubmit(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))
	password := r.FormValue("password")
	confirm := r.FormValue("confirm")
	firstName := strings.TrimSpace(r.FormValue("first_name"))
	lastName := strings.TrimSpace(r.FormValue("last_name"))

	fail := func(msg string) {
		h.tmpl.ExecuteTemplate(w, "signup.html", map[string]any{
			"Error": msg, "Email": email, "FirstName": firstName, "LastName": lastName, "Brand": h.cfg.CustomBrand(),
		})
	}

	if firstName == "" || lastName == "" {
		fail("First and last name are required.")
		return
	}

	ip := ratelimit.ClientIP(r)
	for _, key := range []string{ip, "e:" + email} {
		if n, retry := h.signupAttempts.Count(key); n >= 5 {
			mins := int(retry.Minutes()) + 1
			w.WriteHeader(http.StatusTooManyRequests)
			fail(fmt.Sprintf("Too many sign-up attempts. Try again in %d minute(s).", mins))
			return
		}
	}
	h.signupAttempts.Hit(ip)
	h.signupAttempts.Hit("e:" + email)

	if !strings.HasSuffix(email, signupEmailDomain) {
		fail("Sign-up is limited to " + signupEmailDomain + " email addresses.")
		return
	}
	if len(password) < 6 {
		fail("Password must be at least 6 characters.")
		return
	}
	if password != confirm {
		fail("Passwords don't match.")
		return
	}
	if _, err := h.db.GetUserByEmail(r.Context(), email); err == nil {
		fail("An account with that email already exists.")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		fail("Internal error, please try again.")
		return
	}
	u, err := h.db.CreateUserNamed(r.Context(), email, string(hash), "viewer", &email, false, firstName, lastName)
	if err != nil {
		fail("An account with that email already exists.")
		return
	}

	token, err := newToken()
	if err == nil {
		if err := h.db.CreateUserToken(r.Context(), token, u.ID, "verify", time.Now().Add(24*time.Hour)); err == nil {
			verifyURL := h.baseURL(r) + "/verify-email?token=" + token
			if err := h.mail.Send(r.Context(), email, "Confirm your email", mailer.VerifyEmailHTML(verifyURL)); err != nil {
				log.Printf("signup: send verify email to %s: %v", email, err)
			}
		} else {
			log.Printf("signup: create verify token for %s: %v", email, err)
		}
	} else {
		log.Printf("signup: generate verify token for %s: %v", email, err)
	}

	// Username (== email here), not the display name — see h.audit for why the
	// actor field stores the stable identifier rather than a name snapshot.
	h.auditAs(r, u.Username, "user.signup", u.ID.String(), email)
	h.tmpl.ExecuteTemplate(w, "signup.html", map[string]any{
		"Sent": true, "Email": email, "Brand": h.cfg.CustomBrand(),
	})
}

// VerifyEmailPage renders a confirm button rather than consuming the token
// immediately on GET — see db.PeekUserToken for why (mail-client link
// prefetching would otherwise silently burn the token before the user's real
// click). Read-only: PeekUserToken doesn't mark anything used.
func (h *Handler) VerifyEmailPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if _, err := h.db.PeekUserToken(r.Context(), token, "verify"); err != nil {
		h.tmpl.ExecuteTemplate(w, "verify_email.html", map[string]any{
			"Invalid": true, "Brand": h.cfg.CustomBrand(), "AssetVer": h.assetVer,
		})
		return
	}
	h.tmpl.ExecuteTemplate(w, "verify_email.html", map[string]any{
		"Token": token, "Brand": h.cfg.CustomBrand(), "AssetVer": h.assetVer,
	})
}

// VerifyEmailSubmit does the actual token consumption — only reachable via the
// confirm page's POST, which an automated prefetch/scanner never submits.
func (h *Handler) VerifyEmailSubmit(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	token := r.FormValue("token")
	userID, err := h.db.ConsumeUserToken(r.Context(), token, "verify")
	if err != nil {
		h.tmpl.ExecuteTemplate(w, "verify_email.html", map[string]any{
			"Invalid": true, "Brand": h.cfg.CustomBrand(),
		})
		return
	}
	if err := h.db.SetUserEmailVerified(r.Context(), userID); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	verifyActor := userID.String()
	if u, err := h.db.GetUser(r.Context(), userID); err == nil && u != nil {
		verifyActor = u.Username
	}
	h.auditAs(r, verifyActor, "user.email_verified", userID.String(), "")
	http.Redirect(w, r, "/login?verified=1", http.StatusFound)
}

func (h *Handler) ForgotPasswordPage(w http.ResponseWriter, r *http.Request) {
	h.tmpl.ExecuteTemplate(w, "forgot_password.html", map[string]any{"Brand": h.cfg.CustomBrand(), "AssetVer": h.assetVer})
}

// ForgotPasswordSubmit emails a reset link when the address matches a user, but
// always shows the same generic response either way (no account enumeration).
func (h *Handler) ForgotPasswordSubmit(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))

	sent := func() {
		h.tmpl.ExecuteTemplate(w, "forgot_password.html", map[string]any{"Sent": true, "Brand": h.cfg.CustomBrand(), "AssetVer": h.assetVer})
	}

	ip := ratelimit.ClientIP(r)
	for _, key := range []string{ip, "e:" + email} {
		if n, _ := h.resetAttempts.Count(key); n >= 5 {
			sent() // still generic — don't reveal rate limiting to a prober either
			return
		}
	}
	h.resetAttempts.Hit(ip)
	h.resetAttempts.Hit("e:" + email)

	if email == "" {
		sent()
		return
	}
	u, err := h.db.GetUserByEmail(r.Context(), email)
	if err != nil {
		sent()
		return
	}
	token, err := newToken()
	if err != nil {
		sent()
		return
	}
	if err := h.db.CreateUserToken(r.Context(), token, u.ID, "reset", time.Now().Add(time.Hour)); err != nil {
		log.Printf("forgot-password: create reset token for %s: %v", email, err)
		sent()
		return
	}
	resetURL := h.baseURL(r) + "/reset-password?token=" + token
	if err := h.mail.Send(r.Context(), email, "Reset your password", mailer.ResetPasswordHTML(resetURL)); err != nil {
		log.Printf("forgot-password: send reset email to %s: %v", email, err)
	}
	h.audit(r, "user.password_reset_requested", u.ID.String(), "")
	sent()
}

func (h *Handler) ResetPasswordPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		h.tmpl.ExecuteTemplate(w, "reset_password.html", map[string]any{"Invalid": true, "Brand": h.cfg.CustomBrand(), "AssetVer": h.assetVer})
		return
	}
	h.tmpl.ExecuteTemplate(w, "reset_password.html", map[string]any{"Token": token, "Brand": h.cfg.CustomBrand(), "AssetVer": h.assetVer})
}

// ResetPasswordSubmit validates the token, sets the new password, and — since a
// leaked old session could otherwise outlive the credential change — logs the
// account out everywhere by clearing its sessions.
func (h *Handler) ResetPasswordSubmit(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	token := r.FormValue("token")
	password := r.FormValue("password")
	confirm := r.FormValue("confirm")

	invalid := func() {
		h.tmpl.ExecuteTemplate(w, "reset_password.html", map[string]any{"Invalid": true, "Brand": h.cfg.CustomBrand(), "AssetVer": h.assetVer})
	}
	retry := func(msg string) {
		h.tmpl.ExecuteTemplate(w, "reset_password.html", map[string]any{"Token": token, "Error": msg, "Brand": h.cfg.CustomBrand(), "AssetVer": h.assetVer})
	}

	if len(password) < 6 {
		retry("Password must be at least 6 characters.")
		return
	}
	if password != confirm {
		retry("Passwords don't match.")
		return
	}

	userID, err := h.db.ConsumeUserToken(r.Context(), token, "reset")
	if err != nil {
		invalid()
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if err := h.db.SetUserPassword(r.Context(), userID, string(hash)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	_ = h.db.DeleteSessionsForUser(r.Context(), userID)
	h.audit(r, "user.password_reset", userID.String(), "")
	http.Redirect(w, r, "/login?reset=1", http.StatusFound)
}
