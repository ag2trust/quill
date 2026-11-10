package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func decode(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("invalid JSON %q: %v", line, err)
	}
	return m
}

func TestRunCompletedShape(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	err := w.RunCompleted(RunCompleted{Status: StatusCompleted, FinalText: "done", Steps: 2, DurationMS: 5})
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, strings.TrimSuffix(buf.String(), "\n"))
	if m["type"] != "run.completed" || m["status"] != "completed" {
		t.Fatalf("bad type/status: %v", m)
	}
	usage, ok := m["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing: %v", m)
	}
	if _, ok := usage["cache_read"]; !ok {
		t.Fatalf("usage.cache_read missing: %v", usage)
	}
	for _, k := range []string{"error", "blocker"} {
		if _, ok := m[k]; ok {
			t.Fatalf("unexpected key %q", k)
		}
	}
	if !strings.HasPrefix(buf.String(), `{"type":"run.completed"`) {
		t.Fatalf("type not first: %s", buf.String())
	}
}

func TestOnlyOneRunCompleted(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.RunCompleted(RunCompleted{Status: StatusCompleted}); err != nil {
		t.Fatal(err)
	}
	n := buf.Len()
	if err := w.RunCompleted(RunCompleted{Status: StatusBlocked}); !errors.Is(err, ErrCompleted) {
		t.Fatalf("got %v", err)
	}
	if err := w.ItemCompleted(Item{Type: "message"}); !errors.Is(err, ErrCompleted) {
		t.Fatalf("got %v", err)
	}
	if buf.Len() != n {
		t.Fatal("wrote after completion")
	}
}

func TestExitCodes(t *testing.T) {
	cases := map[Status]int{
		StatusCompleted: 0, StatusBlocked: 5, StatusLimitReached: 3,
		StatusProviderError: 4, StatusCancelled: 143, StatusInternalError: 1, StatusMCPError: 6,
	}
	for s, want := range cases {
		if got := s.ExitCode(); got != want {
			t.Errorf("%s: got %d want %d", s, got, want)
		}
	}
}

func TestOneLinePerEvent(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.RunStarted(RunStarted{Format: Format, Model: "m", Version: Version}); err != nil {
		t.Fatal(err)
	}
	if err := w.ItemStarted(Item{Type: "tool_call", ID: "1", Name: "shell", Input: json.RawMessage(`{"command":"ls"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := w.ItemCompleted(Item{Type: "message", Text: "hi\nthere"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Fatal("not newline terminated")
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines: %q", len(lines), out)
	}
	want := []string{"run.started", "item.started", "item.completed"}
	for i, l := range lines {
		m := decode(t, l)
		if m["type"] != want[i] {
			t.Errorf("line %d type %v", i, m["type"])
		}
	}
	started := decode(t, lines[0])
	if started["format"] != float64(2) {
		t.Errorf("run.started format = %v, want 2", started["format"])
	}
	if started["version"] != Version {
		t.Errorf("run.started version = %v, want %q", started["version"], Version)
	}
	if decode(t, lines[2])["item"].(map[string]any)["text"] != "hi\nthere" {
		t.Fatal("item not nested")
	}
}
