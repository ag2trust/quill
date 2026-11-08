package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"quill/internal/events"
	"quill/internal/provider"
	"quill/internal/tools"
)

// fakeClient replays scripted steps and records every request.
type fakeClient struct {
	mu    sync.Mutex
	steps []func(ctx context.Context) (provider.Response, error)
	reqs  []provider.Request
}

func (c *fakeClient) Step(ctx context.Context, req provider.Request) (provider.Response, error) {
	c.mu.Lock()
	req.Messages = append([]json.RawMessage(nil), req.Messages...)
	c.reqs = append(c.reqs, req)
	if len(c.steps) == 0 {
		c.mu.Unlock()
		return provider.Response{}, errors.New("fake: no more steps")
	}
	f := c.steps[0]
	c.steps = c.steps[1:]
	c.mu.Unlock()
	return f(ctx)
}

func replies(rs ...provider.Response) *fakeClient {
	c := &fakeClient{}
	for _, r := range rs {
		c.steps = append(c.steps, func(context.Context) (provider.Response, error) { return r, nil })
	}
	return c
}

// recStore records appended messages; err makes every Append fail.
type recStore struct {
	msgs []json.RawMessage
	err  error
}

func (s *recStore) Append(m json.RawMessage) error {
	if s.err != nil {
		return s.err
	}
	s.msgs = append(s.msgs, append(json.RawMessage(nil), m...))
	return nil
}

// blockShell blocks until ctx is done, after signalling started.
type blockShell struct{ started chan struct{} }

func (s blockShell) Run(ctx context.Context, command string) tools.Result {
	close(s.started)
	<-ctx.Done()
	return tools.Result{Content: "killed", IsError: true}
}

type harness struct {
	loop  *Loop
	ws    string
	store *recStore
	out   *bytes.Buffer
}

func newHarness(t *testing.T, c provider.Client, shell tools.Shell) *harness {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(tools.WorkspaceWrite, ws, shell)
	h := &harness{ws: ws, store: &recStore{}, out: &bytes.Buffer{}}
	h.loop = &Loop{
		Client:   c,
		Registry: reg,
		Store:    h.store,
		Events:   events.NewWriter(h.out),
		Opts: Options{
			Model: "claude-opus-5-5", Effort: "high", System: "sys",
			Tools: reg.Definitions(), MaxSteps: 100, MaxTokens: 2000000,
		},
	}
	return h
}

type event struct {
	Type string      `json:"type"`
	Item events.Item `json:"item"`
}

func (h *harness) events(t *testing.T) []event {
	t.Helper()
	var evs []event
	for _, l := range strings.Split(strings.TrimSpace(h.out.String()), "\n") {
		if l == "" {
			continue
		}
		var e event
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("bad event %q: %v", l, err)
		}
		evs = append(evs, e)
	}
	return evs
}

func use(id, name, input string) provider.ToolUse {
	return provider.ToolUse{ID: id, Name: name, Input: json.RawMessage(input)}
}

// reply builds a response whose Message matches its Text and ToolUses.
func reply(text, stop string, u events.Usage, uses ...provider.ToolUse) provider.Response {
	var content []any
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, tu := range uses {
		content = append(content, map[string]any{"type": "tool_use", "id": tu.ID, "name": tu.Name, "input": tu.Input})
	}
	if content == nil {
		content = []any{}
	}
	msg, err := json.Marshal(map[string]any{"role": "assistant", "content": content})
	if err != nil {
		panic(err)
	}
	return provider.Response{Message: msg, StopReason: stop, Text: text, ToolUses: uses, Usage: u}
}

var (
	firstMsg = json.RawMessage(`{"role":"user","content":[{"type":"text","text":"do it"}]}`)
	u1       = events.Usage{Input: 10, Output: 5, CacheRead: 100, CacheWrite: 1}
)

const blockerOK = `{"reason":"conflict","summary":"s","details":"d","question":"q?"}`

type userMsg struct {
	Role    string       `json:"role"`
	Content []toolResult `json:"content"`
}

func decodeUser(t *testing.T, m json.RawMessage) userMsg {
	t.Helper()
	var u userMsg
	if err := json.Unmarshal(m, &u); err != nil {
		t.Fatal(err)
	}
	return u
}

