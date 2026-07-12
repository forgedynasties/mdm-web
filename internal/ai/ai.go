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
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"mdm/internal/db"
	"mdm/internal/safehttp"
)

// personaContext grounds every prompt in the same framing and voice. The four
// signals it names are the focus; offline/connectivity is deliberately out of scope.
// Deployment state is central: most units are still on the bench in the lab, and the
// model must not judge an idle lab unit as a failing restaurant unit.
const personaContext = `You're keeping an eye on AIO's TSAI units — tableside devices in restaurants that take orders and run ads, each with a built-in wireless charging pad customers use for their own phones. The fleet is small and most units are still on the bench in our lab, not yet in a restaurant. Your job is early warning: read the telemetry and say, plainly, which deployed units look healthy and which are trending toward trouble.

The signals that matter are whether a unit charged properly, running hot, and memory pressure (RAM near the ceiling → crashes). Connectivity is not a concern — never flag a unit for being offline or briefly unreachable.

Deployment state changes how much a unit matters. LAB units are on the bench undergoing testing — idle, unplugged, or powered-down stretches are expected and normal, not a problem. Don't raise alarms about lab units; at most note a genuine hardware fault worth a second look. Reserve real concern for DEPLOYED units live in a restaurant. Lab units still deserve a read, though: when there's nothing deployed, summarize how the units under test are doing rather than waving the whole fleet off.

Voice: calm and measured. State the facts and a sensible next step without drama. Right now there's usually nothing to physically do but watch and log — so a good report is an honest status read, not a call to action. Ground every statement in the actual numbers and name the specific unit or restaurant. Don't invent problems to sound busy, and don't manufacture urgency the data doesn't support.`

// deviceSystem is the system prompt for a single-unit prose analysis.
const deviceSystem = personaContext + `

Give a short, measured read on this one TSAI: a one-line bottom line, the top 1-3 things worth noting tied to its numbers, and — only if there's something to do — a sensible next step. A few sentences, no preamble, no restating the question. If this is a lab unit, keep it low-stakes: note hardware health if something looks genuinely wrong, but don't treat idle, unplugged, or powered-down behavior as a problem.`

// Thresholds are the configurable cutoffs (from the alert rules) the fleet report
// uses to decide what counts as a problem.
type Thresholds struct {
	TempC         float64 // overheating limit °C
	MinFullPct    float64 // overnight battery target %
	MaxChargeFrac float64 // overnight charging-coverage floor (0–1)
	RAMPct        float64 // memory-pressure RAM %
}

