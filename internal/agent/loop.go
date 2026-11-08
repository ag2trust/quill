// Package agent runs the model-tool loop for one run.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"

	"quill/internal/events"
	"quill/internal/provider"
	"quill/internal/session"
	"quill/internal/tools"
)

const continueText = "Your previous response hit the output limit. Continue, using smaller steps."

// Options are the fixed settings of a run.
type Options struct {
	Model, Effort, System string
	Tools                 json.RawMessage
	MaxSteps              int
	MaxTokens             int64
}

// Loop drives one run. History holds the session so far and grows as the run
// goes; every message added to it is also appended to Store, in order.
type Loop struct {
	Client   provider.Client
	Registry *tools.Registry
	Store    session.Store
	Events   *events.Writer
	Opts     Options
	History  []json.RawMessage
}

type toolResult struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error"`
}

type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type userMessage[T any] struct {
	Role    string `json:"role"`
	Content []T    `json:"content"`
}

// Run appends next to the history and steps until the run ends. ctx is
// cancelled by signals, limitCtx by the run timeout. It never writes
// run.completed; DurationMS is left for the caller.
func (l *Loop) Run(ctx, limitCtx context.Context, next ...json.RawMessage) events.RunCompleted {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(limitCtx, cancel)
	defer stop()

	var res events.RunCompleted
	finish := func(s events.Status) events.RunCompleted {
		res.Status = s
		return res
	}
	internal := func(err error) events.RunCompleted {
		res.Error = &events.ErrorInfo{Kind: "internal", Message: err.Error()}
		return finish(events.StatusInternalError)
	}
	// interrupted reports why runCtx ended, or ok=false while it is live.
	interrupted := func() (events.Status, bool) {
		switch {
		case ctx.Err() != nil:
			return events.StatusCancelled, true
		case limitCtx.Err() != nil:
			return events.StatusLimitReached, true
		}
		return "", false
	}

	for _, m := range next {
		if err := l.add(m); err != nil {
			return internal(err)
		}
	}

	for {
		if res.Steps >= l.Opts.MaxSteps {
			return finish(events.StatusLimitReached)
		}
		resp, err := l.Client.Step(runCtx, provider.Request{
			Model: l.Opts.Model, Effort: l.Opts.Effort, System: l.Opts.System,
			Tools: l.Opts.Tools, Messages: slices.Clip(l.History),
		})
		if err != nil {
			if s, ok := interrupted(); ok {
				return finish(s)
			}
			if isBadResponse(err) {
				return internal(err)
			}
			perr := provider.Classify(err)
			if perr == nil {
				perr = &provider.Error{Info: events.ErrorInfo{Kind: "network", Message: err.Error()}}
			}
			res.Error = &perr.Info
			return finish(events.StatusProviderError)
		}

		res.Steps++
		res.Usage.Add(resp.Usage)
		if err := l.add(resp.Message); err != nil {
			return internal(err)
		}
		if resp.Text != "" {
			res.FinalText = resp.Text
			l.Events.ItemCompleted(events.Item{Type: "message", Text: resp.Text})
		}

		if resp.StopReason == "refusal" {
			res.Error = &provider.Refused().Info
			return finish(events.StatusProviderError)
		}

		var reply json.RawMessage
		if len(resp.ToolUses) == 0 {
			if resp.StopReason != "max_tokens" {
				return finish(events.StatusCompleted)
			}
			reply, err = marshal(userMessage[textBlock]{"user", []textBlock{{"text", continueText}}})
			if err != nil {
				return internal(err)
			}
		} else {
			for _, tu := range resp.ToolUses {
				if tu.Name != tools.BlockerTool {
					continue
				}
				if b, err := tools.DecodeBlocker(tu.Input); err == nil {
					res.Blocker = &events.Blocker{Reason: b.Reason, Summary: b.Summary, Details: b.Details, Question: b.Question}
					return finish(events.StatusBlocked)
				}
			}

			results := make([]toolResult, 0, len(resp.ToolUses))
			for _, tu := range resp.ToolUses {
				if s, ok := interrupted(); ok {
					return finish(s)
				}
				l.Events.ItemStarted(events.Item{Type: "tool_call", ID: tu.ID, Name: tu.Name, Input: tu.Input})
				r := l.runTool(runCtx, tu)
				if s, ok := interrupted(); ok {
					// The result may be cut short by the stop; it is not saved.
					return finish(s)
				}
				l.Events.ItemCompleted(events.Item{Type: "tool_call", ID: tu.ID, Name: tu.Name, Input: tu.Input, Text: r.Content, IsError: r.IsError})
				results = append(results, toolResult{"tool_result", tu.ID, r.Content, r.IsError})
			}
			reply, err = marshal(userMessage[toolResult]{"user", results})
			if err != nil {
				return internal(err)
			}
		}
		if err := l.add(reply); err != nil {
			return internal(err)
		}

		if res.Usage.Billable() >= l.Opts.MaxTokens {
			return finish(events.StatusLimitReached)
		}
	}
}

// runTool runs one tool call; raise_blocker reaches here only with invalid input.
func (l *Loop) runTool(ctx context.Context, tu provider.ToolUse) tools.Result {
	if tu.Name == tools.BlockerTool {
		_, err := tools.DecodeBlocker(tu.Input)
		return tools.Result{Content: err.Error(), IsError: true}
	}
	return l.Registry.Run(ctx, tu.Name, tu.Input)
}

// isBadResponse reports a malformed API response, which is a Quill-side
// problem rather than a provider failure.
// TODO(controller): becomes errors.Is(err, provider.ErrBadResponse) once
// the provider exports that sentinel.
func isBadResponse(err error) bool { return false }

// add appends m to the history and the store.
func (l *Loop) add(m json.RawMessage) error {
	l.History = append(l.History, m)
	return l.Store.Append(m)
}

// marshal encodes v compactly without HTML escaping.
func marshal(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}