func roles(t *testing.T, msgs []json.RawMessage) []string {
	t.Helper()
	var out []string
	for _, m := range msgs {
		var r struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(m, &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r.Role)
	}
	return out
}

func sameMsgs(t *testing.T, got, want []json.RawMessage) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d", len(got), len(want))
	}
	for i := range got {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("message %d: got %s, want %s", i, got[i], want[i])
		}
	}
}

func TestCompletesOnText(t *testing.T) {
	r1 := reply("all done", "end_turn", u1)
	c := replies(r1)
	h := newHarness(t, c, nil)
	res := h.loop.Run(context.Background(), context.Background(), firstMsg)

	if res.Status != events.StatusCompleted || res.FinalText != "all done" || res.Steps != 1 || res.Usage != u1 {
		t.Fatalf("result %+v", res)
	}
	if res.Error != nil || res.Blocker != nil {
		t.Fatalf("unexpected error/blocker %+v", res)
	}
	sameMsgs(t, h.loop.History, []json.RawMessage{firstMsg, r1.Message})
	sameMsgs(t, h.store.msgs, h.loop.History)
	req := c.reqs[0]
	if req.Model != "claude-opus-5-5" || req.Effort != "high" || req.System != "sys" || !bytes.Equal(req.Tools, h.loop.Opts.Tools) {
		t.Fatalf("request %+v", req)
	}
	evs := h.events(t)
	if len(evs) != 1 || evs[0].Type != "item.completed" || evs[0].Item.Type != "message" || evs[0].Item.Text != "all done" {
		t.Fatalf("events %+v", evs)
	}
}

func TestRunsToolThenCompletes(t *testing.T) {
	r1 := reply("", "tool_use", u1, use("t1", "write", `{"path":"a.txt","content":"hi"}`))
	r2 := reply("wrote it", "end_turn", u1)
	c := replies(r1, r2)
	h := newHarness(t, c, nil)
	res := h.loop.Run(context.Background(), context.Background(), firstMsg)

	if res.Status != events.StatusCompleted || res.FinalText != "wrote it" || res.Steps != 2 {
		t.Fatalf("result %+v", res)
	}
	if want := (events.Usage{Input: 20, Output: 10, CacheRead: 200, CacheWrite: 2}); res.Usage != want {
		t.Fatalf("usage %+v", res.Usage)
	}
	if b, err := os.ReadFile(filepath.Join(h.ws, "a.txt")); err != nil || string(b) != "hi" {
		t.Fatalf("file %q %v", b, err)
	}
	if got := roles(t, h.loop.History); !reflect.DeepEqual(got, []string{"user", "assistant", "user", "assistant"}) {
		t.Fatalf("roles %v", got)
	}
	if !bytes.Equal(h.loop.History[1], r1.Message) || !bytes.Equal(h.loop.History[3], r2.Message) {
		t.Fatal("assistant messages not stored verbatim")
	}
	results := decodeUser(t, h.loop.History[2])
	if len(results.Content) != 1 || results.Content[0].Type != "tool_result" || results.Content[0].ToolUseID != "t1" || results.Content[0].IsError {
		t.Fatalf("tool results %+v", results)
	}
	sameMsgs(t, h.store.msgs, h.loop.History)
	sameMsgs(t, c.reqs[1].Messages, h.loop.History[:3])

	evs := h.events(t)
	if len(evs) != 3 {
		t.Fatalf("events %+v", evs)
	}
	if evs[0].Type != "item.started" || evs[0].Item.Type != "tool_call" || evs[0].Item.ID != "t1" || evs[0].Item.Name != "write" ||
		string(evs[0].Item.Input) != `{"path":"a.txt","content":"hi"}` {
		t.Errorf("event 0 %+v", evs[0])
	}
	if evs[1].Type != "item.completed" || evs[1].Item.Type != "tool_call" || evs[1].Item.ID != "t1" || evs[1].Item.IsError ||
		evs[1].Item.Text != results.Content[0].Content {
		t.Errorf("event 1 %+v", evs[1])
	}
	if evs[2].Type != "item.completed" || evs[2].Item.Type != "message" || evs[2].Item.Text != "wrote it" {
		t.Errorf("event 2 %+v", evs[2])
	}
}

