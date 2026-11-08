// Package proc runs shell commands in their own process groups and stops
// every group it started on request.
package proc

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// killGrace is how long a group gets between SIGTERM and SIGKILL when a
// command times out or is cancelled.
const killGrace = 2 * time.Second

// waitDelay bounds how long Run waits for output after the shell exits, so a
// background child holding the output pipe cannot block it.
const waitDelay = 2 * time.Second

// Runner runs commands in dir with env, each in a new process group. Groups
// that may still have live members are recorded until KillAll.
type Runner struct {
	dir string
	env []string

	mu     sync.Mutex
	groups map[int]struct{}
}

// NewRunner returns a Runner for dir and env.
func NewRunner(dir string, env []string) *Runner {
	return &Runner{dir: dir, env: env, groups: map[int]struct{}{}}
}

// Run executes /bin/sh -c command with stdin from /dev/null and stdout and
// stderr both written to out. When timeout (if positive) elapses or ctx is
// done, the group gets SIGTERM, then SIGKILL after 2 s. timedOut reports the
// timeout; a cancelled ctx is returned as err. A command killed by a signal
// reports exit code 128+signal; exitCode is -1 when no code is available.
func (r *Runner) Run(ctx context.Context, command string, timeout time.Duration, out io.Writer) (exitCode int, timedOut bool, err error) {
	if err := ctx.Err(); err != nil {
		return -1, false, err
	}
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = r.dir
	cmd.Env = r.env
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay

	// Start and record under the lock so a concurrent KillAll cannot miss
	// the new group.
	r.mu.Lock()
	r.pruneLocked()
	if err := cmd.Start(); err != nil {
		r.mu.Unlock()
		return -1, false, err
	}
	pgid := cmd.Process.Pid
	r.groups[pgid] = struct{}{}
	r.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}

	var waitErr error
	select {
	case waitErr = <-done:
	case <-timer:
		timedOut = true
	case <-ctx.Done():
		err = ctx.Err()
	}
	if timedOut || err != nil {
		signalGroup(pgid, syscall.SIGTERM)
		waitGone(pgid, killGrace)
		signalGroup(pgid, syscall.SIGKILL)
		<-done
		waitErr = nil
	}

	r.mu.Lock()
	if !groupAlive(pgid) {
		delete(r.groups, pgid)
	}
	r.mu.Unlock()

	exitCode = -1
	if ps := cmd.ProcessState; ps != nil {
		exitCode = ps.ExitCode()
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			exitCode = 128 + int(ws.Signal())
		}
	}
	var exitErr *exec.ExitError
	if err == nil && waitErr != nil && !errors.As(waitErr, &exitErr) && !errors.Is(waitErr, exec.ErrWaitDelay) {
		err = waitErr
	}
	return exitCode, timedOut, err
}

// KillAll sends SIGTERM to every recorded group, waits up to grace for them
// to exit, and sends SIGKILL to survivors. Safe to call more than once and
// concurrently with Run.
func (r *Runner) KillAll(grace time.Duration) {
	r.mu.Lock()
	pgids := make([]int, 0, len(r.groups))
	for pgid := range r.groups {
		pgids = append(pgids, pgid)
	}
	clear(r.groups)
	r.mu.Unlock()

	if len(pgids) == 0 {
		return
	}
	for _, pgid := range pgids {
		signalGroup(pgid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(grace)
	for _, pgid := range pgids {
		waitGone(pgid, time.Until(deadline))
	}
	for _, pgid := range pgids {
		signalGroup(pgid, syscall.SIGKILL)
	}
}

// pruneLocked forgets groups with no live members. r.mu must be held.
func (r *Runner) pruneLocked() {
	for pgid := range r.groups {
		if !groupAlive(pgid) {
			delete(r.groups, pgid)
		}
	}
}

// safeGroup reports whether pgid may be signalled: never 0, 1, or our own
// group.
func safeGroup(pgid int) bool {
	return pgid > 1 && pgid != syscall.Getpgrp()
}

// signalGroup sends sig to process group pgid and reports whether it was
// delivered.
func signalGroup(pgid int, sig syscall.Signal) bool {
	if !safeGroup(pgid) {
		return false
	}
	return syscall.Kill(-pgid, sig) == nil
}

// groupAlive reports whether process group pgid still has members.
func groupAlive(pgid int) bool {
	if !safeGroup(pgid) {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// waitGone polls until group pgid has no members or d has elapsed.
func waitGone(pgid int, d time.Duration) {
	deadline := time.Now().Add(d)
	for groupAlive(pgid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
}
