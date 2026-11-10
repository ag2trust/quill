package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"quill/internal/fakemcp"
	"quill/internal/tools"
)

const testToken = "tok-3f9a-secret"

func newClient(t *testing.T, srv *fakemcp.Server, token string) *Client {
	t.Helper()
	c := New(srv.URL(), token, "test")
	t.Cleanup(c.Close)
	return c
}

func startError(t *testing.T, err error) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error = %v (%T), want *Error", err, err)
	}
	return e
}

func TestListNamesSortedWithAuth(t *testing.T) {
	const schema = `{"type":"object","properties":{"q":{"type":"string","description":"<query>"}},"required":["q"]}`
	srv := fakemcp.New(t,
		fakemcp.Tool{Name: "zeta"},
		fakemcp.Tool{Name: "alpha", Description: "Find things.", Schema: schema},
	)
	defs, err := newClient(t, srv, testToken).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 2 {
		t.Fatalf("got %d definitions, want 2", len(defs))
	}
	var first struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal(defs[0], &first); err != nil {
		t.Fatal(err)
	}
	if first.Name != "mcp__alpha" || first.Description != "Find things." {
		t.Errorf("first definition = %s", defs[0])
	}
	var got, want any
	json.Unmarshal(first.InputSchema, &got)
	json.Unmarshal([]byte(schema), &want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("input_schema = %s, want %s", first.InputSchema, schema)
	}
	if !strings.Contains(string(defs[1]), `"name":"mcp__zeta"`) {
		t.Errorf("second definition = %s", defs[1])
	}
	// Listing again gives the same bytes.
	again, err := newClient(t, srv, testToken).List(context.Background())
	if err != nil || string(again[0]) != string(defs[0]) || string(again[1]) != string(defs[1]) {
		t.Errorf("second listing differs: %s, %v", again, err)
	}
	auth := srv.Auth()
	if len(auth) == 0 {
		t.Fatal("server saw no requests")
	}
	for _, a := range auth {
		if a != "Bearer "+testToken {
			t.Fatalf("Authorization = %q, want the bearer token on every request", a)
		}
	}
}

