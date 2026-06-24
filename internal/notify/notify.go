// Package notify sends outbound alert notifications to a configured webhook.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

var client = &http.Client{Timeout: 10 * time.Second}

// SendWebhook POSTs a Slack/Discord/Mattermost-compatible {"text": ...} payload to
// the given webhook URL. Best-effort: a non-2xx response is reported as an error so
// the caller can log it, but delivery is never retried.
func SendWebhook(ctx context.Context, url, text string) error {
	return postJSON(ctx, url, map[string]string{"text": text})
}

// SendTeams POSTs a Microsoft Teams Adaptive Card to a Teams webhook URL (the
// "Post to a channel when a webhook request is received" Workflows trigger). Teams
// doesn't render Slack's {"text":...} shape, so it gets its own payload. severity
// drives the title colour. when, if non-empty, is shown as a "🕒 <when>" line for the
// time the problem happened. Best-effort, never retried.
func SendTeams(ctx context.Context, url, title, text, severity, when string) error {
	color := "accent" // info
	switch severity {
	case "critical":
		color = "attention"
	case "warning":
		color = "warning"
	}
	body := []any{
		map[string]any{"type": "TextBlock", "text": title, "weight": "Bolder", "size": "Medium", "color": color, "wrap": true},
		map[string]any{"type": "TextBlock", "text": text, "wrap": true, "isSubtle": true},
	}
	if when != "" {
		body = append(body, map[string]any{"type": "TextBlock", "text": "🕒 " + when, "wrap": true, "isSubtle": true, "spacing": "Small"})
	}
	card := map[string]any{
		"type": "message",
		"attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content": map[string]any{
				"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
				"type":    "AdaptiveCard",
				"version": "1.4",
				"body":    body,
			},
		}},
	}
	return postJSON(ctx, url, card)
}

// postJSON POSTs body as JSON to url. A non-2xx response is an error so the caller can
// log it; delivery is never retried.
func postJSON(ctx context.Context, url string, body any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}
	return nil
}