func TestParallelToolsOneResultMessage(t *testing.T) {
	r1 := reply("two at once", "tool_use", u1,
		use("w", "write", `{"path":"a.txt","content":"hello"}`),
		use("r", "read", `{"path":"a.txt"}`))
	c := replies(r1, reply("ok", "end_turn", u1))
	h := newHarness(t, c, nil)
	res := h.loop.Run(context.Background(), context.Background(), firstMsg)

	if res.Status != events.StatusCompleted || res.FinalText != "ok" {
		t.Fatalf("result %+v", res)
	}
	if len(h.loop.History) != 4 {
		t.Fatalf("history has %d messages", len(h.loop.History))
	}
	results := decodeUser(t, h.loop.History[2])
	if results.Role != "user" || len(results.Content) != 2 {
		t.Fatalf("results %+v", results)
	}
	if results.Content[0].ToolUseID != "w" || results.Content[1].ToolUseID != "r" {
		t.Fatalf("order %+v", results.Content)
	}
	// Sequential, in order: the read sees the write.
	if results.Content[1].IsError || !strings.Contains(results.Content[1].Content, "hello") {
		t.Fatalf("read result %+v", results.Content[1])
	}
}

func TestBlockerStopsWithoutRunningOthers(t *testing.T) {
	r1 := reply("need a decision", "tool_use", u1,
		use("w", "write", `{"path":"a.txt","content":"x"}`),
		use("b", tools.BlockerTool, blockerOK))
	c := replies(r1)
	h := newHarness(t, c, nil)
	res := h.loop.Run(context.Background(), context.Background(), firstMsg)

	if res.Status != events.StatusBlocked || res.FinalText != "need a decision" || res.Steps != 1 {
		t.Fatalf("result %+v", res)
	}
	want := events.Blocker{Reason: "conflict", Summary: "s", Details: "d", Question: "q?"}
	if res.Blocker == nil || *res.Blocker != want || res.Error != nil {
		t.Fatalf("blocker %+v error %+v", res.Blocker, res.Error)
	}
	if _, err := os.Stat(filepath.Join(h.ws, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("file was written: %v", err)
	}
	sameMsgs(t, h.loop.History, []json.RawMessage{firstMsg, r1.Message})
	sameMsgs(t, h.store.msgs, h.loop.History)
	for _, e := range h.events(t) {
		if e.Item.Type == "tool_call" {
			t.Errorf("tool event %+v", e)
		}
	}
}

func TestBlockerInvalidInputContinues(t *testing.T) {
	r1 := reply("", "tool_use", u1,
		use("b", tools.BlockerTool, `{"reason":"conflict","summary":"s","details":"d"}`),
		use("w", "write", `{"path":"a.txt","content":"x"}`))
	c := replies(r1, reply("done", "end_turn", u1))
	h := newHarness(t, c, nil)
	res := h.loop.Run(context.Background(), context.Background(), firstMsg)

	if res.Status != events.StatusCompleted || res.Blocker != nil {
		t.Fatalf("result %+v", res)
	}
	results := decodeUser(t, h.loop.History[2])
	if len(results.Content) != 2 {
		t.Fatalf("results %+v", results)
	}
	b := results.Content[0]
	if b.ToolUseID != "b" || !b.IsError || !strings.Contains(b.Content, "question") {
		t.Fatalf("blocker result %+v", b)
	}
	if results.Content[1].ToolUseID != "w" || results.Content[1].IsError {
		t.Fatalf("write result %+v", results.Content[1])
	}
	if _, err := os.Stat(filepath.Join(h.ws, "a.txt")); err != nil {
		t.Fatalf("write did not run: %v", err)
	}
	evs := h.events(t)
	if len(evs) < 2 || evs[0].Item.Name != tools.BlockerTool || evs[1].Type != "item.completed" || !evs[1].Item.IsError {
		t.Fatalf("events %+v", evs)
	}
}

func TestMaxSteps(t *testing.T) {
	c := &fakeClient{}
	for i := 0; i < 5; i++ {
		c.steps = append(c.steps, func(context.Context) (provider.Response, error) {
			return reply("looking", "tool_use", u1, use("l", "list", `{}`)), nil
		})
	}
	h := newHarness(t, c, nil)
	h.loop.Opts.MaxSteps = 2
	res := h.loop.Run(context.Background(), context.Background(), firstMsg)

	if res.Status != events.StatusLimitReached || res.Steps != 2 || len(c.reqs) != 2 || res.FinalText != "looking" || res.Error != nil {
		t.Fatalf("result %+v, %d requests", res, len(c.reqs))
	}
	// Tool results of the last step are saved, so a resume can continue.
	if got := roles(t, h.store.msgs); !reflect.DeepEqual(got, []string{"user", "assistant", "user", "assistant", "user"}) {
		t.Fatalf("roles %v", got)
	}
}

func TestMaxTokens(t *testing.T) {
	r1 := reply("", "tool_use", events.Usage{Input: 10, CacheRead: 5000}, use("l", "list", `{}`))
	r2 := reply("", "tool_use", events.Usage{Input: 40, Output: 30, CacheWrite: 20}, use("l2", "list", `{}`))
	c := replies(r1, r2, reply("never", "end_turn", u1))
	h := newHarness(t, c, nil)
	h.loop.Opts.MaxTokens = 100
	res := h.loop.Run(context.Background(), context.Background(), firstMsg)

	// Step 1 is 10 billable (cache reads excluded); step 2 brings it to 100.
	if res.Status != events.StatusLimitReached || res.Steps != 2 || res.Error != nil {
		t.Fatalf("result %+v", res)
	}
	if res.Usage.Billable() != 100 || res.Usage.CacheRead != 5000 {
		t.Fatalf("usage %+v", res.Usage)
	}
}

func TestRefusal(t *testing.T) {
	c := replies(reply("I can't help with that", "refusal", u1))
	h := newHarness(t, c, nil)
	res := h.loop.Run(context.Background(), context.Background(), firstMsg)

	if res.Status != events.StatusProviderError || res.FinalText != "I can't help with that" || res.Steps != 1 {
		t.Fatalf("result %+v", res)
	}
	if res.Error == nil || *res.Error != provider.Refused().Info {
		t.Fatalf("error %+v", res.Error)
	}
	if len(h.store.msgs) != 2 {
		t.Fatalf("store has %d messages", len(h.store.msgs))
	}
}

func TestProviderErrorKind(t *testing.T) {
	cases := []struct {
		name string
		err  error
		kind string
	}{
		{"rate_limited", &anthropic.Error{StatusCode: 429, Message: "slow down", RequestID: "req_1"}, "rate_limited"},
		{"auth", &anthropic.Error{StatusCode: 401}, "auth"},
		{"network", errors.New("connection refused"), "network"},
		// A context error while neither context is done is still a failure.
		{"stray_context_error", context.Canceled, "network"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &fakeClient{steps: []func(context.Context) (provider.Response, error){
				func(context.Context) (provider.Response, error) {
					return reply("", "tool_use", u1, use("l", "list", `{}`)), nil
				},
				func(context.Context) (provider.Response, error) { return provider.Response{}, tc.err },
			}}
			h := newHarness(t, c, nil)
			res := h.loop.Run(context.Background(), context.Background(), firstMsg)
			if res.Status != events.StatusProviderError || res.Error == nil || res.Error.Kind != tc.kind {
				t.Fatalf("result %+v error %+v", res, res.Error)
			}
			if res.Steps != 1 || res.Usage != u1 {
				t.Fatalf("steps %d usage %+v", res.Steps, res.Usage)
			}
			if tc.kind == "rate_limited" && (res.Error.HTTPStatus != 429 || res.Error.RequestID != "req_1") {
				t.Fatalf("error %+v", res.Error)
			}
		})
	}
}

