package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, r *Registry, name string, in any) Result {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return r.Run(context.Background(), name, b)
}

type m map[string]any

func setup(t *testing.T) (*Registry, string) {
	ws := newWS(t)
	return NewRegistry(WorkspaceWrite, ws, nil), ws
}

func TestWriteReadRoundTrip(t *testing.T) {
	r, ws := setup(t)
	res := run(t, r, "write", m{"path": "sub/dir/a.txt", "content": "hello\n"})
	if res.IsError || !strings.HasPrefix(res.Content, "wrote 6 bytes to ") {
		t.Fatalf("%+v", res)
	}
	st, err := os.Stat(filepath.Join(ws, "sub/dir/a.txt"))
	if err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("%v %v", st, err)
	}
	if res := run(t, r, "read", m{"path": "sub/dir/a.txt"}); res.IsError || res.Content != "hello\n" {
		t.Fatalf("%+v", res)
	}
	if res := run(t, r, "write", m{"path": "sub/dir/a.txt", "content": ""}); res.IsError {
		t.Fatalf("overwrite with empty: %+v", res)
	}
	if res := run(t, r, "read", m{"path": "sub"}); !res.IsError {
		t.Fatal("directory read should error")
	}
}

func TestReadOffsetLimit(t *testing.T) {
	r, ws := setup(t)
	os.WriteFile(filepath.Join(ws, "f"), []byte("1\n2\n3\n4\n5\n"), 0o644)
	if res := run(t, r, "read", m{"path": "f", "offset": 2, "limit": 2}); res.Content != "2\n3\n" || res.IsError {
		t.Fatalf("%+v", res)
	}
	if res := run(t, r, "read", m{"path": "f", "offset": 4}); res.Content != "4\n5\n" {
		t.Fatalf("%+v", res)
	}
	if res := run(t, r, "read", m{"path": "f", "limit": 1}); res.Content != "1\n" {
		t.Fatalf("%+v", res)
	}
	if res := run(t, r, "read", m{"path": "f", "offset": 0, "limit": 1}); !res.IsError {
		t.Fatalf("offset 0 should error: %+v", res)
	}
}

func edit(t *testing.T, content string, in m) (Result, string) {
	r, ws := setup(t)
	p := filepath.Join(ws, "e.txt")
	os.WriteFile(p, []byte(content), 0o644)
	in["path"] = "e.txt"
	res := run(t, r, "edit", in)
	b, _ := os.ReadFile(p)
	return res, string(b)
}

func TestEditUnique(t *testing.T) {
	res, got := edit(t, "a b c", m{"old_text": "b", "new_text": "X"})
	if res.IsError || res.Content != "replaced 1 occurrence(s)" || got != "a X c" {
		t.Fatalf("%+v %q", res, got)
	}
}

func TestEditNotFound(t *testing.T) {
	res, got := edit(t, "abc", m{"old_text": "z", "new_text": "X"})
	if !res.IsError || !strings.HasPrefix(res.Content, "old_text not found in ") || got != "abc" {
		t.Fatalf("%+v %q", res, got)
	}
	if res, _ := edit(t, "abc", m{"old_text": "", "new_text": "X"}); !res.IsError {
		t.Fatal("empty old_text should error")
	}
}

func TestEditAmbiguous(t *testing.T) {
	res, got := edit(t, "a a", m{"old_text": "a", "new_text": "X"})
	if !res.IsError || !strings.Contains(res.Content, "matches 2 places") || got != "a a" {
		t.Fatalf("%+v %q", res, got)
	}
}

func TestEditReplaceAll(t *testing.T) {
	res, got := edit(t, "a a", m{"old_text": "a", "new_text": "X", "replace_all": true})
	if res.IsError || res.Content != "replaced 2 occurrence(s)" || got != "X X" {
		t.Fatalf("%+v %q", res, got)
	}
}

func TestListSorted(t *testing.T) {
	r, ws := setup(t)
	os.Mkdir(filepath.Join(ws, "zdir"), 0o755)
	os.WriteFile(filepath.Join(ws, "b"), nil, 0o644)
	os.WriteFile(filepath.Join(ws, "a"), nil, 0o644)
	os.WriteFile(filepath.Join(ws, "zdir", "inner"), nil, 0o644)
	if res := run(t, r, "list", m{}); res.IsError || res.Content != "a\nb\nzdir/" {
		t.Fatalf("%+v", res)
	}
	if res := run(t, r, "list", m{"path": "zdir"}); res.Content != "inner" {
		t.Fatalf("%+v", res)
	}
}

func TestSymlinkEscapeAllTools(t *testing.T) {
	r, ws := setup(t)
	outside := newWS(t)
	os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644)
	if err := os.Symlink(outside, filepath.Join(ws, "out")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		in   m
	}{
		{"read", m{"path": "out/secret"}},
		{"write", m{"path": "out/new", "content": "x"}},
		{"write", m{"path": "out/sub/new", "content": "x"}},
		{"edit", m{"path": "out/secret", "old_text": "s", "new_text": "t"}},
		{"list", m{"path": "out"}},
	}
	for _, c := range cases {
		if res := run(t, r, c.name, c.in); !res.IsError {
			t.Errorf("%s %v: %+v", c.name, c.in, res)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "secret")); string(b) != "s" {
		t.Fatal("outside file modified")
	}
	ents, _ := os.ReadDir(outside)
	if len(ents) != 1 {
		t.Fatalf("outside dir touched: %v", ents)
	}
}
