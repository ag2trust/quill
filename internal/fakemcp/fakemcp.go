// Package fakemcp is an in-process MCP server over Streamable HTTP, for tests.
package fakemcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
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
	srv        *httptest.Server
	newHandler func() http.Handler

	mu       sync.Mutex
	handler  http.Handler
	auth     []string
	status   int    // non-zero: answer every request with this status
	body     string // and this body
	hang     chan struct{}
	sessions map[string]bool // session IDs seen since the last Restart
	stale    map[string]bool // session IDs from before it
}

// StaleBody is what a restarted server answers, with status 404, to a request
// for a session from before the restart.
const StaleBody = `{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"Session not found"}}`

// New starts a server offering tools, answering calls with event streams; it
// is closed when t ends.
func New(t *testing.T, tools ...Tool) *Server {
	t.Helper()
	return start(t, false, tools)
}

// NewJSON is New with plain JSON responses instead of event streams.
func NewJSON(t *testing.T, tools ...Tool) *Server {
	t.Helper()
	return start(t, true, tools)
}

func start(t *testing.T, jsonResponse bool, tools []Tool) *Server {
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
	s := &Server{sessions: map[string]bool{}, stale: map[string]bool{}}
	s.newHandler = func() http.Handler {
		return sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return mcpServer },
			&sdk.StreamableHTTPOptions{JSONResponse: jsonResponse})
	}
	s.handler = s.newHandler()
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("Mcp-Session-Id")
		s.mu.Lock()
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		stale := s.stale[id]
		if id != "" && !stale {
			s.sessions[id] = true
		}
		handler, status, body, hang := s.handler, s.status, s.body, s.hang
		s.mu.Unlock()
		if hang != nil {
			select {
			case <-hang:
			case <-r.Context().Done():
				return
			}
		}
		if stale {
			status, body = http.StatusNotFound, StaleBody
		}
		if status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
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
func (s *Server) SetStatus(code int) { s.SetFailure(code, "fakemcp: forced status") }

// SetFailure is SetStatus with the response body to send.
func (s *Server) SetFailure(code int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.body = code, body
}

// Restart forgets every session, as a restarted server does: requests for an
// old session get status 404 with StaleBody, and new sessions work.
func (s *Server) Restart() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.sessions {
		s.stale[id] = true
	}
	s.sessions = map[string]bool{}
	s.handler = s.newHandler()
}

// Hang makes every later request wait, unanswered, until release is called.
func (s *Server) Hang() (release func()) {
	ch := make(chan struct{})
	s.mu.Lock()
	s.hang = ch
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		if s.hang == ch {
			s.hang = nil
		}
		s.mu.Unlock()
		close(ch)
	}
}

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
