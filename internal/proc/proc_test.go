package proc

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func newRunner(t *testing.T) *Runner {
	t.Helper()
	r := NewRunner(t.TempDir(), []string{"PATH=/usr/bin:/bin"})
	t.Cleanup(func() { r.KillAll(time.Second) })
	return r
}

// gone polls until the process group no longer exists.
func gone(t *testing.T, pgid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process group %d still exists (kill 0: %v)", pgid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func onlyGroup(t *testing.T, r *Runner) int {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.groups) != 1 {
		t.Fatalf("recorded groups = %v, want exactly one", r.groups)
	}
	for g := range r.groups {
		return g
	}
	return 0
}

func firstLinePgid(t *testing.T, out string) int {
	t.Helper()
	line, _, _ := strings.Cut(out, "\n")
	pgid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pgid <= 1 {
		t.Fatalf("no pgid in output %q", out)
	}
	return pgid
}

func TestRunExitCode(t *testing.T) {
	r := newRunner(t)
	code, timedOut, err := r.Run(context.Background(), "exit 3", time.Minute, &bytes.Buffer{})
	if code != 3 || timedOut || err != nil {
		t.Fatalf("got (%d, %v, %v), want (3, false, nil)", code, timedOut, err)
	}
}

func TestRunOutputDirAndStdin(t *testing.T) {
	dir := t.TempDir()
	r := NewRunner(dir, []string{"PATH=/usr/bin:/bin", "FOO=bar"})
	var out bytes.Buffer
	code, _, err := r.Run(context.Background(), `echo out; echo err >&2; echo "$FOO"; cat; pwd -P`, time.Minute, &out)
	if code != 0 || err != nil {
		t.Fatalf("got (%d, %v)", code, err)
	}
	got := out.String()
	for _, want := range []string{"out\n", "err\n", "bar\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q lacks %q", got, want)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(got), strings.TrimPrefix(dir, "/private")) {
		t.Errorf("output %q does not end with dir %q", got, dir)
	}
}

func TestRunSignaledExitCode(t *testing.T) {
	r := newRunner(t)
	code, _, err := r.Run(context.Background(), "kill -KILL $$", time.Minute, &bytes.Buffer{})
	if code != 128+int(syscall.SIGKILL) || err != nil {
		t.Fatalf("got (%d, %v), want (137, nil)", code, err)
	}
}

func TestRunNormalExitForgetsEmptyGroup(t *testing.T) {
	r := newRunner(t)
	if _, _, err := r.Run(context.Background(), "true", time.Minute, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	n := len(r.groups)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("recorded groups = %d after clean exit, want 0", n)
	}
}

func TestRunTimeoutKillsGroup(t *testing.T) {
	r := newRunner(t)
	var out bytes.Buffer
	start := time.Now()
	code, timedOut, err := r.Run(context.Background(), "echo $$; sleep 30 & sleep 30", 300*time.Millisecond, &out)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %v", elapsed)
	}
	if !timedOut || err != nil {
		t.Fatalf("got (%d, %v, %v), want timedOut", code, timedOut, err)
	}
	gone(t, firstLinePgid(t, out.String()))
}

func TestRunTimeoutSigkillsAfterGrace(t *testing.T) {
	r := newRunner(t)
	var out bytes.Buffer
	start := time.Now()
	_, timedOut, _ := r.Run(context.Background(), `trap "" TERM; echo $$; while :; do sleep 0.05; done`, 200*time.Millisecond, &out)
	elapsed := time.Since(start)
	if !timedOut {
		t.Fatal("not timed out")
	}
	if elapsed < 2*time.Second || elapsed > 5*time.Second {
		t.Fatalf("took %v, want about 2.2s (SIGTERM ignored, SIGKILL after 2s)", elapsed)
	}
	gone(t, firstLinePgid(t, out.String()))
}

func TestRunCancelKillsGroup(t *testing.T) {
	r := newRunner(t)
	var out syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for !strings.Contains(out.String(), "\n") {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	_, timedOut, err := r.Run(ctx, "echo $$; sleep 30 & sleep 30", time.Minute, &out)
	if timedOut || !errors.Is(err, context.Canceled) {
		t.Fatalf("got (%v, %v), want context.Canceled", timedOut, err)
	}
	gone(t, firstLinePgid(t, out.String()))
}

func TestRunAlreadyCancelled(t *testing.T) {
	r := newRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	if _, _, err := r.Run(ctx, "echo ran", time.Minute, &out); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if out.Len() != 0 {
		t.Fatalf("command ran: %q", out.String())
	}
}

func TestBackgroundDoesNotBlock(t *testing.T) {
	r := newRunner(t)
	var out bytes.Buffer
	start := time.Now()
	code, timedOut, err := r.Run(context.Background(), "sleep 600 & echo hi", time.Minute, &out)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %v", elapsed)
	}
	if code != 0 || timedOut || err != nil || out.String() != "hi\n" {
		t.Fatalf("got (%d, %v, %v, %q), want (0, false, nil, \"hi\\n\")", code, timedOut, err, out.String())
	}
	pgid := onlyGroup(t, r)
	if err := syscall.Kill(-pgid, 0); err != nil {
		t.Fatalf("background sleep should survive until KillAll: %v", err)
	}
	r.KillAll(time.Second)
	gone(t, pgid)
	r.KillAll(time.Second) // safe twice
}

func TestKillAllSigkillsSurvivors(t *testing.T) {
	r := newRunner(t)
	if _, _, err := r.Run(context.Background(), `(trap "" TERM; while :; do sleep 0.05; done) >/dev/null 2>&1 &`, time.Minute, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	pgid := onlyGroup(t, r)
	start := time.Now()
	r.KillAll(300 * time.Millisecond)
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("KillAll took %v", elapsed)
	}
	gone(t, pgid)
}

func TestKillAllDuringRun(t *testing.T) {
	r := newRunner(t)
	var out syncBuffer
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !strings.Contains(out.String(), "\n") {
			time.Sleep(10 * time.Millisecond)
		}
		r.KillAll(time.Second)
	}()
	start := time.Now()
	code, _, err := r.Run(context.Background(), "echo $$; sleep 30 & sleep 30", time.Minute, &out)
	wg.Wait()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %v", elapsed)
	}
	if err != nil || code != 128+int(syscall.SIGTERM) {
		t.Fatalf("got (%d, %v), want (143, nil)", code, err)
	}
	gone(t, firstLinePgid(t, out.String()))
}

func TestSignalGuards(t *testing.T) {
	for _, pgid := range []int{-5, 0, 1, syscall.Getpgrp()} {
		if signalGroup(pgid, 0) {
			t.Errorf("signalGroup(%d) was sent", pgid)
		}
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
