package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func searchWS(t *testing.T) (string, *Registry) {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ws, NewRegistry(ReadOnly, ws, nil)
}

func put(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runSearch(r *Registry, in string) Result {
	return r.Run(context.Background(), "search", json.RawMessage(in))
}

func TestSearchFindsLines(t *testing.T) {
	ws, r := searchWS(t)
	put(t, filepath.Join(ws, "b.txt"), "nope\nfoo two\n")
	put(t, filepath.Join(ws, "a.txt"), "foo one\nbar\nfoo three\n")
	put(t, filepath.Join(ws, "sub/c.txt"), "xx foo\n")
	res := runSearch(r, `{"pattern":"foo"}`)
	want := "a.txt:1: foo one\na.txt:3: foo three\nb.txt:2: foo two\nsub/c.txt:1: xx foo"
	if res.IsError || res.Content != want {
		t.Fatalf("got %+v, want %q", res, want)
	}
	res = runSearch(r, `{"pattern":"foo","path":"sub"}`)
	if res.Content != "sub/c.txt:1: xx foo" {
		t.Fatalf("got %+v", res)
	}
	res = runSearch(r, `{"pattern":"bar","path":"a.txt"}`)
	if res.Content != "a.txt:2: bar" {
		t.Fatalf("got %+v", res)
	}
}

func TestSearchSkipsGit(t *testing.T) {
	ws, r := searchWS(t)
	put(t, filepath.Join(ws, ".git/config"), "foo\n")
	put(t, filepath.Join(ws, "x.txt"), "foo\n")
	if res := runSearch(r, `{"pattern":"foo"}`); res.Content != "x.txt:1: foo" {
		t.Fatalf("got %+v", res)
	}
}

func TestSearchBadRegexp(t *testing.T) {
	_, r := searchWS(t)
	if res := runSearch(r, `{"pattern":"("}`); !res.IsError {
		t.Fatalf("got %+v", res)
	}
	if res := runSearch(r, `{}`); !res.IsError {
		t.Fatalf("missing pattern: %+v", res)
	}
}

func TestSearchCap(t *testing.T) {
	ws, r := searchWS(t)
	put(t, filepath.Join(ws, "m.txt"), strings.Repeat("hit\n", 250))
	res := runSearch(r, `{"pattern":"hit"}`)
	lines := strings.Split(res.Content, "\n")
	if len(lines) != 201 || lines[200] != "[stopped after 200 matches]" || lines[199] != "m.txt:200: hit" {
		t.Fatalf("got %d lines, last %q", len(lines), lines[len(lines)-1])
	}
}

func TestSearchSkipsBinaryAndLarge(t *testing.T) {
	ws, r := searchWS(t)
	put(t, filepath.Join(ws, "bin"), "foo\x00bar\n")
	put(t, filepath.Join(ws, "big"), "foo\n"+strings.Repeat("a", 1<<20))
	put(t, filepath.Join(ws, "ok"), "foo\n")
	if res := runSearch(r, `{"pattern":"foo"}`); res.Content != "ok:1: foo" {
		t.Fatalf("got %+v", res)
	}
}

func TestSearchDoesNotFollowSymlinkOut(t *testing.T) {
	ws, r := searchWS(t)
	out, _ := filepath.EvalSymlinks(t.TempDir())
	put(t, filepath.Join(out, "secret.txt"), "foo secret\n")
	if err := os.Symlink(out, filepath.Join(ws, "dirlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(out, "secret.txt"), filepath.Join(ws, "filelink")); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(ws, "in.txt"), "foo\n")
	res := runSearch(r, `{"pattern":"foo"}`)
	if res.Content != "in.txt:1: foo" {
		t.Fatalf("got %+v", res)
	}
	if res := runSearch(r, `{"pattern":"foo","path":"dirlink"}`); !res.IsError {
		t.Fatalf("start path through escaping symlink: %+v", res)
	}
}

func TestSearchSkipsFIFO(t *testing.T) {
	ws, r := searchWS(t)
	if err := syscall.Mkfifo(filepath.Join(ws, "pipe"), 0o644); err != nil {
		t.Skip(err)
	}
	put(t, filepath.Join(ws, "z.txt"), "foo\n")
	if res := runSearch(r, `{"pattern":"foo"}`); res.Content != "z.txt:1: foo" {
		t.Fatalf("got %+v", res)
	}
	if res := runSearch(r, `{"pattern":"foo","path":"pipe"}`); res.IsError {
		t.Fatalf("fifo as start path: %+v", res)
	}
}

func TestSearchCancelled(t *testing.T) {
	ws, r := searchWS(t)
	for i := 0; i < 5; i++ {
		put(t, filepath.Join(ws, fmt.Sprintf("f%d", i)), "foo\n")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if res := r.Run(ctx, "search", json.RawMessage(`{"pattern":"foo"}`)); !res.IsError {
		t.Fatalf("got %+v", res)
	}
}
