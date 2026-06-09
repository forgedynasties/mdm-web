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

// personaContext grounds every prompt in the same framing and voice. The four
// signals it names are the focus; offline/connectivity is deliberately out of scope.
const personaContext = `You're the ops lead keeping an eye on AIO's restaurant tablets — the ones that take tableside orders and run ads during service, and charge overnight on wireless pads. What matters: is each tablet healthy enough to last a busy service? The signals that predict trouble are battery health (capacity slipping week over week), overnight charging (did it actually charge), overheating on the pads, and memory pressure (RAM near the ceiling → crashes). A struggling tablet at 7pm on a Friday is lost orders and an annoyed restaurant.

Talk like a person giving a quick heads-up to a colleague who's slammed — plain, direct, a little opinionated, no corporate filler. Ground everything in the actual numbers and name the specific group or device. Don't invent problems to sound busy, and don't flag devices for being briefly offline — connectivity isn't the priority right now.`

// deviceSystem is the system prompt for a single-device prose analysis.
const deviceSystem = personaContext + `

Give a short read on this one device: a one-line bottom line, the top 1-3 concerns tied to its numbers, and a concrete next action. A few sentences, no preamble, no restating the question.`

// Thresholds are the configurable cutoffs (from the alert rules) the fleet report
// uses to decide what counts as a problem.
type Thresholds struct {
	TempC         float64 // overheating limit °C
	MinFullPct    float64 // overnight battery target %
	MaxChargeFrac float64 // overnight charging-coverage floor (0–1)
	DropPct       float64 // battery weekly-decline points
	WindowDays    float64 // decline comparison window
	RAMPct        float64 // memory-pressure RAM %
}

// fleetSystem builds the structured-JSON system prompt for the fleet report.
func fleetSystem(t Thresholds) string {
	return personaContext + fmt.Sprintf(`

Focus ONLY on these four signals, judged against the configured cutoffs:
- Battery health: flag a group/device whose weekly peak-battery drop is about %.0f points or more.
- Overnight charging: flag devices that didn't reach ~%.0f%% overnight or charged less than %.0f%% of the night.
- Overheating: flag devices whose battery hit ~%.0f°C or hotter.
- Memory pressure: flag devices whose peak RAM hit ~%.0f%% or more.
Do NOT raise offline/connectivity as an issue.

Respond with ONLY a JSON object — no markdown, no code fences, no prose around it — in exactly this shape:
{
  "status": "ok" | "watch" | "at_risk",
  "headline": "one short, human sentence — the bottom line",
  "metrics": [{"label": "Charging", "value": "94%%"}, {"label": "Hottest", "value": "41°C"}],
  "issues": [
    {"severity": "warn" | "critical", "area": "battery" | "charging" | "heat" | "memory",
     "scope": "group or device name", "detail": "what's wrong, with numbers", "action": "what to do"}
  ],
  "good": ["short labels of signals that look fine"]
}
status: ok = nothing to do, watch = keep an eye on it, at_risk = act now. Sort issues worst-first; use an empty array when there are none. When everything's healthy give an upbeat one-line headline plus 2-3 grounding metrics. Keep "detail" and "action" human and specific.`,
		t.DropPct, t.MinFullPct, t.MaxChargeFrac*100, t.TempC, t.RAMPct)
}

// ReportMetric is one at-a-glance number on the report card.
type ReportMetric struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// ReportIssue is one flagged problem.
type ReportIssue struct {
	Severity string `json:"severity"`
	Area     string `json:"area"`
	Scope    string `json:"scope"`
	Detail   string `json:"detail"`
	Action   string `json:"action"`
}

// Report is the structured fleet report the model returns (rendered as cards in the
// dashboard; flattened to text for the webhook digest).
type Report struct {
	Status   string         `json:"status"`
	Headline string         `json:"headline"`
	Metrics  []ReportMetric `json:"metrics"`
	Issues   []ReportIssue  `json:"issues"`
	Good     []string       `json:"good"`
}

// ParseReport extracts a Report from the model's response, tolerating ```json fences.
// ok is false if the text isn't a usable report (callers fall back to plain text).
func ParseReport(s string) (Report, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return Report{}, false
	}
	var r Report
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return Report{}, false
	}
	if r.Headline == "" && r.Status == "" {
		return Report{}, false
	}
	return r, true
}

