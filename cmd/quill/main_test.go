package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"quill/internal/events"
	"quill/internal/fakeapi"
)

var quillBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "quill-e2e-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	quillBin = filepath.Join(dir, "quill")
	build := exec.Command("go", "build", "-o", quillBin, ".")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build quill:", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// quillCmd returns a command running the built binary with a clean
// environment pointing at fake. tmp becomes quill's TMPDIR.
func quillCmd(fake *fakeapi.Server, tmp, stdin string, args ...string) *exec.Cmd {
	cmd := exec.Command(quillBin, args...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=" + tmp,
		"ANTHROPIC_API_KEY=test",
		"ANTHROPIC_BASE_URL=" + fake.URL(),
	}
	cmd.Stdin = strings.NewReader(stdin)
	return cmd
}

type result struct {
	code           int
	stdout, stderr string
	lines          []map[string]any
}

// runQuill runs quill to completion with stdin as the prompt.
func runQuill(t *testing.T, fake *fakeapi.Server, stdin string, args ...string) result {
	t.Helper()
	cmd := quillCmd(fake, t.TempDir(), stdin, append([]string{"exec"}, append(args, "-")...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		t.Fatalf("run quill: %v", err)
	}
	r := result{code: cmd.ProcessState.ExitCode(), stdout: out.String(), stderr: errb.String()}
	r.lines = parseLines(t, r.stdout)
	checkStream(t, r.code, r.lines)
	return r
}

// checkStream checks the stdout contract: nothing on exit 2, otherwise
// run.started first and exactly one run.completed, last.
func checkStream(t *testing.T, code int, lines []map[string]any) {
	t.Helper()
	if code == 2 {
		if len(lines) != 0 {
			t.Errorf("exit 2 wrote %d stdout lines", len(lines))
		}
		return
	}
	if len(lines) == 0 {
		t.Fatalf("exit %d with no stdout lines", code)
	}
	if lines[0]["type"] != "run.started" {
		t.Errorf("first line type = %v, want run.started", lines[0]["type"])
	}
	n := 0
	for _, l := range lines {
		if l["type"] == "run.completed" {
			n++
		}
	}
	if n != 1 || lines[len(lines)-1]["type"] != "run.completed" {
		t.Errorf("want exactly one run.completed, last; got %d, last type %v", n, lines[len(lines)-1]["type"])
	}
}

// parseLines checks every stdout line is a JSON object and decodes it.
func parseLines(t *testing.T, stdout string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
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

func last(t *testing.T, r result) map[string]any {
	t.Helper()
	if len(r.lines) == 0 {
		t.Fatalf("no stdout lines; stderr: %s", r.stderr)
	}
	l := r.lines[len(r.lines)-1]
	if l["type"] != "run.completed" {
		t.Fatalf("last line type = %v, want run.completed", l["type"])
	}
	return l
}

type apiRequest struct {
	System   json.RawMessage   `json:"system"`
	Tools    json.RawMessage   `json:"tools"`
	Messages []json.RawMessage `json:"messages"`
}

func request(t *testing.T, body []byte) apiRequest {
	t.Helper()
	var r apiRequest
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("request body: %v", err)
	}
	return r
}

type msg struct {
	Role    string `json:"role"`
	Content []struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		ToolUseID string `json:"tool_use_id"`
		Content   string `json:"content"`
	} `json:"content"`
}

func decodeMsg(t *testing.T, raw json.RawMessage) msg {
	t.Helper()
	var m msg
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("message: %v", err)
	}
	return m
}

