// Command quill is a headless coding-agent harness: quill exec runs one task
// and reports JSON lines on stdout.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"quill/internal/agent"
	"quill/internal/cli"
	"quill/internal/events"
	"quill/internal/mcp"
	"quill/internal/proc"
	"quill/internal/prompt"
	"quill/internal/provider"
	"quill/internal/session"
	"quill/internal/tools"
)

const (
	shellTimeout = 10 * time.Minute
	killGrace    = 5 * time.Second
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}

// run executes one quill invocation and returns the process exit code. Bad
// usage reports on stderr only (exit 2); every other outcome ends with
// exactly one run.completed on stdout. stderr never carries prompts, file
// contents, or secrets.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) (code int) {
	start := time.Now()
	// Catch SIGPIPE so a write to a closed stdout or stderr returns EPIPE
	// instead of killing quill before cleanup. Notify, not Ignore: an ignored
	// signal stays ignored across exec, which would change how tool
	// pipelines behave; a caught one is reset to default in children.
	sigpipe := make(chan os.Signal, 1)
	signal.Notify(sigpipe, syscall.SIGPIPE)
	defer signal.Stop(sigpipe)
	cfg, err := cli.Parse(args, stdin, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "quill: %v\n", err)
		return 2
	}
	usage := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "quill: "+format+"\n", a...)
		return 2
	}

	ew := events.NewWriter(stdout)
	// Signals stay caught until run returns, so a second SIGTERM during
	// cleanup cannot kill quill before run.completed.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stopSignals()
	ctx, cancelRun := context.WithCancel(sigCtx)
	limitCtx, cancelLimit := context.WithTimeout(context.Background(), cfg.Timeout)

	// Settings: from the flags, or from the session header when continuing.
	model, effort, profile := cfg.Model, cfg.Effort, cfg.Profile
	var (
		sess    *session.File
		runner  *proc.Runner
		tmpDir  string
		done    bool
		started bool
		remote  *mcp.Client // the caller's MCP server, or nil
	)
	envPassed := []string{} // names actually passed to tools, set with the env
	mcpTools := []string{}  // names of the session's MCP tools
	startRun := func() {
		started = true
		_ = ew.RunStarted(events.RunStarted{
			Format:  events.Format,
			Session: cfg.SessionPath, Model: model, Effort: effort, Profile: profile,
			Workspace: cfg.Workspace, Version: events.Version, EnvPassed: envPassed, MCPTools: mcpTools,
		})
	}
	cleanup := func() {
		if done {
			return
		}
		done = true
		cancelRun()
		cancelLimit()
		if runner != nil {
			runner.KillAll(killGrace)
		}
		if remote != nil {
			remote.Close()
		}
		if tmpDir != "" {
			os.RemoveAll(tmpDir)
		}
		if sess != nil {
			sess.Close()
		}
	}
	finish := func(res events.RunCompleted) int {
		cleanup()
		res.DurationMS = time.Since(start).Milliseconds()
		_ = ew.RunCompleted(res)
		return res.Status.ExitCode()
	}
	internal := func(what string, err error) int {
		fmt.Fprintf(stderr, "quill: internal error: %s\n", what)
		if !started {
			startRun()
		}
		return finish(events.RunCompleted{
			Status: events.StatusInternalError,
			Error:  &events.ErrorInfo{Kind: "internal", Message: what + ": " + err.Error()},
		})
	}
	defer cleanup()
	defer func() {
		if r := recover(); r != nil {
			// Only the panic value's type: its text could carry file contents.
			fmt.Fprintf(stderr, "quill: internal error: panic (%T)\n", r)
			if !started {
				startRun()
			}
			code = finish(events.RunCompleted{
				Status: events.StatusInternalError,
				Error:  &events.ErrorInfo{Kind: "internal", Message: "panic"},
			})
		}
	}()

	// The MCP server is contacted before run.started, bounded by the signals
	// and the run timeout. A failure ends the run with status mcp_error.
	startCtx, cancelStart := context.WithCancel(ctx)
	defer cancelStart()
	defer context.AfterFunc(limitCtx, cancelStart)()
	mcpFailed := func(err error) int {
		if !started {
			startRun()
		}
		switch {
		case ctx.Err() != nil:
			return finish(events.RunCompleted{Status: events.StatusCancelled})
		case limitCtx.Err() != nil:
			return finish(events.RunCompleted{Status: events.StatusLimitReached})
		}
		info := &events.ErrorInfo{Kind: "mcp_unavailable", Message: err.Error()}
		var me *mcp.Error
		if errors.As(err, &me) {
			info.HTTPStatus = me.HTTPStatus
		}
		fmt.Fprintf(stderr, "quill: %s\n", info.Message)
		return finish(events.RunCompleted{Status: events.StatusMCPError, Error: info})
	}
	if cfg.MCPURL != "" {
		remote = mcp.New(cfg.MCPURL, cfg.MCPToken, events.Version)
	}

	system := prompt.System()
	// newTools is the tool list of a new session: the built-ins, then the
	// MCP server's tools, listed once. A session keeps its list for good.
	newTools := func() (json.RawMessage, error) {
		defs := tools.NewRegistry(tools.Profile(profile), cfg.Workspace, nil).Definitions()
		if remote == nil {
			return defs, nil
		}
		extra, err := remote.List(startCtx)
		if err != nil {
			return nil, err
		}
		return tools.AppendDefinitions(defs, extra), nil
	}
	var toolDefs json.RawMessage
	var store session.Store = session.Discard{}
	var history []json.RawMessage
	var mcpErr *mcp.Error
	if cfg.SessionPath == "" {
		toolDefs, err = newTools()
		if err != nil {
			return mcpFailed(err)
		}
	} else {
		f, created, err := session.OpenWith(cfg.SessionPath, func() (session.Header, error) {
			defs, err := newTools()
			return session.Header{Model: model, Effort: effort, Profile: profile, System: system, Tools: defs}, err
		})
		if errors.As(err, &mcpErr) {
			return mcpFailed(err)
		}
		if errors.Is(err, session.ErrLocked) {
			return usage("--session: %v", err)
		}
		if errors.Is(err, session.ErrEmpty) {
			return usage("--session: session file is empty (an interrupted create); delete it if no run is using it")
		}
		if err != nil {
			var syn *json.SyntaxError
			if errors.As(err, &syn) {
				return usage("--session: session header is corrupt")
			}
			return usage("--session: %v", err)
		}
		sess, store = f, f
		toolDefs = f.Header.Tools
		if !created {
			h := f.Header
			for _, s := range []struct{ flag, set, saved string }{
				{"model", model, h.Model}, {"effort", effort, h.Effort}, {"tools", profile, h.Profile},
			} {
				if cfg.Explicit[s.flag] && s.set != s.saved {
					return usage("--%s %q differs from the session's %q", s.flag, s.set, s.saved)
				}
			}
			if h.Profile != string(tools.ReadOnly) && h.Profile != string(tools.ReadWrite) {
				return usage("--session: invalid tool_profile %q in session header", h.Profile)
			}
			model, effort, profile = h.Model, h.Effort, h.Profile
			system, toolDefs = h.System, h.Tools
			history = f.Messages
			// The stored tools are used as they are; the server is not
			// listed again.
			names, err := tools.RemoteNames(toolDefs)
			switch {
			case err != nil:
				return usage("--session: invalid tools in session header")
			case len(names) > 0 && remote == nil:
				return usage("the session has MCP tools: --mcp-url is required")
			case len(names) == 0 && remote != nil:
				return usage("the session has no MCP tools: --mcp-url cannot be added to it")
			}
			mcpTools = names
			if remote != nil {
				if err := remote.Connect(startCtx); err != nil {
					return mcpFailed(err)
				}
			}
		}
	}
	if mcpTools, err = tools.RemoteNames(toolDefs); err != nil {
		return internal("read tool names", err)
	}

	// The tool environment comes before run.started, which lists the names
	// actually passed.
	tmpDir, err = os.MkdirTemp(getenv("TMPDIR"), "quill-run-*")
	if err != nil {
		return internal("create temp dir", err)
	}
	var env []string
	env, envPassed = proc.BuildEnv(getenv, cfg.EnvNames, tmpDir)

	startRun()

	var next json.RawMessage
	if len(history) == 0 {
		next, err = prompt.FirstMessage(cfg.Workspace, cfg.Prompt)
	} else {
		next, err = prompt.ResumeMessage(history[len(history)-1], cfg.Prompt)
	}
	if err != nil {
		return internal("build message", err)
	}

	runner = proc.NewRunner(cfg.Workspace, env)
	registry := tools.NewRegistry(tools.Profile(profile), cfg.Workspace, tools.NewShell(runner, shellTimeout))
	if remote != nil {
		registry.SetRemote(remote, mcpTools)
	}

	loop := &agent.Loop{
		Client:   provider.NewClaude(cfg.APIKey),
		Registry: registry,
		Store:    store,
		Events:   ew,
		Opts: agent.Options{
			Model: model, Effort: effort, System: system, Tools: toolDefs,
			MaxSteps: cfg.MaxSteps, MaxTokens: cfg.MaxTokens,
		},
		History: history,
	}
	return finish(loop.Run(ctx, limitCtx, next))
}