// Text flattens a Report to a readable plain-text summary (for the webhook digest).
func (r Report) Text() string {
	var b strings.Builder
	b.WriteString(r.Headline)
	for _, is := range r.Issues {
		fmt.Fprintf(&b, "\n• [%s] %s — %s", strings.ToUpper(is.Severity), is.Scope, is.Detail)
		if is.Action != "" {
			fmt.Fprintf(&b, " → %s", is.Action)
		}
	}
	return strings.TrimSpace(b.String())
}

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

// Usage reports the token counts for one call (0 if the provider omitted them).
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// complete dispatches to the configured provider's backend.
func (c *Client) complete(ctx context.Context, system, user string) (string, Usage, error) {
	if c.provider == ProviderAnthropic {
		return c.anthropicComplete(ctx, system, user)
	}
	return c.openaiComplete(ctx, system, user)
}

// anthropicComplete calls the Claude Messages API. With a custom base URL it
// targets an Anthropic-compatible gateway instead (e.g. DeepSeek's /anthropic
// endpoint), which expects bearer-token auth and may not support Claude-native
// adaptive thinking — so that's only enabled against the real Anthropic API.
// Only visible text blocks are returned.
func (c *Client) anthropicComplete(ctx context.Context, system, user string) (string, Usage, error) {
	var opts []option.RequestOption
	if c.baseURL != "" {
		opts = append(opts, option.WithBaseURL(c.baseURL), option.WithAuthToken(c.apiKey))
	} else {
		opts = append(opts, option.WithAPIKey(c.apiKey))
	}
	client := anthropic.NewClient(opts...)

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: 4096,
		System:    []anthropic.TextBlockParam{{Text: system}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
	}
	if c.baseURL == "" {
		params.Thinking = anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}}
	}
	resp, err := client.Messages.New(ctx, params)
	if err != nil {
		return "", Usage{}, err
	}
	var b strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	usage := Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", usage, fmt.Errorf("model returned no text")
	}
	return out, usage, nil
}

// openaiComplete calls an OpenAI-compatible /chat/completions endpoint (DeepSeek,
// OpenAI, Groq, OpenRouter, local servers, …). The system context and user data
// map onto the system/user chat roles.
func (c *Client) openaiComplete(ctx context.Context, system, user string) (string, Usage, error) {
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
		Messages:  []msg{{Role: "system", Content: system}, {Role: "user", Content: user}},
		MaxTokens: 4096,
		Stream:    false,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", Usage{}, err
	}
	defer resp.Body.Close()

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", Usage{}, fmt.Errorf("%s returned status %d (unparseable body)", c.provider, resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		if parsed.Error != nil && parsed.Error.Message != "" {
			return "", Usage{}, fmt.Errorf("%s %d: %s", c.provider, resp.StatusCode, parsed.Error.Message)
		}
		return "", Usage{}, fmt.Errorf("%s returned status %d", c.provider, resp.StatusCode)
	}
	if len(parsed.Choices) == 0 {
		return "", Usage{}, fmt.Errorf("%s returned no choices", c.provider)
	}
	usage := Usage{InputTokens: parsed.Usage.PromptTokens, OutputTokens: parsed.Usage.CompletionTokens}
	out := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if out == "" {
		return "", usage, fmt.Errorf("model returned no text")
	}
	return out, usage, nil
}

// AnalyzeDevice summarizes one device's recent daily stats and open alerts.
func (c *Client) AnalyzeDevice(ctx context.Context, serial string, stats []db.DeviceDailyStat, alerts []db.Alert) (string, Usage, error) {
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
	return c.complete(ctx, deviceSystem, b.String())
}

// AnalyzeFleet returns a structured JSON report (see Report) from the per-group
// health scorecard plus the current open alerts, judged against the configured
// thresholds. Focus is battery/charging/heat/memory — not offline.
func (c *Client) AnalyzeFleet(ctx context.Context, groups []db.GroupHealth, total, online, openAlerts int, alerts []db.Alert, t Thresholds) (string, Usage, error) {
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
	// Cap the alert list so a noisy fleet doesn't blow the prompt size.
	if len(alerts) > 40 {
		alerts = alerts[:40]
	}
	writeAlerts(&b, "Currently open alerts (device serial · type · since · detail)", alerts)
	return c.complete(ctx, fleetSystem(t), b.String())
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
