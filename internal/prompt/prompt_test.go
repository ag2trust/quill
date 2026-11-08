package prompt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type blk struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
}
type msg struct {
	Role    string `json:"role"`
	Content []blk  `json:"content"`
}

func parse(t *testing.T, raw json.RawMessage) msg {
	t.Helper()
	var m msg
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Role != "user" {
		t.Fatalf("role %q", m.Role)
	}
	return m
}

func ws(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSystemStable(t *testing.T) {
	s := System()
	if s != System() || !strings.HasPrefix(s, "You are Quill,") || !strings.Contains(s, "raise_blocker") {
		t.Fatal("bad system prompt")
	}
}

func TestFirstMessageNoGuidance(t *testing.T) {
	raw, err := FirstMessage(ws(t), "do <it> & go")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"role":"user","content":[{"type":"text","text":"do <it> & go"}]}`; string(raw) != want {
		t.Fatalf("got %s", raw)
	}
}

func TestFirstMessageWithGuidance(t *testing.T) {
	w := ws(t)
	os.WriteFile(filepath.Join(w, "AGENTS.md"), []byte("agents rules"), 0o644)
	os.WriteFile(filepath.Join(w, "CLAUDE.md"), []byte("claude rules"), 0o644)
	raw, err := FirstMessage(w, "task")
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, raw)
	if len(m.Content) != 2 || m.Content[1].Text != "task" {
		t.Fatalf("blocks: %+v", m.Content)
	}
	g := m.Content[0].Text
	a := strings.Index(g, `<repository_guidance file="AGENTS.md">`+"\nagents rules\n</repository_guidance>")
	c := strings.Index(g, `<repository_guidance file="CLAUDE.md">`+"\nclaude rules\n</repository_guidance>")
	if a < 0 || c < 0 || a > c || !strings.HasPrefix(g, "Repository guidance from the workspace root follows.") {
		t.Fatalf("guidance: %q", g)
	}
	if strings.Contains(string(raw), `\u00`+`3c`) {
		t.Fatal("html escaped")
	}
}

func TestGuidanceCapped(t *testing.T) {
	w := ws(t)
	os.WriteFile(filepath.Join(w, "AGENTS.md"), []byte(strings.Repeat("x", 40000)), 0o644)
	raw, _ := FirstMessage(w, "t")
	g := parse(t, raw).Content[0].Text
	if !strings.Contains(g, "x\n[truncated]\n</repository_guidance>") || strings.Count(g, "x") != 32768 {
		t.Fatalf("not capped: %d", strings.Count(g, "x"))
	}
}

func TestGuidanceSymlinkOutsideIgnored(t *testing.T) {
	w := ws(t)
	out := filepath.Join(ws(t), "secret.md")
	os.WriteFile(out, []byte("SECRET"), 0o644)
	os.Symlink(out, filepath.Join(w, "AGENTS.md"))
	raw, _ := FirstMessage(w, "t")
	if m := parse(t, raw); len(m.Content) != 1 || strings.Contains(string(raw), "SECRET") {
		t.Fatalf("leaked: %s", raw)
	}
}

func TestGuidanceNonRegularIgnored(t *testing.T) {
	w := ws(t)
	os.Mkdir(filepath.Join(w, "AGENTS.md"), 0o755)
	raw, err := FirstMessage(w, "t")
	if err != nil || len(parse(t, raw).Content) != 1 {
		t.Fatalf("%s %v", raw, err)
	}
}

func TestResumeAfterText(t *testing.T) {
	last := json.RawMessage(`{"role":"assistant","content":[{"type":"thinking","thinking":"x"},{"type":"text","text":"hi"}]}`)
	raw, err := ResumeMessage(last, "go on")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"role":"user","content":[{"type":"text","text":"go on"}]}`; string(raw) != want {
		t.Fatalf("got %s", raw)
	}
}

func TestResumeAfterBlocker(t *testing.T) {
	last := json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"x"},{"type":"tool_use","id":"t1","name":"raise_blocker","input":{}},{"type":"tool_use","id":"t2","name":"read","input":{}}]}`)
	raw, err := ResumeMessage(last, "answer")
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, raw)
	want := []blk{{Type: "tool_result", ToolUseID: "t1", Content: "answer"}, {Type: "tool_result", ToolUseID: "t2", Content: "not run: paused"}}
	if len(m.Content) != 2 || m.Content[0] != want[0] || m.Content[1] != want[1] {
		t.Fatalf("got %s", raw)
	}
}

func TestResumeAfterInterruptedTools(t *testing.T) {
	last := json.RawMessage(`{"role":"assistant","content":[{"type":"tool_use","id":"s1","name":"shell","input":{}}]}`)
	raw, err := ResumeMessage(last, "continue")
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, raw)
	if len(m.Content) != 2 || m.Content[0] != (blk{Type: "tool_result", ToolUseID: "s1", Content: "not run: paused"}) || m.Content[1] != (blk{Type: "text", Text: "continue"}) {
		t.Fatalf("got %s", raw)
	}
}

func TestResumeAfterUserMessage(t *testing.T) {
	raw, err := ResumeMessage(json.RawMessage(`{"role":"user","content":[{"type":"text","text":"a"}]}`), "p")
	if err != nil || string(raw) != `{"role":"user","content":[{"type":"text","text":"p"}]}` {
		t.Fatalf("%s %v", raw, err)
	}
}
