//go:build live

// Package live holds opt-in tests that call the real Anthropic API. They
// spend money, so each one skips unless ANTHROPIC_API_KEY is set and
// QUILL_LIVE=1.
package live

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

var (
	buildOnce sync.Once
	quillBin  string
	buildErr  error
)

// live must be the first call of every test. It skips unless both the key and
// QUILL_LIVE=1 are present, then builds the binary once.
func live(t *testing.T) string {
	t.Helper()
	if os.Getenv("ANTHROPIC_API_KEY") == "" || os.Getenv("QUILL_LIVE") != "1" {
		t.Skip("live test: set ANTHROPIC_API_KEY and QUILL_LIVE=1 to run")
	}
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "quill-live-*")
		if err != nil {
			buildErr = err
			return
		}
		quillBin = filepath.Join(dir, "quill")
		b := exec.Command("go", "build", "-o", quillBin, "quill/cmd/quill")
		if out, err := b.CombinedOutput(); err != nil {
			buildErr = err
			t.Logf("build output: %s", out)
		}
	})
	if buildErr != nil {
		t.Fatalf("build quill: %v", buildErr)
	}
	return quillBin
}

var liveFlags = []string{"--model", "claude-haiku-5-5", "--effort", "low", "--max-steps", "8", "--timeout", "3m"}

func quillCmd(bin, prompt, ws, sess string) *exec.Cmd {
	args := append([]string{"exec"}, liveFlags...)
	args = append(args, "--workspace", ws)
	if sess != "" {
		args = append(args, "--session", sess)
	}
	args = append(args, "-")
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"ANTHROPIC_API_KEY=" + os.Getenv("ANTHROPIC_API_KEY"),
	}
	cmd.Stdin = strings.NewReader(prompt)
	return cmd
}

type result struct {
	code   int
	stderr string
	lines  []map[string]any
}

func parse(t *testing.T, stdout string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, l := range strings.Split(stdout, "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("stdout line is not JSON: %q", l)
		}
		lines = append(lines, m)
	}
	return lines
}

// run runs quill to completion and logs its run.completed line.
func run(t *testing.T, bin, prompt, ws, sess string) result {
	t.Helper()
	cmd := quillCmd(bin, prompt, ws, sess)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		t.Fatalf("run quill: %v", err)
	}
	r := result{code: cmd.ProcessState.ExitCode(), stderr: errb.String(), lines: parse(t, out.String())}
	logCompleted(t, r)
	return r
}

func completed(r result) map[string]any {
	for i := len(r.lines) - 1; i >= 0; i-- {
		if r.lines[i]["type"] == "run.completed" {
			return r.lines[i]
		}
	}
	return nil
}

func logCompleted(t *testing.T, r result) {
	t.Helper()
	if c := completed(r); c != nil {
		b, _ := json.Marshal(c)
		t.Logf("exit %d run.completed: %s", r.code, b)
	} else {
		t.Logf("exit %d, no run.completed; stderr: %s", r.code, r.stderr)
	}
}

func wantExit(t *testing.T, r result, want int) {
	t.Helper()
	if r.code != want {
		t.Fatalf("exit = %d, want %d; stderr: %s", r.code, want, r.stderr)
	}
}

func wantFile(t *testing.T, ws, name, want string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(ws, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if got := strings.TrimSpace(string(b)); got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

func sessionPath(t *testing.T) string {
	return filepath.Join(t.TempDir(), "session.jsonl")
}

func TestLiveWriteFile(t *testing.T) {
	bin := live(t)
	ws := t.TempDir()
	r := run(t, bin, "Create hello.txt containing exactly: hi", ws, "")
	wantExit(t, r, 0)
	wantFile(t, ws, "hello.txt", "hi")
}

func TestLiveCacheReads(t *testing.T) {
	bin := live(t)
	ws := t.TempDir()
	r := run(t, bin, "Create a.txt containing a, then read it back with the read tool, then create b.txt containing b. Reply done.", ws, "")
	wantExit(t, r, 0)
	c := completed(r)
	u, _ := c["usage"].(map[string]any)
	if cr, _ := u["cache_read"].(float64); cr <= 0 {
		t.Errorf("usage.cache_read = %v, want > 0 (usage: %v)", u["cache_read"], u)
	}
}

func TestLiveResume(t *testing.T) {
	bin := live(t)
	ws, sess := t.TempDir(), sessionPath(t)
	wantExit(t, run(t, bin, "Create hello.txt containing exactly: hi", ws, sess), 0)
	wantExit(t, run(t, bin, "Now also create bye.txt containing bye", ws, sess), 0)
	wantFile(t, ws, "hello.txt", "hi")
	wantFile(t, ws, "bye.txt", "bye")
}

// TestLiveResumeAfterInterrupted interrupts a real run mid-shell-call, then
// resumes it. The resumed request carries two consecutive user messages (the
// "not run: cancelled" tool_result, then the prompt); this checks the API
// accepts that.
func TestLiveResumeAfterInterrupted(t *testing.T) {
	bin := live(t)
	ws, sess := t.TempDir(), sessionPath(t)
	t.Cleanup(func() { exec.Command("pkill", "-f", "sleep 20").Run() })

	cmd := quillCmd(bin, "Run exactly this shell command and nothing else: sleep 20", ws, sess)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	sc := bufio.NewScanner(stdout)
	started := false
	for sc.Scan() {
		out.WriteString(sc.Text() + "\n")
		if strings.Contains(sc.Text(), `"item.started"`) && strings.Contains(sc.Text(), `"name":"shell"`) {
			started = true
			break
		}
	}
	if !started {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("model never started a shell call; stdout so far: %s", out.String())
	}
	cmd.Process.Signal(syscall.SIGTERM)
	rest, _ := io.ReadAll(stdout)
	out.Write(rest)
	cmd.Wait()
	r1 := result{code: cmd.ProcessState.ExitCode(), lines: parse(t, out.String())}
	logCompleted(t, r1)
	wantExit(t, r1, 143)

	r2 := run(t, bin, "Continue the task.", ws, sess)
	if r2.code == 4 {
		t.Fatalf("resume got provider_error (exit 4), the API rejected the history; stderr: %s", r2.stderr)
	}
	wantExit(t, r2, 0)
}

func TestLiveConflictBlocks(t *testing.T) {
	bin := live(t)
	ws := t.TempDir()
	const schema = "CREATE TABLE users (id INTEGER);\n"
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"),
		[]byte("Never edit schema.sql directly; add a migration in migrations/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "schema.sql"), []byte(schema), 0o644); err != nil {
		t.Fatal(err)
	}
	r := run(t, bin, "Edit schema.sql to add a created_at column to users", ws, "")
	if r.code != 0 && r.code != 5 {
		t.Fatalf("exit = %d, want 0 or 5; stderr: %s", r.code, r.stderr)
	}
	if r.code == 5 {
		t.Logf("outcome: blocked (raise_blocker)")
	} else {
		b, _ := os.ReadFile(filepath.Join(ws, "schema.sql"))
		_, merr := os.Stat(filepath.Join(ws, "migrations"))
		t.Logf("outcome: completed without blocking; schema.sql edited=%v, migrations/ created=%v",
			string(b) != schema, merr == nil)
	}
}
