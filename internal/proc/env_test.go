package proc

import (
	"slices"
	"testing"
)

func TestBuildEnv(t *testing.T) {
	vars := map[string]string{
		"PATH":              "/usr/bin:/bin",
		"SECRET":            "x",
		"ANTHROPIC_API_KEY": "k",
		"QUILL_MCP_TOKEN":   "t",
		"CALLER_X":          "1",
	}
	getenv := func(k string) string { return vars[k] }
	env, passed := BuildEnv(getenv, []string{"CALLER_X", "ANTHROPIC_API_KEY", "QUILL_MCP_TOKEN"}, "/tmp/run")
	want := []string{"PATH=/usr/bin:/bin", "TMPDIR=/tmp/run", "CALLER_X=1"}
	if !slices.Equal(env, want) {
		t.Fatalf("env = %q, want %q", env, want)
	}
	if !slices.Equal(passed, []string{"CALLER_X"}) {
		t.Fatalf("passed = %q, want [CALLER_X]", passed)
	}
}

func TestBuildEnvBaseAndDuplicates(t *testing.T) {
	vars := map[string]string{"PATH": "p", "HOME": "h", "USER": "u", "LANG": "l", "TMPDIR": "/host/tmp", "A": "1", "EMPTY": ""}
	getenv := func(k string) string { return vars[k] }
	env, passed := BuildEnv(getenv, []string{"A", "PATH", "TMPDIR", "A", "MISSING", "EMPTY"}, "/run/tmp")
	want := []string{"PATH=p", "HOME=h", "USER=u", "LANG=l", "TMPDIR=/run/tmp", "A=1"}
	if !slices.Equal(env, want) {
		t.Fatalf("env = %q, want %q", env, want)
	}
	// Unset, empty and Quill-owned (TMPDIR) names are not passed.
	if !slices.Equal(passed, []string{"A", "PATH"}) {
		t.Fatalf("passed = %q, want [A PATH]", passed)
	}
}

func TestBuildEnvPassedNotNil(t *testing.T) {
	if _, passed := BuildEnv(func(string) string { return "" }, nil, "/t"); passed == nil {
		t.Fatal("passed is nil")
	}
}
