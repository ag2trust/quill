package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Profile selects which tools are exposed.
type Profile string

const (
	ReadOnly       Profile = "read-only"
	WorkspaceWrite Profile = "workspace-write"
)

// BlockerTool is handled by the run loop, never by the registry.
const BlockerTool = "raise_blocker"

// Result is the outcome of a tool call.
type Result struct {
	Content string
	IsError bool
}

// Shell runs shell commands (implemented elsewhere).
type Shell interface {
	Run(ctx context.Context, command string) Result
}

// Registry dispatches tool calls for one profile and workspace.
type Registry struct {
	workspace string
	shell     Shell
	defs      json.RawMessage
	allowed   map[string]bool
}

type schema struct {
	Type                 string         `json:"type"`
	Properties           map[string]any `json:"properties"` // map keys marshal sorted
	Required             []string       `json:"required"`
	AdditionalProperties bool           `json:"additionalProperties"`
}

type definition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema schema `json:"input_schema"`
}

func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

func obj(required []string, props map[string]any) schema {
	return schema{Type: "object", Properties: props, Required: required}
}

var (
	defRead = definition{"read", "Read a text file from the workspace; with offset (1-based line) and limit (lines) return only those lines.",
		obj([]string{"path"}, map[string]any{
			"path":   prop("string", "File path, relative to the workspace."),
			"offset": prop("integer", "1-based line to start from."),
			"limit":  prop("integer", "Maximum number of lines to return."),
		})}
	defWrite = definition{"write", "Write content to a file in the workspace, creating parent directories and overwriting any existing file.",
		obj([]string{"path", "content"}, map[string]any{
			"path":    prop("string", "File path, relative to the workspace."),
			"content": prop("string", "Full file content."),
		})}
	defEdit = definition{"edit", "Replace old_text with new_text in a file; old_text must match exactly one place unless replace_all is set.",
		obj([]string{"path", "old_text", "new_text"}, map[string]any{
			"path":        prop("string", "File path, relative to the workspace."),
			"old_text":    prop("string", "Exact text to replace; must not be empty."),
			"new_text":    prop("string", "Replacement text."),
			"replace_all": prop("boolean", "Replace every occurrence."),
		})}
	defList = definition{"list", "List the entries of a directory (non-recursive, sorted, directories end with /).",
		obj([]string{}, map[string]any{
			"path": prop("string", "Directory path, relative to the workspace; defaults to '.'."),
		})}
	defGlob = definition{"glob", "Find files by path pattern, such as **/*_test.go or src/**/*.{ts,tsx}. A pattern without a slash matches file names at any depth. Skips .git, symlinks, and paths ignored by .gitignore. Returns workspace-relative paths.",
		obj([]string{"pattern"}, map[string]any{
			"pattern": prop("string", "Glob matched against paths relative to path: * ? [...] within a segment, ** across segments, {a,b} alternatives."),
			"path":    prop("string", "Directory to search, relative to the workspace; defaults to '.'."),
		})}
	defSearch = definition{"search", "Search file contents under a path for lines matching a Go regular expression. Skips .git, symlinks, binary files, and paths ignored by .gitignore.",
		obj([]string{"pattern"}, map[string]any{
			"pattern": prop("string", "Go regular expression; use (?i) for case-insensitive."),
			"path":    prop("string", "File or directory to search, relative to the workspace; defaults to '.'."),
			"glob":    prop("string", "Only search files matching this glob, such as *.go or src/**/*.{ts,tsx}."),
			"output_mode": map[string]any{"type": "string", "enum": []string{"content", "files"},
				"description": "content (default) returns path:line: text for each match; files returns each matching file once."},
		})}
	defShell = definition{"shell", "Run a shell command in the workspace and return its combined output and exit status.",
		obj([]string{"command"}, map[string]any{
			"command": prop("string", "Command to run."),
		})}
	defBlocker = definition{BlockerTool, "Stop the run and report a problem that needs a decision from whoever runs you. Use it instead of guessing when the task conflicts with repository guidance, is unclear, lacks access, or looks unsafe. Do not call other tools in the same response.",
		obj([]string{"reason", "summary", "details", "question"}, map[string]any{
			"reason":   map[string]any{"type": "string", "description": "Kind of blocker.", "enum": blockerReasons},
			"summary":  prop("string", "One-line summary of the problem."),
			"details":  prop("string", "What you found and why it blocks you."),
			"question": prop("string", "The decision you need."),
		})}
)

