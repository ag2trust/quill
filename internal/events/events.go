// Package events defines the JSON-lines events quill writes to stdout.
package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// Version is the quill version reported in run.started.
const Version = "0.1.0-dev"

// Format is the event interface version reported in run.started. Raise it on
// an incompatible change to the events.
const Format = 2

// ErrCompleted is returned by any write after run.completed.
var ErrCompleted = errors.New("events: run already completed")

// Status is the terminal outcome of a run.
type Status string

const (
	StatusCompleted     Status = "completed"
	StatusBlocked       Status = "blocked"
	StatusLimitReached  Status = "limit_reached"
	StatusProviderError Status = "provider_error"
	StatusCancelled     Status = "cancelled"
	StatusInternalError Status = "internal_error"
)

// ExitCode maps a status to the process exit code.
func (s Status) ExitCode() int {
	switch s {
	case StatusCompleted:
		return 0
	case StatusBlocked:
		return 5
	case StatusLimitReached:
		return 3
	case StatusProviderError:
		return 4
	case StatusCancelled:
		return 143
	default:
		return 1
	}
}

type Usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

func (u *Usage) Add(o Usage) {
	u.Input += o.Input
	u.Output += o.Output
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
}

// Billable is the count charged against the token budget (cache reads excluded).
func (u Usage) Billable() int64 { return u.Input + u.Output + u.CacheWrite }

type ErrorInfo struct {
	Kind       string `json:"kind"`
	Type       string `json:"type"`
	Message    string `json:"message"`
	RequestID  string `json:"request_id"`
	HTTPStatus int    `json:"http_status"`
	RetryAfter string `json:"retry_after,omitempty"`
	Retries    int    `json:"retries"`
}

type Blocker struct {
	Reason   string `json:"reason"`
	Summary  string `json:"summary"`
	Details  string `json:"details"`
	Question string `json:"question"`
}

// Item is a message or tool call. Type is "message" or "tool_call".
type Item struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Text    string          `json:"text,omitempty"`
	Name    string          `json:"name,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
}

type RunStarted struct {
	Format    int      `json:"format"`
	Session   string   `json:"session,omitempty"`
	Model     string   `json:"model"`
	Effort    string   `json:"effort"`
	Profile   string   `json:"tool_profile"`
	Workspace string   `json:"workspace"`
	Version   string   `json:"version"`
	EnvPassed []string `json:"env_passed"`
}

type RunCompleted struct {
	Status     Status     `json:"status"`
	FinalText  string     `json:"final_text"`
	Usage      Usage      `json:"usage"`
	Steps      int        `json:"steps"`
	DurationMS int64      `json:"duration_ms"`
	Error      *ErrorInfo `json:"error,omitempty"`
	Blocker    *Blocker   `json:"blocker,omitempty"`
}

// Writer writes one JSON object per line. It is safe for concurrent use.
type Writer struct {
	mu   sync.Mutex
	w    io.Writer
	done bool
}

func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// The wrappers below put "type" first by embedding the payload.
type startedLine struct {
	Type string `json:"type"`
	RunStarted
}

type completedLine struct {
	Type string `json:"type"`
	RunCompleted
}

type itemLine struct {
	Type string `json:"type"`
	Item Item   `json:"item"`
}

func (w *Writer) RunStarted(e RunStarted) error {
	return w.write(startedLine{"run.started", e}, false)
}

func (w *Writer) ItemStarted(i Item) error {
	return w.write(itemLine{"item.started", i}, false)
}

func (w *Writer) ItemCompleted(i Item) error {
	return w.write(itemLine{"item.completed", i}, false)
}

func (w *Writer) RunCompleted(e RunCompleted) error {
	return w.write(completedLine{"run.completed", e}, true)
}

func (w *Writer) write(v any, final bool) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return ErrCompleted
	}
	if final {
		w.done = true
	}
	_, err := w.w.Write(buf.Bytes())
	return err
}
