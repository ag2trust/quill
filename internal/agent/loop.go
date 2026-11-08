// Package agent runs the model-tool loop for one run.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"quill/internal/events"
	"quill/internal/provider"
	"quill/internal/session"
	"quill/internal/tools"
)

const (
	continueText = "Your previous response hit the output limit. Continue, using smaller steps."
	cutOffText   = "Your response hit the output limit and this tool call was cut off, so it was not run. Retry using smaller steps."
	notRunText   = "not run: cancelled"
	partialText  = "interrupted: it may have partly run"
)

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
	// Our appends must never write into a caller's backing array.
	l.History = slices.Clip(l.History)

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
		if s, ok := interrupted(); ok {
			return finish(s)
		}
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
			if errors.Is(err, provider.ErrBadResponse) {
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
			l.emitCompleted(events.Item{Type: "message", Text: resp.Text})
		}

		if resp.StopReason == "refusal" {
			res.Error = &provider.Refused().Info
			return finish(events.StatusProviderError)
		}

		var reply json.RawMessage
		var stopped events.Status // set when the run is stopped mid-tools
		switch {
		case len(resp.ToolUses) == 0:
			switch resp.StopReason {
			case "end_turn", "stop_sequence":
				return finish(events.StatusCompleted)
			case "model_context_window_exceeded":
				return finish(events.StatusLimitReached)
			case "max_tokens":
				reply, err = marshal(userMessage[textBlock]{"user", []textBlock{{"text", continueText}}})
			default:
				return internal(fmt.Errorf("unexpected stop reason: %s", resp.StopReason))
			}

		case resp.StopReason == "max_tokens":
			// The last tool call's input may be cut off: run none of them.
			results := make([]toolResult, 0, len(resp.ToolUses))
			for _, tu := range resp.ToolUses {
				l.emitStarted(tu)
				l.emitCompleted(toolItem(tu, cutOffText, true))
				results = append(results, toolResult{"tool_result", tu.ID, cutOffText, true})
			}
			reply, err = marshal(userMessage[toolResult]{"user", results})

		default:
			for _, tu := range resp.ToolUses {
				if tu.Name != tools.BlockerTool {
					continue
				}
				if b, err := tools.DecodeBlocker(tu.Input); err == nil {
					res.Blocker = &events.Blocker{Reason: b.Reason, Summary: b.Summary, Details: b.Details, Question: b.Question}
					return finish(events.StatusBlocked)
				}
			}
			var results []toolResult
			results, stopped = l.runTools(runCtx, resp.ToolUses, interrupted)
			reply, err = marshal(userMessage[toolResult]{"user", results})
		}
		if err != nil {
			return internal(err)
		}
		if err := l.add(reply); err != nil {
			return internal(err)
		}
		if stopped != "" {
			// Saved above: tools that ran had real effects.
			return finish(stopped)
		}

		if res.Usage.Billable() >= l.Opts.MaxTokens {
			return finish(events.StatusLimitReached)
		}
	}
}

// runTools runs the calls sequentially, in order. If the run is stopped, the
// call in progress gets partialText, the rest get notRunText, and stopped is
// the status.
func (l *Loop) runTools(ctx context.Context, uses []provider.ToolUse, interrupted func() (events.Status, bool)) (results []toolResult, stopped events.Status) {
	results = make([]toolResult, 0, len(uses))
	for _, tu := range uses {
		if stopped == "" {
			stopped, _ = interrupted()
		}
		if stopped != "" {
			results = append(results, toolResult{"tool_result", tu.ID, notRunText, true})
			continue
		}
		l.emitStarted(tu)
		r := l.runTool(ctx, tu)
		if stopped, _ = interrupted(); stopped != "" {
			// The call started and may have had effects, but the stop may
			// have cut its result short, so the result is not reported.
			l.emitCompleted(toolItem(tu, partialText, true))
			results = append(results, toolResult{"tool_result", tu.ID, partialText, true})
			continue
		}
		l.emitCompleted(toolItem(tu, r.Content, r.IsError))
		results = append(results, toolResult{"tool_result", tu.ID, r.Content, r.IsError})
	}
	return results, stopped
}

// runTool runs one tool call; raise_blocker reaches here only with invalid input.
func (l *Loop) runTool(ctx context.Context, tu provider.ToolUse) tools.Result {
	if tu.Name == tools.BlockerTool {
		_, err := tools.DecodeBlocker(tu.Input)
		return tools.Result{Content: err.Error(), IsError: true}
	}
	return l.Registry.Run(ctx, tu.Name, tu.Input)
}

func toolItem(tu provider.ToolUse, text string, isError bool) events.Item {
	return events.Item{Type: "tool_call", ID: tu.ID, Name: tu.Name, Input: tu.Input, Text: text, IsError: isError}
}

// Event write errors are deliberately ignored: the caller owns stdout and
// reports the run in run.completed.
func (l *Loop) emitStarted(tu provider.ToolUse) {
	_ = l.Events.ItemStarted(events.Item{Type: "tool_call", ID: tu.ID, Name: tu.Name, Input: tu.Input})
}

func (l *Loop) emitCompleted(it events.Item) { _ = l.Events.ItemCompleted(it) }

// add appends m to the store and, once it is saved, to the history.
func (l *Loop) add(m json.RawMessage) error {
	if err := l.Store.Append(m); err != nil {
		return err
	}
	l.History = append(l.History, m)
	return nil
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
