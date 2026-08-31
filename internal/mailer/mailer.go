// Package mailer sends transactional auth email (sign-up verification, password
// reset) via the Amazon SES v2 API, SigV4-signed with the caller's AWS
// credentials — the same default credential chain internal/apkstore uses for S3
// (env vars, shared config/credentials file, or an attached IAM role). No
// SES-specific SMTP username/password needed, just AWS creds + a verified
// sending identity.
package mailer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4signer "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"mdm/internal/safehttp"
)

var httpClient = safehttp.Client(10 * time.Second)

// Client sends email through the SES v2 SendEmail API. From should include a
// display name, e.g. "AIO MDM <mdm@dev.aioapp.com>", and must be a verified SES
// identity (or in a verified domain) or SES will reject the send.
type Client struct {
	cfg    aws.Config
	region string
	from   string
}

// New resolves AWS credentials via the default chain (env vars, shared config/
// credentials file, or an attached IAM role) for the given region. region == ""
// disables sending — Send then logs the message instead of delivering it, so
// local/dev environments without AWS_REGION set don't error out of the sign-up/
// reset flows.
func New(ctx context.Context, region, from string) (*Client, error) {
	if region == "" {
		return &Client{from: from}, nil
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	return &Client{cfg: cfg, region: region, from: from}, nil
}

// Send delivers one HTML email via SES's SendEmail v2 API, SigV4-signed with the
// resolved AWS credentials. Best-effort: a send failure is returned as an error so
// the caller can log it; delivery is never retried.
func (c *Client) Send(ctx context.Context, to, subject, htmlBody string) error {
	if c.region == "" {
		log.Printf("mailer: AWS_REGION not set, skipping send to %s: %s", to, subject)
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"FromEmailAddress": c.from,
		"Destination":      map[string]any{"ToAddresses": []string{to}},
		"Content": map[string]any{
			"Simple": map[string]any{
				"Subject": map[string]any{"Data": subject, "Charset": "UTF-8"},
				"Body":    map[string]any{"Html": map[string]any{"Data": htmlBody, "Charset": "UTF-8"}},
			},
		},
	})
	if err != nil {
		return err
	}

	endpoint := fmt.Sprintf("https://email.%s.amazonaws.com/v2/email/outbound-emails", c.region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	creds, err := c.cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("resolve aws credentials: %w", err)
	}
	hash := sha256.Sum256(body)
	signer := v4signer.NewSigner()
	if err := signer.SignHTTP(ctx, creds, req, hex.EncodeToString(hash[:]), "ses", c.region, time.Now()); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ses returned status %d", resp.StatusCode)
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