// fleetSystem builds the structured-JSON system prompt for the fleet report.
func fleetSystem(t Thresholds) string {
	return personaContext + fmt.Sprintf(`

This is a DAILY report: every number you're given covers TODAY only (the current day's check-ins). Read it as today's snapshot, not a multi-day trend — don't cite values from earlier days or call something a weeks-long pattern.

Focus ONLY on these three signals, judged against the configured cutoffs:
- Charging: didn't reach ~%.0f%% or charged less than %.0f%% of the day.
- Overheating: running at ~%.0f°C or hotter. When you flag heat, name the specific unit from the row's "hottest unit" column (it's the device that drove that restaurant's max temp) rather than just citing the peak number.
- Memory pressure: peak RAM hit ~%.0f%% or more.
Do NOT raise offline/connectivity as an issue.

Deployment rules — read these carefully:
- The snapshot tells you how many units are DEPLOYED (assigned to a restaurant) vs in the LAB (no restaurant). A device is deployed iff it's assigned to a restaurant.
- Only DEPLOYED units may drive "watch" or "at_risk" status or appear as issues. A lab unit hitting a cutoff is expected bench behavior — do not list it as an issue and do not let it raise the status.
- If nothing is deployed yet, status stays "ok" — there's no restaurant-risk to invent — but still report on the lab. The units are undergoing testing, so give a real read on them: how many are under test, how their hardware (battery, charging, heat, memory) is holding up across the testing, and call out any genuine hardware fault worth a second look as an informational note (not as a "watch"/"at_risk" issue). Don't reduce it to "nothing to act on."

Respond with ONLY a JSON object — no markdown, no code fences, no prose around it — in exactly this shape:
{
  "status": "ok" | "watch" | "at_risk",
  "headline": "one short, plain sentence — the bottom line",
  "metrics": [{"label": "Charging", "value": "94%%"}, {"label": "Hottest", "value": "41°C"}],
  "issues": [
    {"severity": "warn" | "critical", "area": "charging" | "heat" | "memory",
     "scope": "group or device name", "detail": "what's wrong, with numbers", "action": "what to do"}
  ],
  "good": ["short labels of signals that look fine"]
}
status: ok = nothing to act on, watch = a deployed unit worth keeping an eye on, at_risk = a deployed unit needs attention. Sort issues worst-first; use an empty array when there are none. When everything's fine give a calm one-line headline plus 2-3 grounding metrics; when all units are still in the lab, make the headline about how the units under test are doing and ground it in their hardware numbers. Keep "detail" and "action" specific and free of drama.
When you name a unit, write its full device serial exactly as it appears in the data — and when several units are involved, list each full serial separated by commas. Never abbreviate or merge serials (no "ABC1230030/0021/0046" shorthand); the dashboard turns each full serial into a link, so a shortened serial just becomes dead text.`,
		t.MinFullPct, t.MaxChargeFrac*100, t.TempC, t.RAMPct)
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
// the real deadline. It is SSRF-hardened because ai_base_url is operator-configurable
// and every request carries the provider API key as a bearer token — a base URL
// pointed at an internal/attacker host would both SSRF and exfiltrate that key.
var httpClient = safehttp.Client(120 * time.Second)

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
		// Custom Anthropic-compatible gateway: validate the operator-supplied URL and
		// route it through the SSRF-hardened client (it carries the API key).
		if err := safehttp.CheckURL(c.baseURL, true); err != nil {
			return "", Usage{}, err
		}
		opts = append(opts, option.WithBaseURL(c.baseURL), option.WithAuthToken(c.apiKey), option.WithHTTPClient(httpClient))
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
	// baseURL is operator-configurable (ai_base_url) and the request below sends the
	// API key as a bearer token, so validate the scheme before dialing.
	if err := safehttp.CheckURL(c.baseURL, true); err != nil {
		return "", Usage{}, err
	}
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

	// Bound the response from a config-controlled host (2 MiB is ample for a chat
	// completion) so an oversized/hostile reply can't exhaust memory.
	body := io.LimitReader(resp.Body, 2<<20)

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
	if err := json.NewDecoder(body).Decode(&parsed); err != nil {
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
func (c *Client) AnalyzeDevice(ctx context.Context, serial string, deployed bool, stats []db.DeviceDailyStat, alerts []db.Alert) (string, Usage, error) {
	var b strings.Builder
	state := "IN THE LAB (on the bench, not yet deployed to a restaurant — idle/unplugged behavior is expected here)"
	if deployed {
		state = "DEPLOYED (live in a restaurant)"
	}
	fmt.Fprintf(&b, "Device %s is %s.\n", serial, state)
	fmt.Fprintf(&b, "Last %d day(s) of rolled-up telemetry (one row per day, oldest first).\n", len(stats))
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
func (c *Client) AnalyzeFleet(ctx context.Context, groups []db.GroupHealth, total, online, openAlerts, deployed, lab int, alerts []db.Alert, t Thresholds) (string, Usage, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Fleet snapshot: %d devices total — %d DEPLOYED (live in a restaurant), %d in the LAB (on the bench). %d online, %d offline, %d open alerts.\n", total, deployed, lab, online, total-online, openAlerts)
	if deployed == 0 {
		b.WriteString("Nothing is deployed yet — the whole fleet is still in the lab undergoing testing. Report on how the units under test are holding up.\n")
	}
	b.WriteByte('\n')
	if len(groups) == 0 {
		b.WriteString("No restaurants configured.\n")
	} else {
		b.WriteString("Per-restaurant health (worst score first). Score is 0-100 (higher = healthier). All metrics below are for TODAY only (the day's check-ins so far); the Δ column compares today against yesterday. The 'where' column says whether the restaurant is live (deployed) or still a lab venue, and how many of its devices resolve to deployed.\n")
		b.WriteString("restaurant | where | score | devices | offline | crit/warn alerts | avg peak battery % | Δ vs yesterday | charging coverage | max temp °C | hottest unit | distinct builds\n")
		for _, g := range groups {
			where := "lab"
			if g.Deployed || g.DeployedCount > 0 {
				where = fmt.Sprintf("DEPLOYED (%d/%d live)", g.DeployedCount, g.DeviceCount)
			}
			fmt.Fprintf(&b, "%s | %s | %d | %d | %d | %d/%d | %s | %s | %s | %s | %s | %d\n",
				g.Name, where, g.Score, g.DeviceCount, g.OfflineCount,
				g.OpenCritical, g.OpenWarning,
				f64ptr(g.BatteryAvg), f64delta(g.BatteryDelta),
				pct64ptr(g.ChargingAvg), f64ptr(g.TempMax), dash(strptr(g.TempMaxSerial)), g.DistinctBuilds)
		}
	}
	// Cap the alert list so a noisy fleet doesn't blow the prompt size.
	if len(alerts) > 40 {
		alerts = alerts[:40]
	}
	writeAlerts(&b, "Currently open alerts (device serial · type · since · detail)", alerts)
	return c.complete(ctx, fleetSystem(t), b.String())
}

// logcatSystem is the system prompt for turning a plain-language problem report
// into logcat capture settings. It grounds the model in the actual app and the
// levels the capture form accepts.
const logcatSystem = `You configure Android logcat captures for AIO's MDM fleet. The device runs a privileged system app, package com.aioapp.mdm (UID android.uid.system), whose main components are: MdmService (the foreground check-in loop), KioskManager (lock-task / kiosk mode), MdmAdminReceiver (DevicePolicyManager admin), and the wireless-charging + battery telemetry. The units are tableside ordering tablets with a built-in wireless charging pad.

A field tech describes a problem in plain words. Turn that into the tightest logcat capture that would actually contain the evidence.

Choose:
- level: one of E, W, I, D, V. E = errors only, W = warnings and up (default, good for crashes/ANRs), I = info and up, D = debug and up (verbose, for reproducing a specific flow), V = everything (only when you truly need it).
- lines: how many recent lines to pull, 50–5000. Use 500 for a normal issue, more (1500–3000) for intermittent or hard-to-reproduce problems, fewer for a single obvious crash.
- tag: an OPTIONAL space-separated list of logcat tags to filter to. Prefer a real tag when the problem clearly maps to one — e.g. MdmService, KioskManager, MdmAdminReceiver, ActivityManager, WindowManager, PackageManager, BatteryService, ConnectivityService, WifiService, DropBoxManager, AndroidRuntime (Java crashes). Leave it EMPTY ("") when the cause is unknown or could come from anywhere — an empty tag captures everything at the chosen level, which is safer than guessing wrong.

Respond with ONLY a JSON object — no markdown, no code fences, no prose around it — in exactly this shape:
{"level":"W","lines":500,"tag":"MdmService","rationale":"one short sentence on why these settings fit the problem"}
Keep the rationale to one plain sentence. If the tech's description is vague, widen the level and clear the tag rather than guessing a specific component.`

// LogcatSuggestion is the structured logcat configuration the model returns for a
// plain-language problem report.
type LogcatSuggestion struct {
	Level     string `json:"level"`
	Lines     int    `json:"lines"`
	Tag       string `json:"tag"`
	Rationale string `json:"rationale"`
}

// SuggestLogcat asks the model for logcat capture settings that fit a plain-language
// problem description. The returned text is the raw JSON; parse it with
// ParseLogcatSuggestion.
func (c *Client) SuggestLogcat(ctx context.Context, problem string) (string, Usage, error) {
	user := "The tech wants logs because:\n" + strings.TrimSpace(problem)
	return c.complete(ctx, logcatSystem, user)
}

const logAnalyzeSystem = `You are an Android fleet-support engineer triaging device logs (logcat and DropBox crash/ANR/tombstone dumps). Given a raw log buffer, reply in plain text with exactly these three short lines, no preamble:
What happened: <the key error or crash, one line>
Likely cause: <the most probable root cause, one line>
Next step: <one concrete action for the operator, one line>
Be terse and specific — name the package/component and exception if present. If the log shows no clear problem, say "No obvious error in this buffer."`

// AnalyzeLog gives a short operator-facing triage of a raw logcat/crash buffer.
// The buffer is tail-capped so the prompt stays bounded on huge captures.
func (c *Client) AnalyzeLog(ctx context.Context, content string) (string, Usage, error) {
	content = strings.TrimSpace(content)
	if len(content) > 12000 {
		content = "…(truncated)…\n" + content[len(content)-12000:]
	}
	return c.complete(ctx, logAnalyzeSystem, "Log buffer:\n\n"+content)
}

// ParseLogcatSuggestion extracts a LogcatSuggestion from the model's response,
// tolerating ```json fences, and clamps the fields to the capture form's limits.
// ok is false if the text isn't usable JSON.
func ParseLogcatSuggestion(s string) (LogcatSuggestion, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return LogcatSuggestion{}, false
	}
	var g LogcatSuggestion
	if err := json.Unmarshal([]byte(s), &g); err != nil {
		return LogcatSuggestion{}, false
	}
	g.Level = strings.ToUpper(strings.TrimSpace(g.Level))
	switch g.Level {
	case "E", "W", "I", "D", "V":
	default:
		g.Level = "W"
	}
	if g.Lines < 50 {
		g.Lines = 500
	}
	if g.Lines > 5000 {
		g.Lines = 5000
	}
	g.Tag = strings.TrimSpace(g.Tag)
	return g, true
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
		serial := a.Serial
		if serial == "" {
			serial = "unknown-device"
		}
		// Serial first so the model can name the specific unit behind each incident.
		fmt.Fprintf(b, "- [%s] %s · %s (since %s): %s\n",
			strings.ToUpper(a.Severity), serial, a.Type, a.FiredAt.Format("2006-01-02 15:04"), a.Summary)
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

// strptr dereferences a *string to its value, or "" if nil.
func strptr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
