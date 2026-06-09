// Package ai turns MDM telemetry into plain-language analysis via the Anthropic
// API. It is a non-ML "shortcut" for the analytics program: the rolled-up daily
// stats, health scores, and alerts are already computed in Go; this package just
// asks Claude to read them and say what's wrong and what to do about it.
//
// The API key and model come from config and are passed in per call, so changing
// them in Settings takes effect immediately without a restart.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"mdm/internal/db"
)

// Business context every prompt is grounded in. Kept identical across calls so the
// model always reasons about the same failure mode: a device unusable during service.
const systemContext = `You are a fleet-reliability analyst for AIO, which ships tableside ordering and ad tablets to restaurants. Devices take orders and show ads during service hours and charge overnight on wireless pads.

The one question that matters: will each device be available and usable during that restaurant's service hours? Battery, charging coverage, temperature, RAM, and firmware are leading indicators of that single failure mode. A dead tablet at 7pm Friday means lost orders and an unhappy restaurant.

When you analyze the telemetry below:
- Lead with a one-line verdict (healthy / watch / at-risk).
- Call out the top 1-3 concerns, each tied to specific numbers from the data.
- End with a concrete recommended action (e.g. "swap the charging pad", "schedule a battery replacement", "send a tech to <group>").
Be concise and specific. No preamble, no restating the question back. If the data looks fine, say so plainly.`

// Provider identifies the API wire format. "anthropic" uses the Claude SDK; any
// other value (e.g. "deepseek", "openai") uses the OpenAI-compatible
// /chat/completions format, which DeepSeek and most other vendors implement.
const (
	ProviderAnthropic = "anthropic"
	ProviderDeepSeek  = "deepseek"
)

// Client wraps a configured provider, key, model, and optional base URL.
type Client struct {
	provider string
	apiKey   string
	model    string
	baseURL  string
}

// httpClient is reused for OpenAI-compatible calls; the per-call context carries
// the real deadline.
var httpClient = &http.Client{Timeout: 120 * time.Second}

// New builds a client, filling provider-appropriate defaults for an empty model or
// base URL. An empty provider means Anthropic (back-compat with earlier configs).
func New(provider, apiKey, model, baseURL string) *Client {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		provider = ProviderAnthropic
	}
	c := &Client{
		provider: provider,
		apiKey:   apiKey,
		model:    strings.TrimSpace(model),
		baseURL:  strings.TrimRight(strings.TrimSpace(baseURL), "/"),
	}
	if c.model == "" {
		if provider == ProviderAnthropic {
			c.model = "claude-opus-4-8"
		} else {
			c.model = "deepseek-chat"
		}
	}
	if c.baseURL == "" {
		switch provider {
		case ProviderDeepSeek:
			c.baseURL = "https://api.deepseek.com"
		case ProviderAnthropic:
			// SDK has its own default; baseURL unused.
		default: // generic openai-compatible
			c.baseURL = "https://api.openai.com/v1"
		}
	}
	return c
}

// Model returns the resolved model id (after defaulting).
func (c *Client) Model() string { return c.model }

// complete dispatches to the configured provider's backend.
func (c *Client) complete(ctx context.Context, user string) (string, error) {
	if c.provider == ProviderAnthropic {
		return c.anthropicComplete(ctx, user)
	}
	return c.openaiComplete(ctx, user)
}

// anthropicComplete calls the Claude Messages API. Adaptive thinking is left on
// (recommended for Opus 4.x); only visible text blocks are returned.
func (c *Client) anthropicComplete(ctx context.Context, user string) (string, error) {
	client := anthropic.NewClient(option.WithAPIKey(c.apiKey))
	resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: 4096,
		System:    []anthropic.TextBlockParam{{Text: systemContext}},
		Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", fmt.Errorf("model returned no text")
	}
	return out, nil
}

