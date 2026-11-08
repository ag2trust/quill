// Package provider talks to the Claude Messages API.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	return &Claude{client: anthropic.NewClient(append(base, opts...)...)}
}

// Step sends one request and returns the parsed reply. Errors are returned as
// the SDK produced them, except a per-attempt timeout, which is reported as a
// plain error (not context.DeadlineExceeded) so Classify treats it as network.
// Use Classify to turn an error into an *Error.
func (c *Claude) Step(ctx context.Context, req Request) (Response, error) {
	params := anthropic.MessageNewParams{
		Model:        anthropic.Model(req.Model),
		MaxTokens:    maxTokens,
		CacheControl: anthropic.NewCacheControlEphemeralParam(),
	}
	body, err := requestBody(params, req)
	if err != nil {
		return Response{}, err
	}
	var raw []byte
	_, err = c.client.Messages.New(ctx, params,
		option.WithRequestBody("application/json", body),
		option.WithResponseBodyInto(&raw),
	)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Response{}, ctxErr
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return Response{}, fmt.Errorf("provider: request timed out: %v", err)
		}
		return Response{}, err
	}
	return parseResponse(raw)
}

// requestBody serializes params and splices in the raw fields. The SDK's
// WithJSONSet would run them through encoding/json, which compacts them and
// escapes <, >, & — so they are appended here verbatim instead.
func requestBody(params anthropic.MessageNewParams, req Request) ([]byte, error) {
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
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func parseResponse(raw []byte) (Response, error) {
	var msg struct {
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason"`
		Usage      struct {
			Input      int64 `json:"input_tokens"`
			Output     int64 `json:"output_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return Response{}, fmt.Errorf("provider: decode response: %w", err)
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
		return Response{}, fmt.Errorf("provider: decode content: %w", err)
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
