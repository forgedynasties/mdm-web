// Package mailer sends transactional auth email (sign-up verification, password
// reset) via Amazon SES's SMTP interface.
package mailer

import (
	"context"
	"fmt"
	"html"
	"log"
	"net/smtp"
	"strings"
)

// Client sends email through SES SMTP. From should include a display name, e.g.
// "AIO MDM <noreply@aioapp.com>".
type Client struct {
	host, port, username, password, from string
}

// New returns a Client. host/username/password empty is allowed — Send then logs
// the message instead of delivering it, so local/dev environments without SES SMTP
// credentials configured don't error out of the sign-up/reset flows. port defaults
// to "587" (STARTTLS) when empty.
func New(host, port, username, password, from string) *Client {
	if port == "" {
		port = "587"
	}
	return &Client{host: host, port: port, username: username, password: password, from: from}
}

// Send delivers one HTML email over SES SMTP (STARTTLS on 587, negotiated
// automatically by net/smtp.SendMail). Best-effort: a send failure is returned as
// an error so the caller can log it; delivery is never retried. ctx is accepted for
// interface parity with other outbound integrations (notify.SendWebhook etc.) —
// net/smtp has no context-aware send, so it isn't otherwise used here.
func (c *Client) Send(ctx context.Context, to, subject, htmlBody string) error {
	if c.host == "" || c.username == "" || c.password == "" {
		log.Printf("mailer: SES SMTP not configured, skipping send to %s: %s", to, subject)
		return nil
	}
	auth := smtp.PlainAuth("", c.username, c.password, c.host)
	addr := c.host + ":" + c.port
	msg := buildMessage(c.from, to, subject, htmlBody)
	if err := smtp.SendMail(addr, auth, c.from, []string{to}, msg); err != nil {
		return fmt.Errorf("ses smtp: %w", err)
	}
	return nil
}

// buildMessage renders a minimal single-part HTML email (headers + body), the
// smallest valid RFC 5322 message net/smtp.SendMail will accept.
func buildMessage(from, to, subject, htmlBody string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(htmlBody)
	return []byte(b.String())
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