var blockerReasons = []string{"conflict", "unclear_task", "missing_access", "unsafe"}

// NewRegistry builds a registry. shell may be nil (then the shell tool errors).
func NewRegistry(profile Profile, workspace string, shell Shell) *Registry {
	list := []definition{defRead, defList, defGlob, defSearch, defBlocker}
	if profile == WorkspaceWrite {
		list = []definition{defRead, defWrite, defEdit, defList, defGlob, defSearch, defShell, defBlocker}
	}
	allowed := map[string]bool{}
	for _, d := range list {
		allowed[d.Name] = true
	}
	defs, err := json.Marshal(list)
	if err != nil {
		panic(err) // static literals; cannot fail
	}
	return &Registry{workspace: workspace, shell: shell, defs: defs, allowed: allowed}
}

// Definitions returns the tool definitions as a JSON array, byte-identical across calls.
func (r *Registry) Definitions() json.RawMessage { return append(json.RawMessage(nil), r.defs...) }

// Run executes a tool call. All content is capped at OutputCap.
func (r *Registry) Run(ctx context.Context, name string, input json.RawMessage) Result {
	res := r.dispatch(ctx, name, input)
	res.Content = Truncate(res.Content, OutputCap)
	return res
}

func errResult(format string, a ...any) Result {
	return Result{Content: fmt.Sprintf(format, a...), IsError: true}
}

func (r *Registry) dispatch(ctx context.Context, name string, input json.RawMessage) Result {
	if !r.allowed[name] {
		return errResult("unknown tool: %s", name)
	}
	switch name {
	case BlockerTool:
		return errResult("raise_blocker is handled by the run loop")
	case "read":
		return r.read(input)
	case "write":
		return r.write(input)
	case "edit":
		return r.edit(input)
	case "list":
		return r.list(input)
	case "glob":
		return r.glob(ctx, input)
	case "search":
		return r.search(ctx, input)
	case "shell":
		var in struct {
			Command *string `json:"command"`
		}
		if err := decode(input, &in); err != nil {
			return errResult("invalid input: %v", err)
		}
		if in.Command == nil {
			return errResult("invalid input: command is required")
		}
		if r.shell == nil {
			return errResult("shell is not available")
		}
		return r.shell.Run(ctx, *in.Command)
	}
	return errResult("unknown tool: %s", name)
}

// decode strictly decodes tool input, rejecting unknown fields and trailing data.
func decode(input json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("unexpected trailing data")
	}
	return nil
}

// Blocker is the decoded input of raise_blocker.
type Blocker struct {
	Reason, Summary, Details, Question string
}

// DecodeBlocker validates and decodes raise_blocker input.
func DecodeBlocker(input json.RawMessage) (Blocker, error) {
	var in struct {
		Reason   *string `json:"reason"`
		Summary  *string `json:"summary"`
		Details  *string `json:"details"`
		Question *string `json:"question"`
	}
	if err := decode(input, &in); err != nil {
		return Blocker{}, err
	}
	if in.Reason == nil || in.Summary == nil || in.Details == nil || in.Question == nil {
		return Blocker{}, errors.New("reason, summary, details and question are required")
	}
	ok := false
	for _, r := range blockerReasons {
		ok = ok || r == *in.Reason
	}
	if !ok {
		return Blocker{}, fmt.Errorf("invalid reason: %q", *in.Reason)
	}
	return Blocker{*in.Reason, *in.Summary, *in.Details, *in.Question}, nil
}
