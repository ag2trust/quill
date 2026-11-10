// Package fakemcp is an in-process MCP server over Streamable HTTP, for tests.
package fakemcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool is one tool the server offers. An empty Schema means {"type":"object"}.
// A nil Call echoes the arguments back as text.
type Tool struct {
	Name, Description, Schema string
	Call                      func(ctx context.Context, args json.RawMessage) (*sdk.CallToolResult, error)
}

// Server is a running fake MCP server.
type Server struct {
	srv    *httptest.Server
	status atomic.Int32

	mu   sync.Mutex
	auth []string
}

// New starts a server offering tools; it is closed when t ends.
func New(t *testing.T, tools ...Tool) *Server {
	t.Helper()
	mcpServer := sdk.NewServer(&sdk.Implementation{Name: "fakemcp", Version: "0"}, nil)
	for _, tool := range tools {
		schema := tool.Schema
		if schema == "" {
			schema = `{"type":"object"}`
		}
		call := tool.Call
		if call == nil {
			call = func(_ context.Context, args json.RawMessage) (*sdk.CallToolResult, error) {
				return Text(string(args)), nil
			}
		}
		mcpServer.AddTool(
			&sdk.Tool{Name: tool.Name, Description: tool.Description, InputSchema: json.RawMessage(schema)},
			func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				return call(ctx, req.Params.Arguments)
			})
	}
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return mcpServer }, nil)
	s := &Server{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		s.mu.Unlock()
		if code := int(s.status.Load()); code != 0 {
			http.Error(w, "fakemcp: forced status", code)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the server's MCP endpoint.
func (s *Server) URL() string { return s.srv.URL }

// Close stops the server, so later connections are refused.
func (s *Server) Close() { s.srv.Close() }

// SetStatus makes every later request fail with HTTP status code; 0 restores
// normal service.
func (s *Server) SetStatus(code int) { s.status.Store(int32(code)) }

// Auth returns the Authorization header of every request so far.
func (s *Server) Auth() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auth...)
}

// Text is a successful result with one text block.
func Text(text string) *sdk.CallToolResult {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}}
}
