// Package mcp is a minimal MCP client. It gives the model the tools of one
// caller-configured server over Streamable HTTP: list once, then call.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

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
	// MaxResponseBytes caps the body of every HTTP response from the server,
	// and one server-sent event. Results are cut to tools.OutputCap anyway.
	MaxResponseBytes = 4 << 20
	// maxErrorBytes caps error text, which the server can influence.
	maxErrorBytes = 500
	// closeGrace limits the wait for the server to end the MCP session.
	closeGrace = 2 * time.Second
)

var errTooLarge = fmt.Errorf("response exceeds %d bytes", MaxResponseBytes)

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
	// netErr describes the last network failure since the current
	// operation began, without the URL or any address.
	netErr                    atomic.Pointer[string]
	callTimeout, startTimeout time.Duration

	mu      sync.Mutex
	session *sdk.ClientSession
}

// New returns a client for the server at endpoint. A non-empty token is sent
// as a bearer token on every request. Nothing is sent until List, Connect or
// Call.
func New(endpoint, token, version string) *Client {
	c := &Client{endpoint: endpoint, token: token, callTimeout: CallTimeout, startTimeout: StartTimeout}
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
		MaxEventSize:         MaxResponseBytes,
	}
	return c
}

// authTransport adds the bearer token, records the response status and any
// network failure, and caps the response body.
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
		t.c.noteNetErr(err)
		return nil, err
	}
	t.c.status.Store(int32(resp.StatusCode))
	resp.Body = &limitedBody{c: t.c, rc: resp.Body, left: MaxResponseBytes + 1}
	return resp, nil
}

// limitedBody fails a read past MaxResponseBytes, whatever the status or
// content type, so a server cannot make Quill hold a huge response.
type limitedBody struct {
	c    *Client
	rc   io.ReadCloser
	left int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		b.c.noteNetErr(errTooLarge)
		return 0, errTooLarge
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	if err != nil && err != io.EOF {
		b.c.noteNetErr(err)
	}
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }

func (c *Client) noteNetErr(err error) {
	s := netCause(err)
	c.netErr.Store(&s)
}

// netCause names a network failure from the kind of error alone. The text
// of such errors carries the URL, host names and addresses, so none of it is
// used.
func netCause(err error) string {
	var (
		timeout net.Error
		dns     *net.DNSError
		errno   syscall.Errno
	)
	switch {
	case errors.Is(err, errTooLarge):
		return errTooLarge.Error()
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.As(err, &timeout) && timeout.Timeout():
		return "timed out"
	case errors.As(err, &dns):
		return "DNS lookup failed"
	case errors.As(err, &errno):
		return errno.Error()
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed"
	}
	for u := errors.Unwrap(err); u != nil; u = errors.Unwrap(err) {
		err = u
	}
	return fmt.Sprintf("request failed (%T)", err)
}

// Connect opens the MCP session, within StartTimeout. The error is an *Error.
func (c *Client) Connect(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.startTimeout)
	defer cancel()
	c.netErr.Store(nil)
	if _, err := c.connect(ctx); err != nil {
		return c.startError("connect", err)
	}
	return nil
}

// List connects and returns the server's tools as Claude tool definitions
// named mcp__<name>, sorted by name. It fails closed, with an *Error, on a
// name the Claude API would reject, a duplicate, an input schema whose type
// is not "object", and the caps above. Schemas are not checked further.
func (c *Client) List(ctx context.Context) ([]json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, c.startTimeout)
	defer cancel()
	c.netErr.Store(nil)
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
		return nil, &Error{Message: c.sanitize(err.Error())}
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
	c.netErr.Store(nil)
	// A call that has to connect first gets the start limit for that step.
	connCtx, cancelConn := context.WithTimeout(callCtx, c.startTimeout)
	s, err := c.connect(connCtx)
	cancelConn()
	var res *sdk.CallToolResult
	what := "connect: "
	if err == nil {
		what = ""
		res, err = s.CallTool(callCtx, params)
	}
	switch {
	case err == nil:
		r := render(res)
		r.Content = c.redact(r.Content)
		return r
	case ctx.Err() != nil:
		return errResult("[cancelled]")
	case callCtx.Err() != nil:
		return errResult("[MCP call stopped after %s timeout]", c.callTimeout)
	}
	// Only a JSON-RPC error in a successful HTTP response leaves the session
	// usable. After anything else, such as an HTTP error status (which the
	// SDK also reports as a JSON-RPC error when the body holds one, as for a
	// session the server no longer knows), the next call opens a new one.
	var rpcErr *jsonrpc.Error
	if status := c.status.Load(); !errors.As(err, &rpcErr) || status < 200 || status > 299 {
		c.drop(s)
	}
	return errResult("MCP call failed: %s%s", what, c.describe(err))
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
	e := &Error{Message: "MCP " + what + " failed: " + c.describe(err)}
	if s := int(c.status.Load()); s != 0 && (s < 200 || s > 299) {
		e.HTTPStatus = s
	}
	return e
}

// describe is the text for err that goes into events, the session file and
// to the model. A network failure is named by its kind only; other text
// comes from the SDK and the server and is sanitized.
func (c *Client) describe(err error) string {
	if d := c.netErr.Load(); d != nil {
		return *d
	}
	var ue *url.Error
	var oe *net.OpError
	if errors.As(err, &ue) || errors.As(err, &oe) {
		return netCause(err)
	}
	return c.sanitize(err.Error())
}

// sanitize makes text the server can influence fit for an error message:
// the token removed, control characters replaced, and capped.
func (c *Client) sanitize(s string) string {
	if len(s) > 4*maxErrorBytes {
		s = s[:4*maxErrorBytes] // bound the work; cut properly below
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(c.redact(s), "?"))
	if len(s) > maxErrorBytes {
		s = strings.ToValidUTF8(s[:maxErrorBytes], "") + " [...]"
	}
	return s
}

// redact removes the token from text that came from the server.
func (c *Client) redact(s string) string {
	if c.token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.token, "<token>")
}
