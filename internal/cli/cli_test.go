package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

var keyEnv = env(map[string]string{"ANTHROPIC_API_KEY": "k"})

func TestDefaults(t *testing.T) {
	c, err := Parse([]string{"exec", "-"}, strings.NewReader("fix it"), keyEnv)
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "claude-opus-5-5" || c.Effort != "high" || c.Sandbox != "workspace-write" ||
		c.MaxSteps != 100 || c.MaxTokens != 2000000 || c.Timeout != 30*time.Minute ||
		c.Prompt != "fix it" || c.APIKey != "k" {
		t.Fatalf("unexpected config: %+v", c)
	}
	if !filepath.IsAbs(c.Workspace) {
		t.Fatalf("workspace not absolute: %q", c.Workspace)
	}
}

func TestPositionalPrompt(t *testing.T) {
	c, err := Parse([]string{"exec", "do x"}, strings.NewReader(""), keyEnv)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt != "do x" {
		t.Fatalf("prompt %q", c.Prompt)
	}
}

func TestUsageErrors(t *testing.T) {
	ws := t.TempDir()
	file := filepath.Join(t.TempDir(), "f")
	os.WriteFile(file, nil, 0o600)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(ws, link); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(ws, "hist.json")
	os.WriteFile(target, nil, 0o600)
	sessLink := filepath.Join(t.TempDir(), "s.json")
	if err := os.Symlink(target, sessLink); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		args  []string
		stdin string
		env   func(string) string
	}{
		{"no subcommand", nil, "", keyEnv},
		{"run", []string{"run", "x"}, "", keyEnv},
		{"two positionals", []string{"exec", "a", "b"}, "", keyEnv},
		{"empty stdin", []string{"exec", "-"}, "  \n", keyEnv},
		{"bad effort", []string{"exec", "--effort", "extreme", "x"}, "", keyEnv},
		{"bad sandbox", []string{"exec", "--sandbox", "none", "x"}, "", keyEnv},
		{"env api key", []string{"exec", "--env", "ANTHROPIC_API_KEY", "x"}, "", keyEnv},
		{"bad env name", []string{"exec", "--env", "1BAD", "x"}, "", keyEnv},
		{"missing key", []string{"exec", "x"}, "", env(nil)},
		{"unknown flag", []string{"exec", "--nope", "x"}, "", keyEnv},
		{"workspace is file", []string{"exec", "--workspace", file, "x"}, "", keyEnv},
		{"session in workspace", []string{"exec", "--workspace", ws, "--session", filepath.Join(ws, "s.json"), "x"}, "", keyEnv},
		{"session final symlink", []string{"exec", "--workspace", ws, "--session", sessLink, "x"}, "", keyEnv},
		{"zero max-steps", []string{"exec", "--max-steps", "0", "x"}, "", keyEnv},
		{"negative max-tokens", []string{"exec", "--max-tokens", "-1", "x"}, "", keyEnv},
		{"zero timeout", []string{"exec", "--timeout", "0s", "x"}, "", keyEnv},
		{"session via symlink", []string{"exec", "--workspace", ws, "--session", filepath.Join(link, "s.json"), "x"}, "", keyEnv},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.args, strings.NewReader(tc.stdin), tc.env)
			var ue *UsageError
			if !errors.As(err, &ue) {
				t.Fatalf("want *UsageError, got %v", err)
			}
		})
	}
}

func TestSessionOutsideWorkspace(t *testing.T) {
	ws, other := t.TempDir(), t.TempDir()
	c, err := Parse([]string{"exec", "--workspace", ws, "--session", filepath.Join(other, "s.json"), "x"}, strings.NewReader(""), keyEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.SessionPath) || filepath.Base(c.SessionPath) != "s.json" {
		t.Fatalf("session %q", c.SessionPath)
	}
}

func TestExplicitFlags(t *testing.T) {
	c, err := Parse([]string{"exec", "--model", "claude-haiku-5-5", "x"}, strings.NewReader(""), keyEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Explicit["model"] || c.Explicit["effort"] {
		t.Fatalf("explicit: %v", c.Explicit)
	}
}

func TestEnvRepeatable(t *testing.T) {
	c, err := Parse([]string{"exec", "--env", "B_VAR", "--env", "A_VAR", "x"}, strings.NewReader(""), keyEnv)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.EnvNames) != 2 || c.EnvNames[0] != "B_VAR" || c.EnvNames[1] != "A_VAR" {
		t.Fatalf("env names: %v", c.EnvNames)
	}
}

func TestEnvErrorDoesNotEchoValue(t *testing.T) {
	_, err := Parse([]string{"exec", "--env", "1BAD=topsecret", "x"}, strings.NewReader(""), keyEnv)
	if err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("error leaks value: %v", err)
	}
}