// openaiComplete calls an OpenAI-compatible /chat/completions endpoint (DeepSeek,
// OpenAI, Groq, OpenRouter, local servers, …). The system context and user data
// map onto the system/user chat roles.
func (c *Client) openaiComplete(ctx context.Context, user string) (string, error) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	reqBody, _ := json.Marshal(struct {
		Model     string `json:"model"`
		Messages  []msg  `json:"messages"`
		MaxTokens int    `json:"max_tokens"`
		Stream    bool   `json:"stream"`
	}{
		Model:     c.model,
		Messages:  []msg{{Role: "system", Content: systemContext}, {Role: "user", Content: user}},
		MaxTokens: 4096,
		Stream:    false,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("%s returned status %d (unparseable body)", c.provider, resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		if parsed.Error != nil && parsed.Error.Message != "" {
			return "", fmt.Errorf("%s %d: %s", c.provider, resp.StatusCode, parsed.Error.Message)
		}
		return "", fmt.Errorf("%s returned status %d", c.provider, resp.StatusCode)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("%s returned no choices", c.provider)
	}
	out := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if out == "" {
		return "", fmt.Errorf("model returned no text")
	}
	return out, nil
}

// AnalyzeDevice summarizes one device's recent daily stats and open alerts.
func (c *Client) AnalyzeDevice(ctx context.Context, serial string, stats []db.DeviceDailyStat, alerts []db.Alert) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Device %s — last %d day(s) of rolled-up telemetry (one row per day, oldest first).\n", serial, len(stats))
	if len(stats) == 0 {
		b.WriteString("No daily stats recorded yet.\n")
	} else {
		b.WriteString("day | checkins | battery min/avg/max % | max temp °C | peak RAM % | charging coverage | online min | build\n")
		for _, s := range stats {
			fmt.Fprintf(&b, "%s | %d | %s/%s/%s | %s | %s | %s | %d | %s\n",
				s.Day.Format("2006-01-02"), s.CheckinCount,
				iptr(s.BatteryMin), fptr(s.BatteryAvg), iptr(s.BatteryMax),
				fptr(s.TempMax), iptr(s.RAMPctPeak), pctptr(s.ChargingFrac),
				s.OnlineMinutes, dash(s.BuildID))
		}
	}
	writeAlerts(&b, "Open alerts for this device", alerts)
	return c.complete(ctx, b.String())
}

// AnalyzeFleet ranks where to send a tech from the per-group health scorecard.
func (c *Client) AnalyzeFleet(ctx context.Context, groups []db.GroupHealth, total, online, openAlerts int) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Fleet snapshot: %d devices total, %d online, %d offline, %d open alerts.\n\n", total, online, total-online, openAlerts)
	if len(groups) == 0 {
		b.WriteString("No groups configured.\n")
	} else {
		b.WriteString("Per-group health (worst score first). Score is 0-100 (higher = healthier).\n")
		b.WriteString("group | score | devices | offline | crit/warn alerts | avg peak battery % | Δ vs prior wk | charging coverage | max temp °C | distinct builds\n")
		for _, g := range groups {
			fmt.Fprintf(&b, "%s | %d | %d | %d | %d/%d | %s | %s | %s | %s | %d\n",
				g.Name, g.Score, g.DeviceCount, g.OfflineCount,
				g.OpenCritical, g.OpenWarning,
				f64ptr(g.BatteryAvg), f64delta(g.BatteryDelta),
				pct64ptr(g.ChargingAvg), f64ptr(g.TempMax), g.DistinctBuilds)
		}
	}
	return c.complete(ctx, b.String())
}

// writeAlerts appends a labeled alert list (or "none") to b.
func writeAlerts(b *strings.Builder, label string, alerts []db.Alert) {
	fmt.Fprintf(b, "\n%s: ", label)
	if len(alerts) == 0 {
		b.WriteString("none.\n")
		return
	}
	b.WriteByte('\n')
	for _, a := range alerts {
		fmt.Fprintf(b, "- [%s] %s (since %s): %s\n",
			strings.ToUpper(a.Severity), a.Type, a.FiredAt.Format("2006-01-02 15:04"), a.Summary)
	}
}

// --- pointer/format helpers: render null pointers as "n/a" ---

func iptr(p *int) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%d", *p)
}

func fptr(p *float32) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.0f", *p)
}

func f64ptr(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.0f", *p)
}

func f64delta(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%+.0f", *p)
}

func pctptr(p *float32) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", *p*100)
}

func pct64ptr(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", *p*100)
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
