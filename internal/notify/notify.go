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
	body, _ := json.Marshal(map[string]string{"text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
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
