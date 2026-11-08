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
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()
	ctx, cancelRun := context.WithCancel(sigCtx)
	limitCtx, cancelLimit := context.WithTimeout(context.Background(), cfg.Timeout)

	var (
		sess   *session.File
		runner *proc.Runner
		tmpDir string
		done   bool
	)
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
			code = finish(events.RunCompleted{
				Status: events.StatusInternalError,
				Error:  &events.ErrorInfo{Kind: "internal", Message: "panic"},
			})
		}
	}()

	// Settings: from the flags, or from the session header when continuing.
	model, effort, sandbox := cfg.Model, cfg.Effort, cfg.Sandbox
	system := prompt.System()
	toolDefs := tools.NewRegistry(tools.Profile(sandbox), cfg.Workspace, nil).Definitions()
	var store session.Store = session.Discard{}
	var history []json.RawMessage
	if cfg.SessionPath != "" {
		f, created, err := session.Open(cfg.SessionPath, session.Header{
			Model: model, Effort: effort, Sandbox: sandbox, System: system, Tools: toolDefs,
		})
		if errors.Is(err, session.ErrLocked) {
			return usage("--session: %v", err)
		}
		if err != nil {
			var syn *json.SyntaxError
			if errors.As(err, &syn) {
				return usage("--session: session header is corrupt")
			}
			return usage("--session: %v", err)
		}
		sess, store = f, f
		if !created {
			h := f.Header
			for _, s := range []struct{ flag, set, saved string }{
				{"model", model, h.Model}, {"effort", effort, h.Effort}, {"sandbox", sandbox, h.Sandbox},
			} {
				if cfg.Explicit[s.flag] && s.set != s.saved {
					return usage("--%s %q differs from the session's %q", s.flag, s.set, s.saved)
				}
			}
			if h.Sandbox != string(tools.ReadOnly) && h.Sandbox != string(tools.WorkspaceWrite) {
				return usage("--session: invalid sandbox %q in session header", h.Sandbox)
			}
			model, effort, sandbox = h.Model, h.Effort, h.Sandbox
			system, toolDefs = h.System, h.Tools
			history = f.Messages
		}
	}

	envPassed := append([]string{}, cfg.EnvNames...)
	_ = ew.RunStarted(events.RunStarted{
		Session: cfg.SessionPath, Model: model, Effort: effort, Sandbox: sandbox,
		Workspace: cfg.Workspace, Version: events.Version, EnvPassed: envPassed,
	})

	var next json.RawMessage
	if len(history) == 0 {
		next, err = prompt.FirstMessage(cfg.Workspace, cfg.Prompt)
	} else {
		next, err = prompt.ResumeMessage(history[len(history)-1], cfg.Prompt)
	}
	if err != nil {
		return internal("build message", err)
	}

	tmpDir, err = os.MkdirTemp("", "quill-run-*")
	if err != nil {
		return internal("create temp dir", err)
	}
	runner = proc.NewRunner(cfg.Workspace, proc.BuildEnv(getenv, cfg.EnvNames, tmpDir))
	registry := tools.NewRegistry(tools.Profile(sandbox), cfg.Workspace, tools.NewShell(runner, shellTimeout))

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
