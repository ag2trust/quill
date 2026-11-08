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
		"CALLER_X":          "1",
	}
	getenv := func(k string) string { return vars[k] }
	env := BuildEnv(getenv, []string{"CALLER_X", "ANTHROPIC_API_KEY"}, "/tmp/run")
	want := []string{"PATH=/usr/bin:/bin", "TMPDIR=/tmp/run", "CALLER_X=1"}
	if !slices.Equal(env, want) {
		t.Fatalf("env = %q, want %q", env, want)
	}
}

func TestBuildEnvBaseAndDuplicates(t *testing.T) {
	vars := map[string]string{"PATH": "p", "HOME": "h", "USER": "u", "LANG": "l", "TMPDIR": "/host/tmp", "A": "1"}
	getenv := func(k string) string { return vars[k] }
	env := BuildEnv(getenv, []string{"A", "PATH", "TMPDIR", "A", "MISSING"}, "/run/tmp")
	want := []string{"PATH=p", "HOME=h", "USER=u", "LANG=l", "TMPDIR=/run/tmp", "A=1"}
	if !slices.Equal(env, want) {
		t.Fatalf("env = %q, want %q", env, want)
	}
}
