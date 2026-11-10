package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
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

	counts := topLevelKeys(t, body)
	for _, k := range []string{"messages", "system", "tools", "output_config", "context_management", "cache_control", "max_tokens", "model"} {
		if counts[k] != 1 {
			t.Errorf("key %q appears %d times at top level", k, counts[k])
		}
	}
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
	reqs := srv.Requests()
	if resp.Text != "ok" || len(reqs) != 3 {
		t.Fatalf("text %q, %d requests", resp.Text, len(reqs))
	}
	for i := 1; i < len(reqs); i++ {
		if !bytes.Equal(reqs[i], reqs[0]) {
			t.Errorf("retry %d body differs:\n%s\n%s", i, reqs[i], reqs[0])
		}
	}
}

func TestStepRequestsCompaction(t *testing.T) {
	srv := fakeapi.New(t, fakeapi.TextReply("ok", events.Usage{}), fakeapi.TextReply("ok", events.Usage{}))
	c := newClient(t, srv.URL())
	if _, err := c.Step(context.Background(), simpleRequest()); err != nil {
		t.Fatal(err)
	}
	c.CompactTrigger = 50000
	if _, err := c.Step(context.Background(), simpleRequest()); err != nil {
		t.Fatal(err)
	}
	reqs, hdrs := srv.Requests(), srv.Headers()
	for i, want := range []string{"150000", "50000"} {
		if got := hdrs[i].Values("Anthropic-Beta"); len(got) != 1 || got[0] != "compact-2026-01-12" {
			t.Errorf("request %d anthropic-beta = %q", i, got)
		}
		if n := topLevelKeys(t, reqs[i])["context_management"]; n != 1 {
			t.Fatalf("request %d: context_management appears %d times", i, n)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(reqs[i], &top); err != nil {
			t.Fatal(err)
		}
		var cm struct {
			Edits []struct {
				Type    string `json:"type"`
				Trigger struct {
					Type  string      `json:"type"`
					Value json.Number `json:"value"`
				} `json:"trigger"`
				Instructions string `json:"instructions"`
			} `json:"edits"`
		}
		if err := json.Unmarshal(top["context_management"], &cm); err != nil || len(cm.Edits) != 1 {
			t.Fatalf("request %d context_management = %s (%v)", i, top["context_management"], err)
		}
		e := cm.Edits[0]
		if e.Type != "compact_20260112" || e.Trigger.Type != "input_tokens" || e.Trigger.Value.String() != want {
			t.Errorf("request %d context_management = %s", i, top["context_management"])
		}
		if e.Instructions != compactInstructions || !strings.Contains(e.Instructions, "Do not call any tools") ||
			!strings.Contains(e.Instructions, "<summary></summary>") {
			t.Errorf("request %d instructions = %q", i, e.Instructions)
		}
	}
}

// A reply that compacted starts with a compaction block and reports the
// compaction call only under usage.iterations.
func TestStepCompactionReply(t *testing.T) {
	content := `[{"type":"compaction","content":"Summary: <did> a & b","signature":"sig==","encrypted_content":"enc","future_field":{"a": 1}},` +
		`{"type":"thinking","thinking":"next","signature":"t=="},{"type":"tool_use","id":"toolu_1","name":"shell","input":{"command": "ls"}}]`
	srv := fakeapi.New(t, fakeapi.Reply{Body: `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":` +
		content + `,"stop_reason":"tool_use","usage":{"input_tokens":23000,"output_tokens":1000,` +
		`"cache_read_input_tokens":5,"cache_creation_input_tokens":7,"iterations":[` +
		`{"type":"compaction","input_tokens":180000,"output_tokens":3500,"cache_read_input_tokens":11,"cache_creation_input_tokens":13},` +
		`{"type":"message","input_tokens":23000,"output_tokens":1000,"cache_read_input_tokens":5,"cache_creation_input_tokens":7}]}}`},
		fakeapi.TextReply("done", events.Usage{}))
	c := newClient(t, srv.URL())
	req := simpleRequest()
	resp, err := c.Step(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	wantMsg := `{"role":"assistant","content":` + content + `}`
	if string(resp.Message) != wantMsg {
		t.Fatalf("message:\n got %s\nwant %s", resp.Message, wantMsg)
	}
	if len(resp.ToolUses) != 1 || resp.ToolUses[0].ID != "toolu_1" || resp.Text != "" {
		t.Errorf("tool uses %+v, text %q", resp.ToolUses, resp.Text)
	}
	if want := (events.Usage{Input: 203000, Output: 4500, CacheRead: 16, CacheWrite: 20}); resp.Usage != want {
		t.Errorf("usage %+v, want %+v", resp.Usage, want)
	}

	// Replay: the whole history, compaction block included, goes back verbatim.
	result := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"a.txt"}]}`
	req.Messages = append(req.Messages, resp.Message, json.RawMessage(result))
	if _, err := c.Step(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(srv.Requests()[1], &top); err != nil {
		t.Fatal(err)
	}
	if want := `[{"role":"user","content":"hi"},` + wantMsg + `,` + result + `]`; string(top["messages"]) != want {
		t.Fatalf("replayed messages:\n got %s\nwant %s", top["messages"], want)
	}
}

// topLevelKeys counts each key of the top-level JSON object, duplicates included.
func topLevelKeys(t *testing.T, body []byte) map[string]int {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("body is not an object: %v %v", tok, err)
	}
	counts := map[string]int{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		counts[tok.(string)]++
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return counts
}
