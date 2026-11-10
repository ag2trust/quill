// Package cli parses the quill command line.
package cli

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Config is the validated configuration for one run.
type Config struct {
	Prompt, Workspace, Model, Effort, Profile, SessionPath, APIKey string
	// MCPURL is the caller's MCP server, or empty. MCPToken is its optional
	// bearer token, from QUILL_MCP_TOKEN only.
	MCPURL, MCPToken string
	MaxSteps         int
	MaxTokens        int64
	Timeout          time.Duration
	EnvNames         []string
	// Explicit holds the names of flags the caller set.
	Explicit map[string]bool
}

// UsageError reports bad command-line usage (exit code 2).
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

func usagef(format string, a ...any) error { return &UsageError{Msg: fmt.Sprintf(format, a...)} }

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type envList []string

func (l *envList) String() string     { return strings.Join(*l, ",") }
func (l *envList) Set(v string) error { *l = append(*l, v); return nil }

// Parse parses args (excluding the program name, starting with "exec").
func Parse(args []string, stdin io.Reader, getenv func(string) string) (Config, error) {
	if len(args) == 0 {
		return Config{}, usagef("missing subcommand; usage: quill exec [flags] PROMPT|-")
	}
	if args[0] != "exec" {
		return Config{}, usagef("unknown subcommand %q; usage: quill exec [flags] PROMPT|-", args[0])
	}
	var (
		c         Config
		workspace string
		session   string
		mcpURL    string
		envs      envList
	)
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&workspace, "workspace", ".", "")
	fs.StringVar(&c.Model, "model", "claude-opus-5-5", "")
	fs.StringVar(&c.Effort, "effort", "high", "")
	fs.StringVar(&c.Profile, "tools", "read-write", "")
	fs.IntVar(&c.MaxSteps, "max-steps", 100, "")
	fs.Int64Var(&c.MaxTokens, "max-tokens", 2000000, "")
	fs.DurationVar(&c.Timeout, "timeout", 30*time.Minute, "")
	fs.StringVar(&session, "session", "", "")
	fs.Var(&envs, "env", "")
	fs.StringVar(&mcpURL, "mcp-url", "", "")

	// Flags may appear before or after the positional prompt.
	var positional []string
	rest := args[1:]
	for {
		if err := fs.Parse(rest); err != nil {
			return Config{}, usagef("%v", err)
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
	c.Explicit = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { c.Explicit[f.Name] = true })

	if len(positional) != 1 {
		return Config{}, usagef("expected exactly one prompt (or -), got %d", len(positional))
	}
	c.Prompt = positional[0]
	if c.Prompt == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return Config{}, usagef("reading prompt from stdin: %v", err)
		}
		c.Prompt = string(b)
	}
	if strings.TrimSpace(c.Prompt) == "" {
		return Config{}, usagef("prompt is empty")
	}

	if c.MaxSteps <= 0 || c.MaxTokens <= 0 || c.Timeout <= 0 {
		return Config{}, usagef("--max-steps, --max-tokens and --timeout must be greater than zero")
	}
	switch c.Effort {
	case "low", "medium", "high", "xhigh", "max":
	default:
		return Config{}, usagef("invalid --effort %q (low|medium|high|xhigh|max)", c.Effort)
	}
	switch c.Profile {
	case "read-only", "read-write":
	default:
		return Config{}, usagef("invalid --tools %q (read-only|read-write)", c.Profile)
	}

	for _, n := range envs {
		if n == "ANTHROPIC_API_KEY" || n == "QUILL_MCP_TOKEN" {
			return Config{}, usagef("--env %s is not allowed", n)
		}
		if !envName.MatchString(n) {
			name, _, _ := strings.Cut(n, "=")
			return Config{}, usagef("invalid --env name (before '='): %q", name)
		}
	}
	c.EnvNames = []string(envs)

	if getenv("ANTHROPIC_API_KEY") == "" {
		return Config{}, usagef("ANTHROPIC_API_KEY is not set")
	}
	c.APIKey = getenv("ANTHROPIC_API_KEY")

	if mcpURL != "" {
		// The URL is not echoed: it may carry something private.
		u, err := url.Parse(mcpURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return Config{}, usagef("invalid --mcp-url: want an http or https URL without credentials")
		}
		c.MCPURL = u.String()
		c.MCPToken = getenv("QUILL_MCP_TOKEN")
		if strings.ContainsFunc(c.MCPToken, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return Config{}, usagef("QUILL_MCP_TOKEN contains control characters")
		}
	}

	ws, err := filepath.Abs(workspace)
	if err == nil {
		ws, err = filepath.EvalSymlinks(ws)
	}
	if err != nil {
		return Config{}, usagef("invalid --workspace %q: %v", workspace, err)
	}
	if st, err := os.Stat(ws); err != nil || !st.IsDir() {
		return Config{}, usagef("--workspace %q is not a directory", workspace)
	}
	c.Workspace = ws

	if session != "" {
		abs, err := filepath.Abs(session)
		if err != nil {
			return Config{}, usagef("invalid --session %q: %v", session, err)
		}
		parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
		if err != nil {
			return Config{}, usagef("--session parent directory must exist: %v", err)
		}
		resolved := filepath.Join(parent, filepath.Base(abs))
		if full, err := filepath.EvalSymlinks(resolved); err == nil {
			resolved = full
		}
		if rel, err := filepath.Rel(ws, resolved); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return Config{}, usagef("--session must not be inside the workspace")
		}
		c.SessionPath = resolved
	}
	return c, nil
}