func TestNoTokenNoAuthHeader(t *testing.T) {
	srv := fakemcp.New(t, fakemcp.Tool{Name: "a"})
	if _, err := newClient(t, srv, "").List(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, a := range srv.Auth() {
		if a != "" {
			t.Fatalf("Authorization = %q, want none", a)
		}
	}
}

func TestDefinitionsFailClosed(t *testing.T) {
	object := map[string]any{"type": "object"}
	tool := func(name string) *sdk.Tool { return &sdk.Tool{Name: name, InputSchema: object} }
	many := make([]*sdk.Tool, MaxTools+1)
	for i := range many {
		many[i] = tool(fmt.Sprintf("t%d", i))
	}
	big := tool("big")
	big.Description = strings.Repeat("x", MaxDefinitionBytes)
	cases := map[string][]*sdk.Tool{
		"no tools":         nil,
		"empty name":       {tool("")},
		"dot in name":      {tool("a.b")},
		"space in name":    {tool("a b")},
		"name too long":    {tool(strings.Repeat("n", 60))},
		"duplicate":        {tool("a"), tool("b"), tool("a")},
		"schema not a map": {{Name: "a", InputSchema: "object"}},
		"schema not object": {{Name: "a", InputSchema: map[string]any{
			"type": "string"}}},
		"too many tools": many,
		"too many bytes": {big},
	}
	for name, list := range cases {
		if defs, err := definitions(list); err == nil {
			t.Errorf("%s: accepted: %s", name, defs)
		}
	}
	// The longest name that fits, and exactly MaxTools tools, are accepted.
	if _, err := definitions([]*sdk.Tool{tool(strings.Repeat("n", 59))}); err != nil {
		t.Errorf("59-byte name: %v", err)
	}
	if _, err := definitions(many[:MaxTools]); err != nil {
		t.Errorf("%d tools: %v", MaxTools, err)
	}
}

func TestListRejectsBadServerTools(t *testing.T) {
	srv := fakemcp.New(t, fakemcp.Tool{Name: "ok"}, fakemcp.Tool{Name: "has.dot"})
	_, err := newClient(t, srv, "").List(context.Background())
	if e := startError(t, err); !strings.Contains(e.Message, "has.dot") {
		t.Errorf("message = %q", e.Message)
	}
}

func TestCall(t *testing.T) {
	srv := fakemcp.New(t,
		fakemcp.Tool{Name: "echo"},
		fakemcp.Tool{Name: "mixed", Call: func(context.Context, json.RawMessage) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{
				&sdk.TextContent{Text: "one"},
				&sdk.ImageContent{Data: []byte("png"), MIMEType: "image/png"},
				&sdk.TextContent{Text: "two"},
			}}, nil
		}},
		fakemcp.Tool{Name: "fails", Call: func(context.Context, json.RawMessage) (*sdk.CallToolResult, error) {
			r := fakemcp.Text("no such record")
			r.IsError = true
			return r, nil
		}},
		fakemcp.Tool{Name: "empty_error", Call: func(context.Context, json.RawMessage) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{IsError: true}, nil
		}},
	)
	c := newClient(t, srv, testToken)
	ctx := context.Background()

	// Arguments reach the server as the model wrote them, unchecked.
	in := `{"text":"hi","extra":[1,2]}`
	if r := c.Call(ctx, "mcp__echo", json.RawMessage(in)); r.IsError || r.Content != in {
		t.Errorf("echo = %+v", r)
	}
	if r := c.Call(ctx, "mcp__mixed", json.RawMessage(`{}`)); r.IsError || r.Content != "one\n[non-text content omitted]\ntwo" {
		t.Errorf("mixed = %+v", r)
	}
	if r := c.Call(ctx, "mcp__fails", json.RawMessage(`{}`)); !r.IsError || r.Content != "no such record" {
		t.Errorf("fails = %+v", r)
	}
	if r := c.Call(ctx, "mcp__empty_error", json.RawMessage(`{}`)); !r.IsError || r.Content == "" {
		t.Errorf("empty_error = %+v", r)
	}
	// The server's JSON-RPC error is a tool error, and the session survives it.
	if r := c.Call(ctx, "mcp__missing", json.RawMessage(`{}`)); !r.IsError || !strings.HasPrefix(r.Content, "MCP call failed: ") {
		t.Errorf("missing = %+v", r)
	}
	if r := c.Call(ctx, "mcp__echo", json.RawMessage(`{}`)); r.IsError {
		t.Errorf("echo after an error = %+v", r)
	}
}

func TestCallInputRequiredIsToolError(t *testing.T) {
	srv := fakemcp.New(t, fakemcp.Tool{Name: "ask", Call: func(context.Context, json.RawMessage) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{InputRequests: sdk.InputRequestMap{
			"r1": &sdk.ElicitParams{Message: "which one?"},
		}}, nil
	}})
	r := newClient(t, srv, "").Call(context.Background(), "mcp__ask", json.RawMessage(`{}`))
	if !r.IsError || !strings.HasPrefix(r.Content, "MCP call failed: ") {
		t.Errorf("ask = %+v", r)
	}
}

func TestCallTransportFailureThenRecovery(t *testing.T) {
	srv := fakemcp.New(t, fakemcp.Tool{Name: "echo"})
	c := newClient(t, srv, testToken)
	ctx := context.Background()
	if r := c.Call(ctx, "mcp__echo", json.RawMessage(`{}`)); r.IsError {
		t.Fatalf("first call = %+v", r)
	}
	srv.SetStatus(500)
	r := c.Call(ctx, "mcp__echo", json.RawMessage(`{}`))
	if !r.IsError || !strings.HasPrefix(r.Content, "MCP call failed: ") {
		t.Errorf("call during failure = %+v", r)
	}
	if strings.Contains(r.Content, srv.URL()) || strings.Contains(r.Content, testToken) {
		t.Errorf("tool error carries the URL or token: %q", r.Content)
	}
	srv.SetStatus(0)
	if r := c.Call(ctx, "mcp__echo", json.RawMessage(`{}`)); r.IsError {
		t.Errorf("call after recovery = %+v", r)
	}
}

func TestCallUnreachable(t *testing.T) {
	srv := fakemcp.New(t, fakemcp.Tool{Name: "echo"})
	c := newClient(t, srv, testToken)
	srv.Close()
	r := c.Call(context.Background(), "mcp__echo", json.RawMessage(`{}`))
	if !r.IsError || strings.Contains(r.Content, srv.URL()) {
		t.Errorf("call = %+v", r)
	}
}

