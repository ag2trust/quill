// Package mcp is a minimal MCP client. It gives the model the tools of one
// caller-configured server over Streamable HTTP: list once, then call.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"quill/internal/tools"
)

const (
	// MaxTools is the most tools a server may offer.
	MaxTools = 64
	// MaxDefinitionBytes caps the JSON of all tool definitions together.
	MaxDefinitionBytes = 100000
	// StartTimeout limits connecting and listing at the start of a run.
	StartTimeout = 30 * time.Second
	// CallTimeout limits one tool call, like a shell command.
	CallTimeout = 10 * time.Minute
	// closeGrace limits the wait for the server to end the MCP session.
	closeGrace = 2 * time.Second
)

// toolName is what the Claude API accepts as a tool name.
var toolName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Error is a failure to reach or use the server at the start of a run.
// HTTPStatus is the server's last HTTP status when that was not a success.
type Error struct {
	Message    string
	HTTPStatus int
}

func (e *Error) Error() string { return e.Message }

// Client talks to one MCP server. It is safe for concurrent use.
type Client struct {
	endpoint, token string
	client          *sdk.Client
	transport       *sdk.StreamableClientTransport
	status          atomic.Int32 // last HTTP status, 0 after a failed request
	callTimeout     time.Duration

	mu      sync.Mutex
	session *sdk.ClientSession
}

// New returns a client for the server at endpoint. A non-empty token is sent
// as a bearer token on every request. Nothing is sent until List, Connect or
// Call.
func New(endpoint, token, version string) *Client {
	c := &Client{endpoint: endpoint, token: token, callTimeout: CallTimeout}
	c.client = sdk.NewClient(&sdk.Implementation{Name: "quill", Version: version}, &sdk.ClientOptions{
		// No roots, sampling or elicitation, and no automatic answers to a
		// server asking for input.
		Capabilities:   &sdk.ClientCapabilities{},
		MultiRoundTrip: &sdk.MultiRoundTripOptions{Disabled: true},
	})
	c.transport = &sdk.StreamableClientTransport{
		Endpoint: endpoint,
		HTTPClient: &http.Client{
			Transport: &authTransport{c: c, base: http.DefaultTransport.(*http.Transport).Clone()},
			// The token goes to the configured server only.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		DisableStandaloneSSE: true,
	}
	return c
}

// authTransport adds the bearer token and records the response status.
type authTransport struct {
	c    *Client
	base http.RoundTripper
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.c.token != "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.c.token)
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		t.c.status.Store(0)
		return nil, err
	}
	t.c.status.Store(int32(resp.StatusCode))
	return resp, nil
}

// Connect opens the MCP session, within StartTimeout. The error is an *Error.
func (c *Client) Connect(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, StartTimeout)
	defer cancel()
	if _, err := c.connect(ctx); err != nil {
		return c.startError("connect", err)
	}
	return nil
}

// List connects and returns the server's tools as Claude tool definitions
// named mcp__<name>, sorted by name. It fails closed, with an *Error, on
// anything the Claude API would reject and on the caps above.
func (c *Client) List(ctx context.Context) ([]json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, StartTimeout)
	defer cancel()
	s, err := c.connect(ctx)
	if err != nil {
		return nil, c.startError("connect", err)
	}
	var list []*sdk.Tool
	for t, err := range s.Tools(ctx, nil) {
		if err != nil {
			return nil, c.startError("list tools", err)
		}
		if len(list) == MaxTools {
			return nil, &Error{Message: fmt.Sprintf("MCP server offers more than %d tools", MaxTools)}
		}
		list = append(list, t)
	}
	defs, err := definitions(list)
	if err != nil {
		return nil, &Error{Message: err.Error()}
	}
	return defs, nil
}

type definition struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"` // map keys marshal sorted
}