func TestMaxTokensStopContinues(t *testing.T) {
	c := replies(reply("part one", "max_tokens", u1), reply("part two", "end_turn", u1))
	h := newHarness(t, c, nil)
	res := h.loop.Run(context.Background(), context.Background(), firstMsg)

	if res.Status != events.StatusCompleted || res.FinalText != "part two" || res.Steps != 2 {
		t.Fatalf("result %+v", res)
	}
	want := `{"role":"user","content":[{"type":"text","text":"Your previous response hit the output limit. Continue, using smaller steps."}]}`
	if len(h.loop.History) != 4 || string(h.loop.History[2]) != want {
		t.Fatalf("history %s", h.loop.History)
	}
	sameMsgs(t, c.reqs[1].Messages, h.loop.History[:3])
	sameMsgs(t, h.store.msgs, h.loop.History)
}

func TestCancelDuringTool(t *testing.T) {
	r1 := reply("running", "tool_use", u1,
		use("s", "shell", `{"command":"sleep 100"}`),
		use("w", "write", `{"path":"a.txt","content":"x"}`))
	c := replies(r1)
	shell := blockShell{started: make(chan struct{})}
	h := newHarness(t, c, shell)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-shell.started
		cancel()
	}()
	res := h.loop.Run(ctx, context.Background(), firstMsg)

	if res.Status != events.StatusCancelled || res.FinalText != "running" || res.Steps != 1 || res.Error != nil {
		t.Fatalf("result %+v", res)
	}
	if _, err := os.Stat(filepath.Join(h.ws, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("second tool ran: %v", err)
	}
	sameMsgs(t, h.loop.History, []json.RawMessage{firstMsg, r1.Message})
	sameMsgs(t, h.store.msgs, h.loop.History)
}