// blockingServer has a tool that does not answer until the test ends.
func blockingServer(t *testing.T) *fakemcp.Server {
	release := make(chan struct{})
	srv := fakemcp.New(t, fakemcp.Tool{Name: "hang", Call: func(context.Context, json.RawMessage) (*sdk.CallToolResult, error) {
		<-release
		return fakemcp.Text("late"), nil
	}})
	t.Cleanup(func() { close(release) }) // before the server's own cleanup
	return srv
}

func TestCallTimeout(t *testing.T) {
	c := newClient(t, blockingServer(t), "")
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.callTimeout = 200 * time.Millisecond
	start := time.Now()
	r := c.Call(context.Background(), "mcp__hang", json.RawMessage(`{}`))
	if !r.IsError || !strings.Contains(r.Content, "timeout") {
		t.Errorf("call = %+v", r)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("timed-out call took %s", d)
	}
}

func TestCallCancelled(t *testing.T) {
	c := newClient(t, blockingServer(t), "")
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	r := c.Call(ctx, "mcp__hang", json.RawMessage(`{}`))
	if !r.IsError || r.Content != "[cancelled]" {
		t.Errorf("call = %+v", r)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("cancelled call took %s", d)
	}
	// The server is still busy with the call; closing does not wait for it.
	start = time.Now()
	c.Close()
	if d := time.Since(start); d > closeGrace+2*time.Second {
		t.Errorf("close took %s", d)
	}
}

func TestStartFailures(t *testing.T) {
	down := fakemcp.New(t, fakemcp.Tool{Name: "a"})
	down.Close()
	_, err := New(down.URL(), testToken, "test").List(context.Background())
	e := startError(t, err)
	if e.HTTPStatus != 0 || e.Message == "" {
		t.Errorf("unreachable: %+v", e)
	}
	if strings.Contains(e.Message, down.URL()) || strings.Contains(e.Message, testToken) {
		t.Errorf("message carries the URL or token: %q", e.Message)
	}

	denied := fakemcp.New(t, fakemcp.Tool{Name: "a"})
	denied.SetStatus(401)
	_, err = New(denied.URL(), testToken, "test").List(context.Background())
	if e := startError(t, err); e.HTTPStatus != 401 {
		t.Errorf("401 on list: %+v", e)
	}
	if e := startError(t, New(denied.URL(), testToken, "test").Connect(context.Background())); e.HTTPStatus != 401 {
		t.Errorf("401 on connect: %+v", e)
	}
}

// The registry sends only the session's MCP tool names to the client, and
// truncates what comes back like any other tool output.
func TestThroughRegistry(t *testing.T) {
	huge := strings.Repeat("x", 2*tools.OutputCap)
	srv := fakemcp.New(t,
		fakemcp.Tool{Name: "huge", Call: func(context.Context, json.RawMessage) (*sdk.CallToolResult, error) {
			return fakemcp.Text(huge), nil
		}},
		fakemcp.Tool{Name: "unlisted"},
	)
	for _, profile := range []tools.Profile{tools.ReadOnly, tools.ReadWrite} {
		reg := tools.NewRegistry(profile, t.TempDir(), nil)
		reg.SetRemote(newClient(t, srv, ""), []string{"mcp__huge"})
		r := reg.Run(context.Background(), "mcp__huge", json.RawMessage(`{}`))
		if r.IsError || r.Content != tools.Truncate(huge, tools.OutputCap) {
			t.Errorf("%s: huge: error %v, %d bytes", profile, r.IsError, len(r.Content))
		}
		if r := reg.Run(context.Background(), "mcp__unlisted", json.RawMessage(`{}`)); r.Content != "unknown tool: mcp__unlisted" {
			t.Errorf("%s: unlisted = %+v", profile, r)
		}
	}
}

func TestRenderInputRequired(t *testing.T) {
	var res sdk.CallToolResult
	if err := json.Unmarshal([]byte(`{"resultType":"input_required","inputRequests":{}}`), &res); err != nil {
		t.Fatal(err)
	}
	if !res.NeedsInput() {
		t.Fatal("result does not need input")
	}
	if r := render(&res); !r.IsError || !strings.Contains(r.Content, "asked for input") {
		t.Errorf("render = %+v", r)
	}
}
