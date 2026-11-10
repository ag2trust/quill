// Package fakeapi is a scripted stand-in for the Claude Messages API, for tests.
package fakeapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"quill/internal/events"
)

// Reply is one scripted HTTP response. A zero Status means 200.
type Reply struct {
	Status int
	Body   string
	Header http.Header
}

// Server answers each request with the next scripted reply and records request bodies.
type Server struct {
	srv *httptest.Server

	mu       sync.Mutex
	replies  []Reply
	requests [][]byte
	headers  []http.Header
}

const exhaustedBody = `{"type":"error","error":{"type":"api_error","message":"fakeapi: script exhausted"}}`

// New starts a server that plays replies in order; it is closed when t ends.
// Once the script is exhausted every request gets a 500 api_error.
func New(t *testing.T, replies ...Reply) *Server {
	t.Helper()
	s := &Server{replies: replies}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	s.mu.Lock()
	s.requests = append(s.requests, body)
	s.headers = append(s.headers, r.Header.Clone())
	reply := Reply{Status: http.StatusInternalServerError, Body: exhaustedBody}
	if len(s.replies) > 0 {
		reply = s.replies[0]
		s.replies = s.replies[1:]
	}
	s.mu.Unlock()

	for k, vs := range reply.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	status := reply.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, reply.Body)
}

// URL is the server's base URL.
func (s *Server) URL() string { return s.srv.URL }

// Requests returns the request bodies received so far, in order.
func (s *Server) Requests() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.requests))
	copy(out, s.requests)
	return out
}

// Headers returns the request headers received so far, in order.
func (s *Server) Headers() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]http.Header(nil), s.headers...)
}

// TextReply is a successful response with one text block and stop reason end_turn.
func TextReply(text string, usage events.Usage) Reply {
	content, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	return message(content, "end_turn", usage)
}

// ToolReply is a successful response with one tool_use block (input is raw JSON)
// and stop reason tool_use.
func ToolReply(id, name, input string) Reply {
	// Spliced, not marshalled, so input reaches the client byte for byte.
	content := `[{"type":"tool_use","id":` + quote(id) + `,"name":` + quote(name) + `,"input":` + input + `}]`
	return message([]byte(content), "tool_use", events.Usage{})
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func message(content []byte, stopReason string, u events.Usage) Reply {
	usage, _ := json.Marshal(map[string]int64{
		"input_tokens":                u.Input,
		"output_tokens":               u.Output,
		"cache_read_input_tokens":     u.CacheRead,
		"cache_creation_input_tokens": u.CacheWrite,
	})
	body := `{"id":"msg_fake","type":"message","role":"assistant","model":"claude-fake","content":` +
		string(content) + `,"stop_reason":` + quote(stopReason) + `,"stop_sequence":null,"usage":` + string(usage) + `}`
	return Reply{Status: http.StatusOK, Body: body}
}
