package assistant

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The chunk shapes DeepSeek sent on 25 Nov 2026: reasoning first, then a tool call whose
// arguments arrive in pieces, then the finish chunk carrying usage.
const recorded = `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":""},"finish_reason":null}],"usage":null}

data: {"choices":[{"index":0,"delta":{"content":null,"reasoning_content":"The user asks"},"finish_reason":null}],"usage":null}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}],"usage":null}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\": \"L"}}]},"finish_reason":null}],"usage":null}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ahore\"}"}}]},"finish_reason":null}],"usage":null}

data: {"choices":[{"index":0,"delta":{"content":"","reasoning_content":null},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":307,"completion_tokens":41,"prompt_tokens_details":{"cached_tokens":128}}}

data: [DONE]
`

func TestReadStreamToolCall(t *testing.T) {
	var shown strings.Builder
	res, err := readStream(strings.NewReader(recorded), func(s string) { shown.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Finish != "tool_calls" || len(res.Message.ToolCalls) != 1 {
		t.Fatalf("finish %q, calls %+v", res.Finish, res.Message.ToolCalls)
	}
	tc := res.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city": "Lahore"}` {
		t.Fatalf("call = %+v", tc)
	}
	if res.Message.Reasoning != "The user asks" || shown.Len() != 0 {
		t.Fatalf("reasoning %q, shown %q", res.Message.Reasoning, shown.String())
	}
	if res.Usage.Prompt != 307 || res.Usage.Completion != 41 || res.Usage.Cached != 128 {
		t.Fatalf("usage = %+v", res.Usage)
	}
}

func TestReadStreamCutShort(t *testing.T) {
	if _, err := readStream(strings.NewReader(`data: {"choices":[{"index":0,"delta":{"content":"Hel"}}]}`+"\n"), nil); err == nil {
		t.Fatal("a stream with no finish should be an error")
	}
}

// Live: one question that needs the tool, its result, then the answer. Run with
// DEEPSEEK_API_KEY set; costs a fraction of a cent and sends nothing from the MDM.
func TestLiveToolRoundTrip(t *testing.T) {
	c := FromEnv()
	if c == nil {
		t.Skip("DEEPSEEK_API_KEY not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tools := []Tool{{Name: "get_weather", Description: "Current weather for a city", Parameters: map[string]any{
		"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []string{"city"}}}}
	msgs := []Message{{Role: "system", Content: "Answer in one short sentence. Use the tool for weather."}, {Role: "user", Content: "What is the weather in Lahore?"}}
	r1, err := c.Stream(ctx, msgs, tools, 800, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Finish != "tool_calls" || len(r1.Message.ToolCalls) == 0 {
		t.Fatalf("first turn: finish %q, %d calls, text %q", r1.Finish, len(r1.Message.ToolCalls), r1.Message.Content)
	}
	msgs = append(msgs, r1.Message)
	for _, tc := range r1.Message.ToolCalls {
		msgs = append(msgs, Message{Role: "tool", ToolCallID: tc.ID, Content: `{"city":"Lahore","temp_c":24,"sky":"clear"}`})
	}
	var shown strings.Builder
	r2, err := c.Stream(ctx, msgs, tools, 800, func(s string) { shown.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	if r2.Finish != "stop" || !strings.Contains(shown.String(), "24") {
		t.Fatalf("second turn: finish %q, text %q", r2.Finish, shown.String())
	}
	t.Logf("answer: %s (tokens in %d / cached %d / out %d)", shown.String(), r2.Usage.Prompt, r2.Usage.Cached, r2.Usage.Completion)
}
