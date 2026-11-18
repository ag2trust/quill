package main

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"quill/internal/events"
	"quill/internal/fakeapi"
)

// stopBound is the longest a run may take to end after --timeout. It is the
// margin README.md tells callers to allow before their own watchdog fires.
const stopBound = 15 * time.Second

// runTimedOut runs quill with a 2s --timeout and checks it ends with
// limit_reached within stopBound after the timeout, leaving TMPDIR empty.
func runTimedOut(t *testing.T, fake *fakeapi.Server) {
	t.Helper()
	tmp := t.TempDir()
	cmd := quillCmd(fake, tmp, "run it", "exec", "--timeout", "2s", "--workspace", t.TempDir(), "-")
	start := time.Now()
	out, err := cmd.Output()
	elapsed := time.Since(start)
	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		t.Fatalf("run quill: %v", err)
	}
	code := cmd.ProcessState.ExitCode()
	lines := parseLines(t, string(out))
	checkStream(t, code, lines)
	if code != 3 {
		t.Errorf("exit = %d, want 3", code)
	}
	if l := lines[len(lines)-1]; l["status"] != "limit_reached" {
		t.Errorf("status = %v, want limit_reached", l["status"])
	}
	if elapsed > 2*time.Second+stopBound {
		t.Errorf("run took %v, want at most --timeout + %v", elapsed, stopBound)
	}
	t.Logf("ended %v after start (--timeout 2s)", elapsed)
	if entries, err := os.ReadDir(tmp); err != nil || len(entries) != 0 {
		t.Errorf("TMPDIR not empty after exit: %v, %v", entries, err)
	}
}

// A command that ignores SIGTERM is the slowest shell case: Quill must
// escalate to SIGKILL.
func TestE2ETimeoutMidShellIgnoringSigterm(t *testing.T) {
	const marker = "sleep 31.77"
	t.Cleanup(func() { exec.Command("pkill", "-f", marker).Run() })
	input, _ := json.Marshal(map[string]string{"command": "trap '' TERM; " + marker})
	runTimedOut(t, fakeapi.New(t, fakeapi.ToolReply("tu1", "shell", string(input))))
	if exec.Command("pgrep", "-f", marker).Run() == nil {
		t.Errorf("%q survived quill", marker)
	}
}

func TestE2ETimeoutMidRequest(t *testing.T) {
	r := fakeapi.TextReply("late", events.Usage{})
	r.Delay = time.Minute
	runTimedOut(t, fakeapi.New(t, r))
}

// retry-after under the 60s cap is slept inside the step; the run timeout
// still ends it.
func TestE2ETimeoutDuringRetryWait(t *testing.T) {
	runTimedOut(t, fakeapi.New(t, fakeapi.Reply{
		Status: http.StatusTooManyRequests,
		Header: http.Header{"Retry-After": {"30"}},
		Body:   `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
	}))
}
