package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A config written before the AI settings were removed still loads: the old AI keys are
// ignored, and the digest switch carries over from "ai_digest_enabled".
func TestLegacyAIConfigLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	old := `{"anthropic_api_key":"sk-x","anthropic_model":"m","ai_provider":"deepseek","ai_base_url":"https://x","ai_digest_enabled":true,"alert_webhook_url":"https://hooks.example/x"}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !c.DailyDigestEnabled() {
		t.Fatal("the digest switch must carry over from ai_digest_enabled")
	}
	if c.AlertWebhookURL() != "https://hooks.example/x" {
		t.Fatalf("other settings must survive: %q", c.AlertWebhookURL())
	}
	if err := c.SetDailyDigestEnabled(false); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c2.DailyDigestEnabled() {
		t.Fatal("switching it off must stick: the old key may not turn it back on")
	}
	b, _ := os.ReadFile(path)
	for _, k := range []string{"anthropic_api_key", "ai_provider", "ai_base_url", "ai_digest_enabled"} {
		if strings.Contains(string(b), k) {
			t.Fatalf("saved config still carries %s: %s", k, b)
		}
	}
}
