package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"quill/internal/proc"
)

type shell struct {
	runner  *proc.Runner
	timeout time.Duration
}

// NewShell returns a Shell that runs commands through r, each limited to
// timeout. Output is capped at OutputCap and followed by the exit code; a
// non-zero exit is not an error, a timeout or cancellation is.
func NewShell(r *proc.Runner, timeout time.Duration) Shell {
	return &shell{runner: r, timeout: timeout}
}

func (s *shell) Run(ctx context.Context, command string) Result {
	out := NewCapped(OutputCap)
	code, timedOut, err := s.runner.Run(ctx, command, s.timeout, out)
	content := out.String()
	switch {
	case timedOut:
		return Result{Content: withMarker(content, fmt.Sprintf("[killed after %s timeout]", s.timeout)), IsError: true}
	case err != nil && ctx.Err() != nil:
		return Result{Content: withMarker(content, "[cancelled]"), IsError: true}
	case err != nil:
		return Result{Content: withMarker(content, fmt.Sprintf("[shell error: %v]", err)), IsError: true}
	}
	return Result{Content: withMarker(content, fmt.Sprintf("[exit code %d]", code))}
}

// withMarker appends marker on its own line.
func withMarker(content, marker string) string {
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return content + marker
}
