package tools

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"quill/internal/proc"
)

func newTestShell(t *testing.T, env []string, timeout time.Duration) Shell {
	t.Helper()
	r := proc.NewRunner(t.TempDir(), env)
	t.Cleanup(func() { r.KillAll(time.Second) })
	return NewShell(r, timeout)
}

func TestShellExitCode(t *testing.T) {
	sh := newTestShell(t, []string{"PATH=/usr/bin:/bin"}, time.Minute)
	if got := sh.Run(context.Background(), "echo hi"); got != (Result{Content: "hi\n[exit code 0]"}) {
		t.Fatalf("got %+v", got)
	}
	if got := sh.Run(context.Background(), "echo bad >&2; exit 2"); got != (Result{Content: "bad\n[exit code 2]"}) {
		t.Fatalf("non-zero exit: got %+v", got)
	}
}

func TestShellMarkerSeparator(t *testing.T) {
	sh := newTestShell(t, []string{"PATH=/usr/bin:/bin"}, time.Minute)
	for cmd, want := range map[string]string{
		"true":         "[exit code 0]",
		"printf abc":   "abc\n[exit code 0]",
		"printf 'a\n'": "a\n[exit code 0]",
	} {
		if got := sh.Run(context.Background(), cmd); got != (Result{Content: want}) {
			t.Errorf("%s: got %q, want %q", cmd, got.Content, want)
		}
	}
	if got := newTestShell(t, []string{"PATH=/usr/bin:/bin"}, 200*time.Millisecond).Run(context.Background(), "sleep 30"); got.Content != "[killed after 200ms timeout]" {
		t.Errorf("timeout with no output: got %q", got.Content)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := sh.Run(ctx, "true"); got != (Result{Content: "[cancelled]", IsError: true}) {
		t.Errorf("cancelled with no output: got %+v", got)
	}
}

func TestShellTimeout(t *testing.T) {
	sh := newTestShell(t, []string{"PATH=/usr/bin:/bin"}, 300*time.Millisecond)
	got := sh.Run(context.Background(), "echo start; sleep 30")
	if !got.IsError || !strings.HasPrefix(got.Content, "start\n") || !strings.HasSuffix(got.Content, "[killed after 300ms timeout]") {
		t.Fatalf("got %+v", got)
	}
}

func TestShellCancelled(t *testing.T) {
	sh := newTestShell(t, []string{"PATH=/usr/bin:/bin"}, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	got := sh.Run(ctx, "sleep 30")
	if !got.IsError || !strings.HasSuffix(got.Content, "[cancelled]") {
		t.Fatalf("got %+v", got)
	}
}

func TestEndlessOutputBounded(t *testing.T) {
	sh := newTestShell(t, []string{"PATH=/usr/bin:/bin"}, time.Second)
	got := sh.Run(context.Background(), "yes")
	if !got.IsError || len(got.Content) >= 30100 {
		t.Fatalf("IsError=%v len=%d, want IsError and len < 30100", got.IsError, len(got.Content))
	}
	if !strings.Contains(got.Content, "bytes omitted") || !strings.HasSuffix(got.Content, "[killed after 1s timeout]") {
		t.Fatalf("unexpected content tail %q", got.Content[max(0, len(got.Content)-200):])
	}
}

func TestShellEnvClean(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-secret")
	t.Setenv("SHELL_TEST_SECRET", "hidden")
	t.Setenv("SHELL_TEST_PASSED", "visible")
	env := proc.BuildEnv(os.Getenv, []string{"SHELL_TEST_PASSED", "ANTHROPIC_API_KEY"}, t.TempDir())
	got := newTestShell(t, env, time.Minute).Run(context.Background(), "env")
	if got.IsError {
		t.Fatalf("got %+v", got)
	}
	for _, bad := range []string{"ANTHROPIC_API_KEY", "sk-test-secret", "SHELL_TEST_SECRET"} {
		if strings.Contains(got.Content, bad) {
			t.Errorf("env output contains %q", bad)
		}
	}
	for _, want := range []string{"PATH=", "TMPDIR=", "SHELL_TEST_PASSED=visible"} {
		if !strings.Contains(got.Content, want) {
			t.Errorf("env output lacks %q", want)
		}
	}
}

// The registry's cap must not cut shell output a second time: the marker
// must report the real number of bytes the shell dropped.
func TestRegistryShellTruncatedOnce(t *testing.T) {
	r := NewRegistry(WorkspaceWrite, newWS(t), newTestShell(t, []string{"PATH=/usr/bin:/bin"}, time.Second))
	got := r.Run(context.Background(), "shell", []byte(`{"command":"yes"}`))
	if !got.IsError || len(got.Content) > OutputCap || !strings.HasSuffix(got.Content, "[killed after 1s timeout]") {
		t.Fatalf("IsError=%v len=%d tail=%q", got.IsError, len(got.Content), got.Content[max(0, len(got.Content)-100):])
	}
	ms := regexp.MustCompile(`\[\.\.\. (\d+) bytes omitted \.\.\.\]`).FindAllStringSubmatch(got.Content, -1)
	if len(ms) != 1 {
		t.Fatalf("want one omission marker, got %d", len(ms))
	}
	if n, _ := strconv.Atoi(ms[0][1]); n <= 1_000_000 {
		t.Fatalf("omitted %d bytes, want > 1000000", n)
	}
}
