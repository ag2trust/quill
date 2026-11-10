// Package provider talks to the Claude Messages API.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"quill/internal/events"
)

const (
	maxTokens      = 32000
	maxRetries     = 2
	requestTimeout = 10 * time.Minute

	// Server-side compaction (beta): once a request's input reaches the
	// trigger, the API summarises the older turns into a compaction block at
	// the start of the reply. 150,000 input tokens is the API default.
	compactBeta    = "compact-2026-01-12"
	compactEdit    = "compact_20260112"
	compactTrigger = 150000

	// compactInstructions replaces the API's default summarisation prompt.
	// With tools in the request the default can end in a tool call and no
	// summary; this one says not to call tools.
	compactInstructions = "Summarize the transcript above inside <summary></summary> tags so that the coding task " +
		"can continue in a new context window where the transcript is no longer available. Include the goal, " +
		"the decisions made and why, the files changed, the current state, and the next steps. " +
		"Do not call any tools while writing this summary; respond with text only."
)

// Request is one Messages API call. Tools and Messages are stored raw JSON and
// are sent byte for byte so the prompt cache prefix stays identical.
type Request struct {
	Model, Effort, System string
	Tools                 json.RawMessage
	Messages              []json.RawMessage
}

// ToolUse is one tool_use block of a response.
type ToolUse struct {
	ID, Name string
	Input    json.RawMessage
}

// Response is the model's reply. Message is {"role":"assistant","content":...}
// with content exactly as received; Text joins the text blocks with "\n".
type Response struct {
	Message    json.RawMessage
	StopReason string
	Text       string
	ToolUses   []ToolUse
	Usage      events.Usage
}

// Client takes one model step.
type Client interface {
	Step(ctx context.Context, req Request) (Response, error)
}

// Claude is a Client backed by the official SDK.
type Claude struct {
	client anthropic.Client
	// CompactTrigger is the input-token count at which the API compacts.
	// NewClaude sets the fixed default; only tests change it.
	CompactTrigger int64
}

var _ Client = (*Claude)(nil)

// NewClaude builds a client with an explicit API key. The base URL comes from
// ANTHROPIC_BASE_URL when set; no other environment variable affects it.
// opts are applied last and override the defaults.
func NewClaude(apiKey string, opts ...option.RequestOption) *Claude {
	base := []option.RequestOption{
		// Skip the SDK's env autoload (ANTHROPIC_AUTH_TOKEN, profiles,
		// ANTHROPIC_CUSTOM_HEADERS, ...): the key and base URL are explicit.
		option.WithoutEnvironmentDefaults(),
		option.WithAPIKey(apiKey),
		option.WithMaxRetries(maxRetries),
		option.WithRequestTimeout(requestTimeout),
	}
	if u, ok := os.LookupEnv("ANTHROPIC_BASE_URL"); ok && u != "" {
		base = append(base, option.WithBaseURL(u))
	}
	return &Claude{client: anthropic.NewClient(append(base, opts...)...), CompactTrigger: compactTrigger}
}

// ErrBadResponse marks a failure that is neither an API error nor a network
// error: a request body that could not be built, or a 2xx response that could
// not be decoded. Classify returns nil for it; the caller reports internal_error.
var ErrBadResponse = errors.New("provider: bad response")

// maxRetryWait is the longest retry-after the SDK may sleep for inside a run.
// Longer waits end the step with the API error so the caller can decide.
const maxRetryWait = 60 * time.Second

// stepError carries how many retries Step made before failing.
type stepError struct {
	err     error
	retries int
}

func (e *stepError) Error() string { return e.err.Error() }
func (e *stepError) Unwrap() error { return e.err }

// Step sends one request and returns the parsed reply. Errors wrap what the SDK
// produced (errors.As/Is still see it), except a per-attempt timeout, which is
// reported as a plain error (not context.DeadlineExceeded) so Classify treats it
// as network. Use Classify to turn an error into an *Error.
func (c *Claude) Step(ctx context.Context, req Request) (Response, error) {
	params := anthropic.MessageNewParams{
		Model:        anthropic.Model(req.Model),
		MaxTokens:    maxTokens,
		CacheControl: anthropic.NewCacheControlEphemeralParam(),
	}
	body, err := requestBody(params, req, c.CompactTrigger)
	if err != nil {
		return Response{}, fmt.Errorf("%w: build request: %v", ErrBadResponse, err)
	}
	attempts := 0
	var raw []byte
	_, err = c.client.Messages.New(ctx, params,
		option.WithRequestBody("application/json", body),
		option.WithHeaderAdd("anthropic-beta", compactBeta),
		option.WithResponseBodyInto(&raw),
		option.WithMiddleware(func(r *http.Request, next option.MiddlewareNext) (*http.Response, error) {
			attempts++
			res, err := next(r)
			if res != nil && retryWait(res.Header) > maxRetryWait {
				res.Header.Set("x-should-retry", "false")
			}
			return res, err
		}),
	)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		} else if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("provider: request timed out: %v", err)
		}
		return Response{}, &stepError{err: err, retries: max(0, attempts-1)}
	}
	return parseResponse(raw)
}