func TestE2ECompleted(t *testing.T) {
	fake := fakeapi.New(t,
		fakeapi.ToolReply("tu1", "write", `{"path":"hello.txt","content":"hi"}`),
		fakeapi.TextReply("done", events.Usage{Input: 10, Output: 2}),
	)
	ws := t.TempDir()
	r := runQuill(t, fake, "write hello", "--workspace", ws)
	if r.code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", r.code, r.stderr)
	}
	l := last(t, r)
	if r.lines[0]["type"] != "run.started" {
		t.Fatalf("first line type = %v", r.lines[0]["type"])
	}
	if !strings.Contains(strings.SplitN(r.stdout, "\n", 2)[0], `"env_passed":[]`) {
		t.Errorf("run.started env_passed is not []: %s", strings.SplitN(r.stdout, "\n", 2)[0])
	}
	if l["status"] != "completed" || l["final_text"] != "done" {
		t.Errorf("run.completed = %v", l)
	}
	b, err := os.ReadFile(filepath.Join(ws, "hello.txt"))
	if err != nil || string(b) != "hi" {
		t.Errorf("hello.txt = %q, %v", b, err)
	}
}

func TestE2EUsageError(t *testing.T) {
	fake := fakeapi.New(t)
	cmd := quillCmd(fake, t.TempDir(), "", "exec")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if _, ok := cmd.Run().(*exec.ExitError); !ok {
		t.Fatal("quill did not exit with an error status")
	}
	if code := cmd.ProcessState.ExitCode(); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
	if errb.Len() == 0 {
		t.Error("stderr is empty")
	}
}

const blockerInput = `{"reason":"conflict","summary":"schema edit","details":"AGENTS.md says use migrations","question":"edit or migrate?"}`

func TestE2EBlockedThenResume(t *testing.T) {
	ws, sess := t.TempDir(), filepath.Join(t.TempDir(), "s.jsonl")

	fake1 := fakeapi.New(t, fakeapi.ToolReply("tu1", "raise_blocker", blockerInput))
	r := runQuill(t, fake1, "edit schema.sql", "--workspace", ws, "--session", sess)
	if r.code != 5 {
		t.Fatalf("run 1 exit = %d, want 5; stderr: %s", r.code, r.stderr)
	}
	l := last(t, r)
	if b, _ := l["blocker"].(map[string]any); b == nil || b["reason"] != "conflict" {
		t.Fatalf("blocker = %v", l["blocker"])
	}

	fake2 := fakeapi.New(t, fakeapi.TextReply("ok", events.Usage{}))
	r = runQuill(t, fake2, "use a migration", "--workspace", ws, "--session", sess)
	if r.code != 0 {
		t.Fatalf("run 2 exit = %d, want 0; stderr: %s", r.code, r.stderr)
	}
	req1 := request(t, fake1.Requests()[0])
	reqs := fake2.Requests()
	req2 := request(t, reqs[len(reqs)-1])
	if len(req2.Messages) != 3 {
		t.Fatalf("run 2 sent %d messages, want 3", len(req2.Messages))
	}
	for i, m := range req1.Messages {
		if !bytes.Equal(m, req2.Messages[i]) {
			t.Errorf("message %d differs:\nrun 1: %s\nrun 2: %s", i, m, req2.Messages[i])
		}
	}
	if !bytes.Equal(req1.System, req2.System) || !bytes.Equal(req1.Tools, req2.Tools) {
		t.Error("system or tools differ between runs")
	}
	m := decodeMsg(t, req2.Messages[2])
	if m.Role != "user" || len(m.Content) != 1 || m.Content[0].Type != "tool_result" ||
		m.Content[0].ToolUseID != "tu1" || m.Content[0].Content != "use a migration" {
		t.Errorf("resume message = %s", req2.Messages[2])
	}
}

func TestE2EResumeModelMismatch(t *testing.T) {
	for _, flag := range [][]string{
		{"--model", "claude-haiku-5-5"}, {"--effort", "low"}, {"--sandbox", "read-only"},
	} {
		t.Run(flag[0][2:], func(t *testing.T) {
			ws, sess := t.TempDir(), filepath.Join(t.TempDir(), "s.jsonl")
			r := runQuill(t, fakeapi.New(t, fakeapi.TextReply("done", events.Usage{})), "task", "--workspace", ws, "--session", sess)
			if r.code != 0 {
				t.Fatalf("run 1 exit = %d; stderr: %s", r.code, r.stderr)
			}
			fake2 := fakeapi.New(t)
			r = runQuill(t, fake2, "more", append([]string{"--workspace", ws, "--session", sess}, flag...)...)
			if r.code != 2 {
				t.Fatalf("exit = %d, want 2", r.code)
			}
			if r.stdout != "" || !strings.Contains(r.stderr, flag[0]) {
				t.Errorf("stdout = %q, stderr = %q", r.stdout, r.stderr)
			}
			if n := len(fake2.Requests()); n != 0 {
				t.Errorf("sent %d requests", n)
			}
		})
	}
}

