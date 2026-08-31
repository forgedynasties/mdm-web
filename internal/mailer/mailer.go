// Package mailer sends transactional auth email (sign-up verification, password
// reset) via the Resend API.
package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"time"

	"mdm/internal/safehttp"
)

const apiURL = "https://api.resend.com/emails"

var client = safehttp.Client(10 * time.Second)

// Client sends email through Resend. From should include a display name, e.g.
// "AIO MDM <noreply@aioapp.com>".
type Client struct {
	apiKey string
	from   string
}

// New returns a Client. An empty apiKey is allowed — Send then logs the message
// instead of delivering it, so local/dev environments without a Resend key don't
// error out of the sign-up/reset flows.
func New(apiKey, from string) *Client {
	return &Client{apiKey: apiKey, from: from}
}

// Send delivers one HTML email. Best-effort: a non-2xx response is returned as an
// error so the caller can log it; delivery is never retried.
func (c *Client) Send(ctx context.Context, to, subject, htmlBody string) error {
	if c.apiKey == "" {
		log.Printf("mailer: RESEND_API_KEY not set, skipping send to %s: %s", to, subject)
		return nil
	}
	body := map[string]any{
		"from":    c.from,
		"to":      []string{to},
		"subject": subject,
		"html":    htmlBody,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("resend returned status %d", resp.StatusCode)
	}
	return nil
}

// VerifyEmailHTML builds the sign-up verification email body.
func VerifyEmailHTML(verifyURL string) string {
	return emailShell("Confirm your email", fmt.Sprintf(
		`<p>Click the button below to verify your email and finish creating your AIO MDM account.</p>
		<p><a href="%s" style="display:inline-block;padding:10px 20px;background:#4f63d6;color:#fff;text-decoration:none;border-radius:6px;font-weight:600;">Verify email</a></p>
		<p style="color:#666;font-size:13px;">This link expires in 24 hours. If you didn't sign up for AIO MDM, you can ignore this email.</p>`,
		html.EscapeString(verifyURL)))
}

// ResetPasswordHTML builds the password-reset email body.
func ResetPasswordHTML(resetURL string) string {
	return emailShell("Reset your password", fmt.Sprintf(
		`<p>Click the button below to choose a new password for your AIO MDM account.</p>
		<p><a href="%s" style="display:inline-block;padding:10px 20px;background:#4f63d6;color:#fff;text-decoration:none;border-radius:6px;font-weight:600;">Reset password</a></p>
		<p style="color:#666;font-size:13px;">This link expires in 1 hour. If you didn't request a password reset, you can ignore this email — your password won't change.</p>`,
		html.EscapeString(resetURL)))
}

func emailShell(title, bodyHTML string) string {
	return fmt.Sprintf(`<!doctype html><html><body style="font-family:-apple-system,Segoe UI,Roboto,sans-serif;color:#111;max-width:480px;margin:0 auto;padding:24px;">
<h2 style="margin:0 0 12px;">%s</h2>
%s
</body></html>`, html.EscapeString(title), bodyHTML)
}
