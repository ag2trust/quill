package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"quill/internal/events"
	"quill/internal/fakeapi"
)

// newClient points a Claude client at srv through ANTHROPIC_BASE_URL.
func newClient(t *testing.T, url string, opts ...option.RequestOption) *Claude {
	t.Helper()
	t.Setenv("ANTHROPIC_BASE_URL", url)
	return NewClaude("test-key", opts...)
}

func simpleRequest() Request {
	return Request{
		Model:    "claude-opus-5-5",
		Effort:   "high",
		System:   "sys",
		Tools:    json.RawMessage(`[{"name":"shell","input_schema":{"type":"object"}}]`),
		Messages: []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)},
	}
}

func TestStepSendsCacheAndRawHistory(t *testing.T) {
	srv := fakeapi.New(t, fakeapi.TextReply("ok", events.Usage{}))
	c := newClient(t, srv.URL())

	// Whitespace and HTML-significant characters must survive untouched.
	m1 := `{"role": "user", "content":[{"type":"text","text":"fix <b> & run"}]}`
	m2 := `{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"abc"},{"type":"text","text":"ok"}]}`
	tools := `[{"name":"shell", "description":"run <cmd>","input_schema":{"type":"object"}}]`
	req := Request{
		Model:    "claude-opus-5-5",
		Effort:   "high",
		System:   "be terse",
		Tools:    json.RawMessage(tools),
		Messages: []json.RawMessage{json.RawMessage(m1), json.RawMessage(m2)},
	}
	if _, err := c.Step(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	reqs := srv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests", len(reqs))
	}
	body := reqs[0]

	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("invalid body %s: %v", body, err)
	}
	wantMsgs := "[" + m1 + "," + m2 + "]"
	if string(top["messages"]) != wantMsgs || !bytes.Contains(body, []byte(wantMsgs)) {
		t.Fatalf("messages not verbatim:\n got %s\nwant %s", top["messages"], wantMsgs)
	}
	if string(top["tools"]) != tools {
		t.Fatalf("tools not verbatim: %s", top["tools"])
	}
	for _, k := range []string{"temperature", "top_p", "top_k", "thinking"} {
		if _, ok := top[k]; ok {
			t.Errorf("unexpected key %q", k)
		}
	}

	var parsed struct {
		Model        string `json:"model"`
		MaxTokens    int    `json:"max_tokens"`
		CacheControl struct {
			Type string `json:"type"`
		} `json:"cache_control"`
		OutputConfig struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
		System []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Model != "claude-opus-5-5" || parsed.MaxTokens != 32000 {
		t.Errorf("model/max_tokens: %+v", parsed)
	}
	if parsed.CacheControl.Type != "ephemeral" {
		t.Errorf("cache_control: %s", top["cache_control"])
	}
	if parsed.OutputConfig.Effort != "high" {
		t.Errorf("output_config: %s", top["output_config"])
	}
	if len(parsed.System) != 1 || parsed.System[0].Type != "text" || parsed.System[0].Text != "be terse" {
		t.Errorf("system: %s", top["system"])
	}
}

func TestStepParsesToolUse(t *testing.T) {
	input := `{"command": "ls -la"}`
	srv := fakeapi.New(t, fakeapi.ToolReply("toolu_1", "shell", input))
	c := newClient(t, srv.URL())
	resp, err := c.Step(context.Background(), simpleRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("stop reason %q", resp.StopReason)
	}
	if len(resp.ToolUses) != 1 {
		t.Fatalf("tool uses: %+v", resp.ToolUses)
	}
	tu := resp.ToolUses[0]
	if tu.ID != "toolu_1" || tu.Name != "shell" || string(tu.Input) != input {
		t.Errorf("tool use: %+v (input %s)", tu, tu.Input)
	}
}

func TestStepText(t *testing.T) {
	srv := fakeapi.New(t, fakeapi.Reply{Body: `{"id":"msg_1","type":"message","role":"assistant","model":"m",
		"content":[{"type":"text","text":"one"},{"type":"text","text":"two"}],
		"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`})
	c := newClient(t, srv.URL())
	resp, err := c.Step(context.Background(), simpleRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "one\ntwo" || resp.StopReason != "end_turn" || len(resp.ToolUses) != 0 {
		t.Errorf("got %+v", resp)
	}
}

func TestStepKeepsUnknownFields(t *testing.T) {
	content := `[{"type":"thinking","thinking":"plan <x>","signature":"sig==","future_field":{"a": 1}},{"type":"text","text":"done"}]`
	srv := fakeapi.New(t, fakeapi.Reply{Body: `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":` +
		content + `,"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`})
	c := newClient(t, srv.URL())
	resp, err := c.Step(context.Background(), simpleRequest())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"role":"assistant","content":` + content + `}`
	if string(resp.Message) != want {
		t.Fatalf("message:\n got %s\nwant %s", resp.Message, want)
	}
	if resp.Text != "done" {
		t.Errorf("text %q", resp.Text)
	}
}

func TestUsageMapping(t *testing.T) {
	u := events.Usage{Input: 10, Output: 20, CacheRead: 300, CacheWrite: 40}
	srv := fakeapi.New(t, fakeapi.TextReply("hi", u))
	c := newClient(t, srv.URL())
	resp, err := c.Step(context.Background(), simpleRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage != u {
		t.Errorf("usage %+v want %+v", resp.Usage, u)
	}
}

func TestRetriesTransient(t *testing.T) {
	fast := http529()
	srv := fakeapi.New(t, fast, fast, fakeapi.TextReply("ok", events.Usage{}))
	c := newClient(t, srv.URL())
	resp, err := c.Step(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("got %v", err)
	}
	if resp.Text != "ok" || len(srv.Requests()) != 3 {
		t.Errorf("text %q, %d requests", resp.Text, len(srv.Requests()))
	}
}