// signalMidShell runs quill on a script whose shell call runs marker twice
// (once in the background), sends sig once marker is running, and waits up
// to 10 s for quill to exit. tmp is quill's TMPDIR.
func signalMidShell(t *testing.T, sig os.Signal, marker string, tmp string, args ...string) (code int, lines []map[string]any, elapsed time.Duration) {
	t.Helper()
	t.Cleanup(func() { exec.Command("pkill", "-f", marker).Run() })
	command := marker + " & " + marker
	input, _ := json.Marshal(map[string]string{"command": command})
	fake := fakeapi.New(t, fakeapi.ToolReply("tu1", "shell", string(input)))
	cmd := quillCmd(fake, tmp, "run it", append(append([]string{"exec"}, args...), "-")...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		out.WriteString(sc.Text() + "\n")
		if strings.Contains(sc.Text(), `"item.started"`) {
			break
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for exec.Command("pgrep", "-f", marker).Run() != nil {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("shell command never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	start := time.Now()
	cmd.Process.Signal(sig)
	done := make(chan []byte)
	go func() {
		rest, _ := io.ReadAll(stdout)
		cmd.Wait()
		done <- rest
	}()
	select {
	case rest := <-done:
		elapsed = time.Since(start)
		out.Write(rest)
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		<-done
		t.Fatalf("quill did not exit within 10 s of %v", sig)
	}
	code, lines = cmd.ProcessState.ExitCode(), parseLines(t, out.String())
	checkStream(t, code, lines)
	return code, lines, elapsed
}

func TestE2ESigterm(t *testing.T) {
	for sig, marker := range map[syscall.Signal]string{syscall.SIGTERM: "sleep 31.57", syscall.SIGHUP: "sleep 31.67"} {
		t.Run(sig.String(), func(t *testing.T) { testSignalCancels(t, sig, marker) })
	}
}

func testSignalCancels(t *testing.T, sig syscall.Signal, marker string) {
	tmp := t.TempDir()
	code, lines, elapsed := signalMidShell(t, sig, marker, tmp, "--workspace", t.TempDir())
	if code != 143 {
		t.Fatalf("exit = %d, want 143", code)
	}
	if elapsed >= 10*time.Second {
		t.Errorf("exit took %v after %v", elapsed, sig)
	}
	if len(lines) == 0 {
		t.Fatal("no stdout lines")
	}
	l := lines[len(lines)-1]
	if l["type"] != "run.completed" || l["status"] != "cancelled" {
		t.Errorf("last line = %v", l)
	}
	if exec.Command("pgrep", "-f", marker).Run() == nil {
		t.Errorf("%q survived quill", marker)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil || len(entries) != 0 {
		t.Errorf("TMPDIR not empty after exit: %v, %v", entries, err)
	}
}

func TestE2EResumeAfterCancel(t *testing.T) {
	ws, sess := t.TempDir(), filepath.Join(t.TempDir(), "s.jsonl")
	code, _, elapsed := signalMidShell(t, syscall.SIGTERM, "sleep 32.57", t.TempDir(), "--workspace", ws, "--session", sess)
	if code != 143 {
		t.Fatalf("run 1 exit = %d, want 143", code)
	}
	if elapsed >= 10*time.Second {
		t.Errorf("exit took %v after SIGTERM", elapsed)
	}
	fake := fakeapi.New(t, fakeapi.TextReply("ok", events.Usage{}))
	r := runQuill(t, fake, "continue", "--workspace", ws, "--session", sess)
	if r.code != 0 {
		t.Fatalf("run 2 exit = %d, want 0; stderr: %s", r.code, r.stderr)
	}
	reqs := fake.Requests()
	req := request(t, reqs[len(reqs)-1])
	var roles []string
	for _, raw := range req.Messages {
		roles = append(roles, decodeMsg(t, raw).Role)
	}
	// The cancelled run saved the interrupted call's tool_result, so the
	// resume prompt follows as a second consecutive user message; the API
	// combines consecutive user turns.
	if got := strings.Join(roles, ","); got != "user,assistant,user,user" {
		t.Fatalf("roles = %s", got)
	}
	cancelled := decodeMsg(t, req.Messages[2])
	if cancelled.Content[0].Type != "tool_result" || cancelled.Content[0].Content != "interrupted: it may have partly run" {
		t.Errorf("message 2 = %s", req.Messages[2])
	}
	resume := decodeMsg(t, req.Messages[3])
	if len(resume.Content) != 1 || resume.Content[0].Type != "text" || resume.Content[0].Text != "continue" {
		t.Errorf("message 3 = %s", req.Messages[3])
	}
}

func TestE2EProviderError(t *testing.T) {
	fake := fakeapi.New(t, fakeapi.Reply{Status: 401,
		Body: `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`})
	r := runQuill(t, fake, "task", "--workspace", t.TempDir())
	if r.code != 4 {
		t.Fatalf("exit = %d, want 4; stderr: %s", r.code, r.stderr)
	}
	l := last(t, r)
	if e, _ := l["error"].(map[string]any); e == nil || e["kind"] != "auth" {
		t.Errorf("error = %v", l["error"])
	}
}

func TestE2EGuidanceInFirstMessage(t *testing.T) {
	const marker = "Run make check before finishing."
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte(marker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := fakeapi.New(t, fakeapi.TextReply("ok", events.Usage{}))
	r := runQuill(t, fake, "task", "--workspace", ws)
	if r.code != 0 {
		t.Fatalf("exit = %d; stderr: %s", r.code, r.stderr)
	}
	req := request(t, fake.Requests()[0])
	first := decodeMsg(t, req.Messages[0])
	if !strings.Contains(first.Content[0].Text, "repository_guidance") || !strings.Contains(first.Content[0].Text, marker) {
		t.Errorf("first block = %q", first.Content[0].Text)
	}
	if bytes.Contains(req.System, []byte("repository_guidance")) || bytes.Contains(req.System, []byte(marker)) {
		t.Error("system prompt carries repository guidance")
	}
}

func TestE2ESessionLocked(t *testing.T) {
	ws, sess := t.TempDir(), filepath.Join(t.TempDir(), "s.jsonl")
	if r := runQuill(t, fakeapi.New(t, fakeapi.TextReply("done", events.Usage{})), "task", "--workspace", ws, "--session", sess); r.code != 0 {
		t.Fatalf("run 1 exit = %d; stderr: %s", r.code, r.stderr)
	}
	f, err := os.OpenFile(sess, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	r := runQuill(t, fakeapi.New(t), "more", "--workspace", ws, "--session", sess)
	if r.code != 2 || r.stdout != "" || !strings.Contains(r.stderr, "locked") {
		t.Errorf("exit = %d, stdout = %q, stderr = %q", r.code, r.stdout, r.stderr)
	}
}

func TestE2ESessionEmpty(t *testing.T) {
	sess := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(sess, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := runQuill(t, fakeapi.New(t), "task", "--workspace", t.TempDir(), "--session", sess)
	if r.code != 2 || r.stdout != "" || !strings.Contains(r.stderr, "session file is empty") || !strings.Contains(r.stderr, "delete it") {
		t.Errorf("exit = %d, stdout = %q, stderr = %q", r.code, r.stdout, r.stderr)
	}
}

func TestE2EEnvPassed(t *testing.T) {
	fake := fakeapi.New(t, fakeapi.TextReply("done", events.Usage{}))
	cmd := quillCmd(fake, t.TempDir(), "task", "exec", "--workspace", t.TempDir(),
		"--env", "QUILL_TEST_SET", "--env", "QUILL_TEST_UNSET", "-")
	cmd.Env = append(cmd.Env, "QUILL_TEST_SET=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run quill: %v", err)
	}
	lines := parseLines(t, string(out))
	checkStream(t, cmd.ProcessState.ExitCode(), lines)
	got, _ := json.Marshal(lines[0]["env_passed"])
	if string(got) != `["QUILL_TEST_SET"]` {
		t.Errorf("env_passed = %s, want [\"QUILL_TEST_SET\"]", got)
	}
}

func TestE2ESessionCorrupt(t *testing.T) {
	const secret = "s3cret-file-content"
	sess := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(sess, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runQuill(t, fakeapi.New(t), "task", "--workspace", t.TempDir(), "--session", sess)
	if r.code != 2 || r.stdout != "" || r.stderr == "" {
		t.Errorf("exit = %d, stdout = %q, stderr = %q", r.code, r.stdout, r.stderr)
	}
	if strings.Contains(r.stderr, "s3cret") {
		t.Errorf("stderr carries file contents: %q", r.stderr)
	}
}

func TestE2EResumeInheritsSandbox(t *testing.T) {
	ws, sess := t.TempDir(), filepath.Join(t.TempDir(), "s.jsonl")
	r := runQuill(t, fakeapi.New(t, fakeapi.TextReply("done", events.Usage{})), "look", "--workspace", ws, "--session", sess, "--sandbox", "read-only")
	if r.code != 0 {
		t.Fatalf("run 1 exit = %d; stderr: %s", r.code, r.stderr)
	}
	fake := fakeapi.New(t,
		fakeapi.ToolReply("tu1", "write", `{"path":"hello.txt","content":"hi"}`),
		fakeapi.TextReply("done", events.Usage{}),
	)
	r = runQuill(t, fake, "now write", "--workspace", ws, "--session", sess)
	if r.code != 0 {
		t.Fatalf("run 2 exit = %d; stderr: %s", r.code, r.stderr)
	}
	if r.lines[0]["sandbox"] != "read-only" {
		t.Errorf("run.started sandbox = %v", r.lines[0]["sandbox"])
	}
	if _, err := os.Stat(filepath.Join(ws, "hello.txt")); !os.IsNotExist(err) {
		t.Errorf("read-only session wrote hello.txt (stat err %v)", err)
	}
	reqs := fake.Requests()
	req := request(t, reqs[len(reqs)-1])
	res := decodeMsg(t, req.Messages[len(req.Messages)-1])
	if res.Content[0].Type != "tool_result" || res.Content[0].Content != "unknown tool: write" {
		t.Errorf("tool result = %s", req.Messages[len(req.Messages)-1])
	}
}

func TestE2EStdoutClosed(t *testing.T) {
	fake := fakeapi.New(t,
		fakeapi.ToolReply("tu1", "write", `{"path":"hello.txt","content":"hi"}`),
		fakeapi.TextReply("done", events.Usage{}),
	)
	ws, tmp := t.TempDir(), t.TempDir()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	pr.Close() // every stdout write now fails with EPIPE
	cmd := quillCmd(fake, tmp, "write hello", "exec", "--workspace", ws, "-")
	cmd.Stdout = pw
	err = cmd.Run()
	pw.Close()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		t.Fatalf("quill was killed by %v", ws.Signal())
	}
	if err != nil {
		t.Fatalf("exit: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "hello.txt")); err != nil || string(b) != "hi" {
		t.Errorf("hello.txt = %q, %v", b, err)
	}
	if entries, err := os.ReadDir(tmp); err != nil || len(entries) != 0 {
		t.Errorf("TMPDIR not empty after exit: %v, %v", entries, err)
	}
}