// definitions converts the server's tools, checking names, schemas and caps.
func definitions(list []*sdk.Tool) ([]json.RawMessage, error) {
	if len(list) == 0 {
		return nil, errors.New("MCP server offers no tools")
	}
	if len(list) > MaxTools {
		return nil, fmt.Errorf("MCP server offers more than %d tools", MaxTools)
	}
	list = append([]*sdk.Tool(nil), list...)
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	defs := make([]json.RawMessage, 0, len(list))
	total := 0
	for i, t := range list {
		name := tools.RemotePrefix + t.Name
		if t.Name == "" || !toolName.MatchString(name) {
			return nil, fmt.Errorf("MCP tool name %q is not usable: %s must match %s", t.Name, name, toolName)
		}
		if i > 0 && list[i-1].Name == t.Name {
			return nil, fmt.Errorf("MCP server lists tool %q twice", t.Name)
		}
		schema, ok := t.InputSchema.(map[string]any)
		if !ok || schema["type"] != "object" {
			return nil, fmt.Errorf("MCP tool %q has no object input schema", t.Name)
		}
		def, err := json.Marshal(definition{name, t.Description, schema})
		if err != nil {
			return nil, fmt.Errorf("MCP tool %q: %v", t.Name, err)
		}
		if total += len(def); total > MaxDefinitionBytes {
			return nil, fmt.Errorf("MCP tool definitions exceed %d bytes", MaxDefinitionBytes)
		}
		defs = append(defs, def)
	}
	return defs, nil
}

// Call runs tools/call for the tool with the given mcp__ name, passing input
// through unchecked: the server validates it. Every failure is a tool error
// for the model. The result is untrusted content.
func (c *Client) Call(ctx context.Context, name string, input json.RawMessage) tools.Result {
	callCtx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()
	params := &sdk.CallToolParams{Name: strings.TrimPrefix(name, tools.RemotePrefix)}
	if len(input) > 0 {
		params.Arguments = input
	}
	s, err := c.connect(callCtx)
	var res *sdk.CallToolResult
	if err == nil {
		res, err = s.CallTool(callCtx, params)
	}
	switch {
	case err == nil:
		return render(res)
	case ctx.Err() != nil:
		return errResult("[cancelled]")
	case callCtx.Err() != nil:
		return errResult("[MCP call stopped after %s timeout]", c.callTimeout)
	}
	// An error that is not the server's JSON-RPC answer may have broken the
	// connection: the next call opens a new one.
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) {
		c.drop(s)
	}
	return errResult("MCP call failed: %s", c.clean(err))
}

// render turns a call result into text: text blocks joined by newlines, and
// a placeholder for anything else.
func render(res *sdk.CallToolResult) tools.Result {
	if res.NeedsInput() {
		return errResult("MCP call failed: the server asked for input, which is not supported")
	}
	parts := make([]string, 0, len(res.Content))
	for _, block := range res.Content {
		if t, ok := block.(*sdk.TextContent); ok {
			parts = append(parts, t.Text)
		} else {
			parts = append(parts, "[non-text content omitted]")
		}
	}
	text := strings.Join(parts, "\n")
	if res.IsError && text == "" {
		text = "MCP tool reported an error without a message"
	}
	return tools.Result{Content: text, IsError: res.IsError}
}

func errResult(format string, a ...any) tools.Result {
	return tools.Result{Content: fmt.Sprintf(format, a...), IsError: true}
}

// Close ends the MCP session, if one is open.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != nil {
		closeSession(c.session)
		c.session = nil
	}
}

// closeSession asks the server to end s but does not wait long: a server
// still busy with a cancelled call may not answer until it is done.
func closeSession(s *sdk.ClientSession) {
	done := make(chan struct{})
	go func() {
		_ = s.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(closeGrace):
	}
}

func (c *Client) connect(ctx context.Context) (*sdk.ClientSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		s, err := c.client.Connect(ctx, c.transport, nil)
		if err != nil {
			return nil, err
		}
		c.session = s
	}
	return c.session, nil
}

// drop closes s and forgets it, so the next call reconnects.
func (c *Client) drop(s *sdk.ClientSession) {
	if s == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == s {
		c.session = nil
	}
	closeSession(s)
}

func (c *Client) startError(what string, err error) *Error {
	e := &Error{Message: "MCP " + what + " failed: " + c.clean(err)}
	if s := int(c.status.Load()); s != 0 && (s < 200 || s > 299) {
		e.HTTPStatus = s
	}
	return e
}

// clean is err's text without the server URL or the token: error text ends up
// in events and the session file.
func (c *Client) clean(err error) string {
	s := strings.ReplaceAll(err.Error(), c.endpoint, "<mcp-url>")
	if c.token != "" {
		s = strings.ReplaceAll(s, c.token, "<token>")
	}
	return s
}
