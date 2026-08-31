package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"mdm/internal/safehttp"
)

// msStateCookie holds the OAuth CSRF state between MicrosoftLoginStart and
// MicrosoftLoginCallback — short-lived, cleared on use.
const msStateCookie = "mdm-ms-state"

var msHTTPClient = safehttp.Client(10 * time.Second)

// MicrosoftLoginEnabled reports whether MS_CLIENT_ID/MS_CLIENT_SECRET/MS_TENANT_ID
// are all configured. The login page's button is separately gated by the
// msLoginEnabled template func (same underlying env vars) so it never links to a
// 404 when unconfigured.
func (h *Handler) MicrosoftLoginEnabled() bool {
	return h.msClientID != "" && h.msClientSecret != "" && h.msTenantID != ""
}

// MicrosoftLoginStart redirects to Microsoft's OAuth2 authorize endpoint, scoped
// to the single configured tenant (MS_TENANT_ID) — matches the existing
// @aioapp.com restriction on self-signup rather than accepting any Microsoft
// account. A random state, stashed in a short-lived cookie, is verified on
// callback to prevent CSRF.
func (h *Handler) MicrosoftLoginStart(w http.ResponseWriter, r *http.Request) {
	if !h.MicrosoftLoginEnabled() {
		http.Error(w, "Microsoft sign-in is not configured", http.StatusNotFound)
		return
	}
	state, err := newToken()
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     msStateCookie,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		Secure:   os.Getenv("COOKIE_SECURE") != "false",
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
	})
	q := url.Values{
		"client_id":     {h.msClientID},
		"response_type": {"code"},
		"redirect_uri":  {h.baseURL(r) + "/auth/microsoft/callback"},
		"response_mode": {"query"},
		"scope":         {"openid email profile User.Read"},
		"state":         {state},
	}
	authURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/authorize?%s", h.msTenantID, q.Encode())
	http.Redirect(w, r, authURL, http.StatusFound)
}

// MicrosoftLoginCallback exchanges the auth code for an access token, fetches the
// signed-in user's verified email from Microsoft Graph, and signs them in —
// auto-creating a viewer account on first login (still gated to signupEmailDomain;
// pre-verified since Microsoft already vouched for the identity, so no
// confirmation email is sent).
func (h *Handler) MicrosoftLoginCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string) {
		h.tmpl.ExecuteTemplate(w, "login.html", map[string]any{"Error": msg, "Brand": h.cfg.CustomBrand()})
	}
	if !h.MicrosoftLoginEnabled() {
		http.Error(w, "Microsoft sign-in is not configured", http.StatusNotFound)
		return
	}

	stateCookie, err := r.Cookie(msStateCookie)
	state := r.URL.Query().Get("state")
	http.SetCookie(w, &http.Cookie{Name: msStateCookie, Value: "", Path: "/", MaxAge: -1})
	if err != nil || state == "" || stateCookie.Value != state {
		fail("Microsoft sign-in failed (session expired). Please try again.")
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		fail("Microsoft sign-in was cancelled or failed.")
		return
	}

	tok, err := h.msExchangeCode(r.Context(), code, h.baseURL(r)+"/auth/microsoft/callback")
	if err != nil {
		log.Printf("microsoft login: token exchange error: %v", err)
		fail("Microsoft sign-in failed. Please try again.")
		return
	}
	profile, err := h.msFetchProfile(r.Context(), tok)
	if err != nil {
		log.Printf("microsoft login: fetch profile error: %v", err)
		fail("Microsoft sign-in failed. Please try again.")
		return
	}

	email := strings.ToLower(strings.TrimSpace(profile.Mail))
	if email == "" {
		email = strings.ToLower(strings.TrimSpace(profile.UserPrincipalName))
	}
	if email == "" || !strings.HasSuffix(email, signupEmailDomain) {
		fail("Microsoft sign-in is limited to " + signupEmailDomain + " accounts.")
		return
	}

	user, err := h.db.GetUserByEmail(r.Context(), email)
	if err != nil {
		// First Microsoft login for this email: create a viewer account. The
		// password is a random, never-stored value — this account only ever signs
		// in via Microsoft.
		randomPW, tErr := newToken()
		if tErr != nil {
			fail("Internal error, please try again.")
			return
		}
		hash, hErr := bcrypt.GenerateFromPassword([]byte(randomPW), bcrypt.DefaultCost)
		if hErr != nil {
			fail("Internal error, please try again.")
			return
		}
		created, cErr := h.db.CreateUser(r.Context(), email, string(hash), "viewer", &email, true)
		if cErr != nil {
			log.Printf("microsoft login: create user error: %v", cErr)
			fail("Internal error, please try again.")
			return
		}
		user = created
		h.audit(r, "user.microsoft_signup", user.ID.String(), email)
	} else if user.EmailVerifiedAt == nil {
		// e.g. a pending self-signup account — Microsoft already verified this
		// identity, so unblock it too instead of leaving it stuck.
		_ = h.db.SetUserEmailVerified(r.Context(), user.ID)
	}

	uid := user.ID
	if err := h.startSession(w, r, &uid, user.Username, user.Role); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "user.microsoft_login", user.ID.String(), email)
	http.Redirect(w, r, "/", http.StatusFound)
}

type msTokenResponse struct {
	AccessToken string `json:"access_token"`
}

// msExchangeCode swaps an authorization code for an access token at Microsoft's
// v2.0 token endpoint. redirectURI must byte-for-byte match what was sent to
// /authorize (the caller builds it the same way via h.baseURL).
func (h *Handler) msExchangeCode(ctx context.Context, code, redirectURI string) (string, error) {
	form := url.Values{
		"client_id":     {h.msClientID},
		"client_secret": {h.msClientSecret},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"scope":         {"openid email profile User.Read"},
	}

	tokenURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", h.msTenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := msHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("token endpoint returned status %d: %s", resp.StatusCode, string(body))
	}
	var tr msTokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", err
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("token endpoint returned no access_token")
	}
	return tr.AccessToken, nil
}

type msProfile struct {
	Mail              string `json:"mail"`
	UserPrincipalName string `json:"userPrincipalName"`
}

// msFetchProfile calls Microsoft Graph's /me endpoint with the access token to get
// the signed-in user's verified email — trust comes from this direct HTTPS call to
// Microsoft's own API, so there's no need to parse/verify an id_token JWT.
func (h *Handler) msFetchProfile(ctx context.Context, accessToken string) (*msProfile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://graph.microsoft.com/v1.0/me", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := msHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("graph /me returned status %d: %s", resp.StatusCode, string(body))
	}
	var p msProfile
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	return &p, nil
}