// retryWait is the wait the response asks for, read the way the SDK does:
// retry-after-ms first, then retry-after as seconds or an HTTP date.
func retryWait(h http.Header) time.Duration {
	if v := h.Get("retry-after-ms"); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	v := h.Get("retry-after")
	if v == "" {
		return 0
	}
	if s, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(s * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

// requestBody serializes params and splices in the raw fields. The SDK's
// WithJSONSet would run them through encoding/json, which compacts them and
// escapes <, >, & — so they are appended here verbatim instead.
func requestBody(params anthropic.MessageNewParams, req Request, trigger int64) ([]byte, error) {
	b, err := params.MarshalJSON()
	if err != nil {
		return nil, err
	}
	b = bytes.TrimSpace(b)
	if len(b) < 2 || b[len(b)-1] != '}' {
		return nil, fmt.Errorf("provider: unexpected params JSON %q", b)
	}
	var buf bytes.Buffer
	buf.Write(b[:len(b)-1])
	field := func(name string, v []byte) {
		buf.WriteString(`,"` + name + `":`)
		buf.Write(v)
	}
	if req.System != "" {
		sys, err := json.Marshal([]map[string]string{{"type": "text", "text": req.System}})
		if err != nil {
			return nil, err
		}
		field("system", sys)
	}
	if len(req.Tools) > 0 {
		field("tools", req.Tools)
	}
	buf.WriteString(`,"messages":[`)
	for i, m := range req.Messages {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(m)
	}
	buf.WriteByte(']')
	if req.Effort != "" {
		oc, err := json.Marshal(map[string]string{"effort": req.Effort})
		if err != nil {
			return nil, err
		}
		field("output_config", oc)
	}
	field("context_management", fmt.Appendf(nil,
		`{"edits":[{"type":%q,"trigger":{"type":"input_tokens","value":%d},"instructions":%q}]}`,
		compactEdit, trigger, compactInstructions))
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func parseResponse(raw []byte) (Response, error) {
	type usage struct {
		Input      int64 `json:"input_tokens"`
		Output     int64 `json:"output_tokens"`
		CacheRead  int64 `json:"cache_read_input_tokens"`
		CacheWrite int64 `json:"cache_creation_input_tokens"`
	}
	var msg struct {
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason"`
		Usage      struct {
			usage
			Iterations []struct {
				Type string `json:"type"`
				usage
			} `json:"iterations"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return Response{}, fmt.Errorf("%w: decode response: %v", ErrBadResponse, err)
	}
	if len(msg.Content) == 0 || string(msg.Content) == "null" {
		msg.Content = json.RawMessage(`[]`)
	}
	var blocks []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return Response{}, fmt.Errorf("%w: decode content: %v", ErrBadResponse, err)
	}

	resp := Response{
		StopReason: msg.StopReason,
		Usage: events.Usage{
			Input:      msg.Usage.Input,
			Output:     msg.Usage.Output,
			CacheRead:  msg.Usage.CacheRead,
			CacheWrite: msg.Usage.CacheWrite,
		},
	}
	// The top-level counts leave out the compaction call; it is billed, so
	// add its iteration back in.
	for _, it := range msg.Usage.Iterations {
		if it.Type == "compaction" {
			resp.Usage.Add(events.Usage{Input: it.Input, Output: it.Output, CacheRead: it.CacheRead, CacheWrite: it.CacheWrite})
		}
	}
	var texts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "tool_use":
			resp.ToolUses = append(resp.ToolUses, ToolUse{ID: b.ID, Name: b.Name, Input: b.Input})
		}
	}
	resp.Text = strings.Join(texts, "\n")

	m := make([]byte, 0, len(msg.Content)+32)
	m = append(m, `{"role":"assistant","content":`...)
	m = append(m, msg.Content...)
	m = append(m, '}')
	resp.Message = m
	return resp, nil
}
