// Package assistant talks to DeepSeek for the dashboard assistant (25 Nov 2026; user: "we
// will be using deepseek"). DeepSeek speaks the OpenAI chat-completions format, so this is
// plain HTTP: one streamed request per model turn, text and tool calls read off the stream.
// The tool loop, the tools and the records live in the dashboard (assistant.go there); this
// package only knows the wire format. Configuration is the environment:
//
//	DEEPSEEK_API_KEY   required; no key, no assistant
//	DEEPSEEK_BASE_URL  default https://api.deepseek.com
//	ASSISTANT_MODEL    default deepseek-flash
package assistant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client is one configured DeepSeek endpoint.
type Client struct {
	BaseURL string
	Key     string
	Model   string
	HTTP    *http.Client
}

// FromEnv builds the client from the environment, or nil when no key is set.
func FromEnv() *Client {
	key := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if key == "" {
		return nil
	}
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("DEEPSEEK_BASE_URL")), "/")
	if base == "" {
		base = "https://api.deepseek.com"
	}
	model := strings.TrimSpace(os.Getenv("ASSISTANT_MODEL"))
	if model == "" {
		model = "deepseek-flash"
	}
	// No overall timeout: a turn streams for as long as the model writes; the request's
	// context (the browser leaving, a deploy draining) ends it.
	return &Client{BaseURL: base, Key: key, Model: model, HTTP: &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 60 * time.Second, IdleConnTimeout: 90 * time.Second,
	}}}
}

// Message is one chat message in the OpenAI format.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Reasoning  string     `json:"reasoning_content,omitempty"` // passed back within a turn that used tools
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a function call the model asked for.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool is a function the model may call: a name, what it is for, a JSON schema.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// Usage is the token count of one request.
type Usage struct {
	Prompt     int
	Completion int
	Cached     int
}

// Result is one finished model turn.
type Result struct {
	Message Message // the assistant message: text, and tool calls when Finish is "tool_calls"
	Finish  string
	Usage   Usage
}

// Stream sends one request and reads the streamed answer, handing each piece of answer
// text to onText as it arrives (reasoning is not shown). MaxTokens caps the turn.
func (c *Client) Stream(ctx context.Context, msgs []Message, tools []Tool, maxTokens int, onText func(string)) (Result, error) {
	body := map[string]any{
		"model": c.Model, "messages": msgs, "stream": true, "max_tokens": maxTokens, "temperature": 0.2,
		"stream_options": map[string]any{"include_usage": true},
	}
	if len(tools) > 0 {
		ts := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			ts = append(ts, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": t.Parameters}})
		}
		body["tools"] = ts
	}
	b, err := json.Marshal(body)
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return Result{}, &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
	}
	return readStream(resp.Body, onText)
}

// APIError is a non-200 answer from DeepSeek.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("deepseek: HTTP %d: %s", e.Status, e.Body) }

type chunk struct {
	Choices []struct {
		Delta struct {
			Content   *string `json:"content"`
			Reasoning *string `json:"reasoning_content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// readStream assembles a streamed turn: text pieces, reasoning, and tool calls whose
// id/name/arguments arrive in fragments keyed by index.
func readStream(r io.Reader, onText func(string)) (Result, error) {
	var res Result
	res.Message.Role = "assistant"
	var text, reasoning strings.Builder
	calls := map[int]*ToolCall{}
	order := []int{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var ch chunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			continue // a keep-alive or a field we don't know; the next chunk carries on
		}
		if ch.Usage != nil {
			res.Usage.Prompt, res.Usage.Completion = ch.Usage.PromptTokens, ch.Usage.CompletionTokens
			if ch.Usage.PromptTokensDetails != nil {
				res.Usage.Cached = ch.Usage.PromptTokensDetails.CachedTokens
			}
		}
		for _, c := range ch.Choices {
			if c.Delta.Content != nil && *c.Delta.Content != "" {
				text.WriteString(*c.Delta.Content)
				if onText != nil {
					onText(*c.Delta.Content)
				}
			}
			if c.Delta.Reasoning != nil {
				reasoning.WriteString(*c.Delta.Reasoning)
			}
			for _, tc := range c.Delta.ToolCalls {
				cur, ok := calls[tc.Index]
				if !ok {
					cur = &ToolCall{Type: "function"}
					calls[tc.Index] = cur
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				if tc.Function.Name != "" {
					cur.Function.Name += tc.Function.Name
				}
				cur.Function.Arguments += tc.Function.Arguments
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				res.Finish = *c.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		return res, err
	}
	res.Message.Content = text.String()
	res.Message.Reasoning = reasoning.String()
	for _, i := range order {
		res.Message.ToolCalls = append(res.Message.ToolCalls, *calls[i])
	}
	if res.Finish == "" {
		return res, errors.New("deepseek: the answer stopped before it finished")
	}
	return res, nil
}
