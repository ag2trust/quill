package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func names(t *testing.T, defs json.RawMessage) []string {
	t.Helper()
	var arr []struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Schema      map[string]any `json:"input_schema"`
	}
	if err := json.Unmarshal(defs, &arr); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range arr {
		if d.Description == "" || d.Schema["additionalProperties"] != false {
			t.Errorf("tool %s: bad description or schema", d.Name)
		}
		out = append(out, d.Name)
	}
	return out
}

func TestDefinitionsStable(t *testing.T) {
	a := NewRegistry(ReadOnly, "/x", nil).Definitions()
	b := NewRegistry(ReadOnly, "/y", nil).Definitions()
	if !bytes.Equal(a, b) {
		t.Fatal("definitions differ")
	}
	if got := names(t, a); !reflect.DeepEqual(got, []string{"read", "list", "glob", "search", "raise_blocker"}) {
		t.Fatalf("read-only names: %v", got)
	}
	w := NewRegistry(WorkspaceWrite, "/x", nil).Definitions()
	want := []string{"read", "write", "edit", "list", "glob", "search", "shell", "raise_blocker"}
	if got := names(t, w); !reflect.DeepEqual(got, want) {
		t.Fatalf("write names: %v", got)
	}
	if !bytes.Equal(w, NewRegistry(WorkspaceWrite, "/z", nil).Definitions()) {
		t.Fatal("write definitions differ")
	}
}

func TestReadOnlyRefusesWrite(t *testing.T) {
	ws := newWS(t)
	r := NewRegistry(ReadOnly, ws, nil)
	res := r.Run(context.Background(), "write", json.RawMessage(`{"path":"a.txt","content":"x"}`))
	if !res.IsError || res.Content != "unknown tool: write" {
		t.Fatalf("got %+v", res)
	}
	if _, err := os.Stat(filepath.Join(ws, "a.txt")); err == nil {
		t.Fatal("file created")
	}
}

func TestUnknownField(t *testing.T) {
	r := NewRegistry(WorkspaceWrite, newWS(t), nil)
	if res := r.Run(context.Background(), "read", json.RawMessage(`{"path":"a","bogus":1}`)); !res.IsError {
		t.Fatalf("got %+v", res)
	}
}

func TestMissingRequired(t *testing.T) {
	r := NewRegistry(WorkspaceWrite, newWS(t), nil)
	for _, c := range [][2]string{{"read", `{}`}, {"write", `{"path":"a"}`}, {"edit", `{"path":"a","old_text":"x"}`}} {
		if res := r.Run(context.Background(), c[0], json.RawMessage(c[1])); !res.IsError {
			t.Errorf("%s: %+v", c[0], res)
		}
	}
}

func TestBlockerAndNilShell(t *testing.T) {
	r := NewRegistry(WorkspaceWrite, newWS(t), nil)
	res := r.Run(context.Background(), BlockerTool, json.RawMessage(`{}`))
	if !res.IsError || res.Content != "raise_blocker is handled by the run loop" {
		t.Fatalf("%+v", res)
	}
	if res := r.Run(context.Background(), "shell", json.RawMessage(`{"command":"ls"}`)); !res.IsError {
		t.Fatalf("nil shell: %+v", res)
	}
}

type fakeShell struct{ got string }

func (f *fakeShell) Run(_ context.Context, c string) Result { f.got = c; return Result{Content: "ok"} }

func TestShellDispatch(t *testing.T) {
	fs := &fakeShell{}
	r := NewRegistry(WorkspaceWrite, newWS(t), fs)
	res := r.Run(context.Background(), "shell", json.RawMessage(`{"command":"echo hi"}`))
	if res.IsError || res.Content != "ok" || fs.got != "echo hi" {
		t.Fatalf("%+v %q", res, fs.got)
	}
}

func TestDecodeBlocker(t *testing.T) {
	b, err := DecodeBlocker(json.RawMessage(`{"reason":"unsafe","summary":"s","details":"d","question":"q"}`))
	if err != nil || b.Reason != "unsafe" || b.Question != "q" {
		t.Fatalf("%+v %v", b, err)
	}
	for _, in := range []string{`{"reason":"nope","summary":"s","details":"d","question":"q"}`, `{"reason":"unsafe"}`, `{"reason":"unsafe","summary":"s","details":"d","question":"q","x":1}`, `x`} {
		if _, err := DecodeBlocker(json.RawMessage(in)); err == nil {
			t.Errorf("expected error for %s", in)
		}
	}
}

func TestReadOnlyRefusesEditAndShell(t *testing.T) {
	r := NewRegistry(ReadOnly, newWS(t), &fakeShell{})
	for _, c := range [][2]string{{"edit", `{"path":"a","old_text":"x","new_text":"y"}`}, {"shell", `{"command":"ls"}`}} {
		res := r.Run(context.Background(), c[0], json.RawMessage(c[1]))
		if !res.IsError || res.Content != "unknown tool: "+c[0] {
			t.Errorf("%s: %+v", c[0], res)
		}
	}
}

func TestTrailingData(t *testing.T) {
	r := NewRegistry(WorkspaceWrite, newWS(t), nil)
	if res := r.Run(context.Background(), "list", json.RawMessage(`{"path":"a"}}`)); !res.IsError {
		t.Fatalf("%+v", res)
	}
}

func TestDefinitionsCopy(t *testing.T) {
	r := NewRegistry(ReadOnly, "/x", nil)
	a := r.Definitions()
	a[0] = 'X'
	if r.Definitions()[0] != '[' {
		t.Fatal("Definitions exposes internal slice")
	}
}

// Changing tool definitions invalidates every cached session prefix; update these hashes deliberately.
func TestDefinitionsGolden(t *testing.T) {
	for p, want := range map[Profile]string{WorkspaceWrite: "9a8af1d118dffb904a6162b08c63a7700ad476f9c07687d5f8a8c8f74714cbc1", ReadOnly: "cbe82b833c847f7d429892da6009c7844e33fcb5f7377d0e498ed6081fc8f549"} {
		sum := sha256.Sum256(NewRegistry(p, "/x", nil).Definitions())
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s: sha256 %s, want %s", p, got, want)
		}
	}
}