func TestTimeoutIsLimit(t *testing.T) {
	t.Run("during_step", func(t *testing.T) {
		c := &fakeClient{steps: []func(context.Context) (provider.Response, error){
			func(ctx context.Context) (provider.Response, error) {
				<-ctx.Done()
				return provider.Response{}, ctx.Err()
			},
		}}
		h := newHarness(t, c, nil)
		limitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		res := h.loop.Run(context.Background(), limitCtx, firstMsg)
		if res.Status != events.StatusLimitReached || res.Error != nil || res.Steps != 0 {
			t.Fatalf("result %+v", res)
		}
	})
	t.Run("during_tool", func(t *testing.T) {
		r1 := reply("", "tool_use", u1, use("s", "shell", `{"command":"sleep 100"}`))
		shell := blockShell{started: make(chan struct{})}
		h := newHarness(t, replies(r1), shell)
		limitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		res := h.loop.Run(context.Background(), limitCtx, firstMsg)
		if res.Status != events.StatusLimitReached || res.Error != nil || res.Steps != 1 {
			t.Fatalf("result %+v", res)
		}
		sameMsgs(t, h.store.msgs, []json.RawMessage{firstMsg, r1.Message})
	})
}

func TestCancelBeatsLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	limitCtx, cancelLimit := context.WithCancel(context.Background())
	cancelLimit()
	c := &fakeClient{steps: []func(context.Context) (provider.Response, error){
		func(ctx context.Context) (provider.Response, error) { return provider.Response{}, ctx.Err() },
	}}
	h := newHarness(t, c, nil)
	if res := h.loop.Run(ctx, limitCtx, firstMsg); res.Status != events.StatusCancelled {
		t.Fatalf("result %+v", res)
	}
}

func TestStoreErrorIsInternal(t *testing.T) {
	c := replies(reply("done", "end_turn", u1))
	h := newHarness(t, c, nil)
	h.store.err = errors.New("disk full")
	res := h.loop.Run(context.Background(), context.Background(), firstMsg)

	if res.Status != events.StatusInternalError || res.Error == nil || res.Error.Kind != "internal" || res.Error.Message != "disk full" {
		t.Fatalf("result %+v error %+v", res, res.Error)
	}
	if len(c.reqs) != 0 {
		t.Fatalf("stepped with unsaved history: %d requests", len(c.reqs))
	}
}

func TestResumeCountsThisRunOnly(t *testing.T) {
	prior := []json.RawMessage{firstMsg, json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"earlier"}]}`)}
	next := json.RawMessage(`{"role":"user","content":[{"type":"text","text":"more"}]}`)
	c := replies(reply("again", "end_turn", u1))
	h := newHarness(t, c, nil)
	h.loop.History = append([]json.RawMessage(nil), prior...)
	res := h.loop.Run(context.Background(), context.Background(), next)

	if res.Status != events.StatusCompleted || res.Steps != 1 || res.Usage != u1 {
		t.Fatalf("result %+v", res)
	}
	sameMsgs(t, c.reqs[0].Messages, append(append([]json.RawMessage(nil), prior...), next))
	// Only this run's messages go to the store; earlier ones are already saved.
	sameMsgs(t, h.store.msgs, []json.RawMessage{next, h.loop.History[3]})
}
