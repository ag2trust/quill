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
	"unicode/utf8"
)

func searchWS(t *testing.T) (string, *Registry) {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ws, NewRegistry(ReadOnly, ws, nil)
}

func searchPut(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func searchRun(r *Registry, in string) Result {
	return r.Run(context.Background(), "search", json.RawMessage(in))
}

func TestSearchFindsLines(t *testing.T) {
	ws, r := searchWS(t)
	searchPut(t, filepath.Join(ws, "b.txt"), "nope\nfoo two\n")
	searchPut(t, filepath.Join(ws, "a.txt"), "foo one\nbar\nfoo three\n")
	searchPut(t, filepath.Join(ws, "sub/c.txt"), "xx foo\n")
	res := searchRun(r, `{"pattern":"foo"}`)
	want := "a.txt:1: foo one\na.txt:3: foo three\nb.txt:2: foo two\nsub/c.txt:1: xx foo"
	if res.IsError || res.Content != want {
		t.Fatalf("got %+v, want %q", res, want)
	}
	res = searchRun(r, `{"pattern":"foo","path":"sub"}`)
	if res.Content != "sub/c.txt:1: xx foo" {
		t.Fatalf("got %+v", res)
	}
	res = searchRun(r, `{"pattern":"bar","path":"a.txt"}`)
	if res.Content != "a.txt:2: bar" {
		t.Fatalf("got %+v", res)
	}
}

func TestSearchSkipsGit(t *testing.T) {
	ws, r := searchWS(t)
	searchPut(t, filepath.Join(ws, ".git/config"), "foo\n")
	searchPut(t, filepath.Join(ws, "x.txt"), "foo\n")
	if res := searchRun(r, `{"pattern":"foo"}`); res.Content != "x.txt:1: foo" {
		t.Fatalf("got %+v", res)
	}
}

func TestSearchBadRegexp(t *testing.T) {
	_, r := searchWS(t)
	if res := searchRun(r, `{"pattern":"("}`); !res.IsError {
		t.Fatalf("got %+v", res)
	}
	if res := searchRun(r, `{}`); !res.IsError {
		t.Fatalf("missing pattern: %+v", res)
	}
}

func TestSearchCap(t *testing.T) {
	ws, r := searchWS(t)
	searchPut(t, filepath.Join(ws, "m.txt"), strings.Repeat("hit\n", 250))
	res := searchRun(r, `{"pattern":"hit"}`)
	lines := strings.Split(res.Content, "\n")
	if len(lines) != 201 || lines[200] != "[stopped after 200 matches]" || lines[199] != "m.txt:200: hit" {
		t.Fatalf("got %d lines, last %q", len(lines), lines[len(lines)-1])
	}
}

func TestSearchSkipsBinaryAndLarge(t *testing.T) {
	ws, r := searchWS(t)
	searchPut(t, filepath.Join(ws, "bin"), "foo\x00bar\n")
	searchPut(t, filepath.Join(ws, "big"), "foo\n"+strings.Repeat("a", 1<<20))
	searchPut(t, filepath.Join(ws, "ok"), "foo\n")
	if res := searchRun(r, `{"pattern":"foo"}`); res.Content != "ok:1: foo" {
		t.Fatalf("got %+v", res)
	}
}

func TestSearchDoesNotFollowSymlinkOut(t *testing.T) {
	ws, r := searchWS(t)
	out, _ := filepath.EvalSymlinks(t.TempDir())
	searchPut(t, filepath.Join(out, "secret.txt"), "foo secret\n")
	if err := os.Symlink(out, filepath.Join(ws, "dirlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(out, "secret.txt"), filepath.Join(ws, "filelink")); err != nil {
		t.Fatal(err)
	}
	searchPut(t, filepath.Join(ws, "in.txt"), "foo\n")
	res := searchRun(r, `{"pattern":"foo"}`)
	if res.Content != "in.txt:1: foo" {
		t.Fatalf("got %+v", res)
	}
	if res := searchRun(r, `{"pattern":"foo","path":"dirlink"}`); !res.IsError {
		t.Fatalf("start path through escaping symlink: %+v", res)
	}
}

func TestSearchSkipsFIFO(t *testing.T) {
	ws, r := searchWS(t)
	if err := syscall.Mkfifo(filepath.Join(ws, "pipe"), 0o644); err != nil {
		t.Skip(err)
	}
	searchPut(t, filepath.Join(ws, "z.txt"), "foo\n")
	if res := searchRun(r, `{"pattern":"foo"}`); res.Content != "z.txt:1: foo" {
		t.Fatalf("got %+v", res)
	}
	if res := searchRun(r, `{"pattern":"foo","path":"pipe"}`); res.IsError {
		t.Fatalf("fifo as start path: %+v", res)
	}
}

func TestSearchCancelled(t *testing.T) {
	ws, r := searchWS(t)
	for i := 0; i < 5; i++ {
		searchPut(t, filepath.Join(ws, fmt.Sprintf("f%d", i)), "foo\n")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if res := r.Run(ctx, "search", json.RawMessage(`{"pattern":"foo"}`)); !res.IsError {
		t.Fatalf("got %+v", res)
	}
}

func TestSearchExactCapNoMarker(t *testing.T) {
	ws, r := searchWS(t)
	searchPut(t, filepath.Join(ws, "m.txt"), strings.Repeat("hit\n", 200))
	res := searchRun(r, `{"pattern":"hit"}`)
	if strings.Contains(res.Content, "stopped") || len(strings.Split(res.Content, "\n")) != 200 {
		t.Fatalf("got %d lines", len(strings.Split(res.Content, "\n")))
	}
}

func TestSearchCapAcrossFiles(t *testing.T) {
	ws, r := searchWS(t)
	for i := 0; i < 5; i++ {
		searchPut(t, filepath.Join(ws, fmt.Sprintf("f%d.txt", i)), strings.Repeat("hit\n", 50))
	}
	res := searchRun(r, `{"pattern":"hit"}`)
	lines := strings.Split(res.Content, "\n")
	if res.IsError || len(lines) != 201 || lines[200] != "[stopped after 200 matches]" {
		t.Fatalf("got %+v lines=%d", res.IsError, len(lines))
	}
}

func TestSearchClipsLongLines(t *testing.T) {
	ws, r := searchWS(t)
	searchPut(t, filepath.Join(ws, "a.txt"), "x"+strings.Repeat("é", 1000)+"\n")
	res := searchRun(r, `{"pattern":"x"}`)
	if !strings.HasSuffix(res.Content, "…") || len(res.Content) > len("a.txt:1: ")+searchMaxLine+len("…") || !utf8.ValidString(res.Content) {
		t.Fatalf("got len %d", len(res.Content))
	}
}

func TestSearchMissingStartPath(t *testing.T) {
	_, r := searchWS(t)
	if res := searchRun(r, `{"pattern":"x","path":"nope"}`); !res.IsError {
		t.Fatalf("got %+v", res)
	}
}

func globRun(r *Registry, in string) Result {
	return r.Run(context.Background(), "glob", json.RawMessage(in))
}

// ignoreWS builds a workspace with ignore files at several levels.
func ignoreWS(t *testing.T) (string, *Registry) {
	t.Helper()
	ws, r := searchWS(t)
	searchPut(t, filepath.Join(ws, ".gitignore"), "node_modules/\n*.log\n!keep.log\n")
	searchPut(t, filepath.Join(ws, ".git/info/exclude"), "secret.txt\n")
	searchPut(t, filepath.Join(ws, "main.go"), "foo\n")
	searchPut(t, filepath.Join(ws, "a.log"), "foo\n")
	searchPut(t, filepath.Join(ws, "keep.log"), "foo\n")
	searchPut(t, filepath.Join(ws, "secret.txt"), "foo\n")
	searchPut(t, filepath.Join(ws, "node_modules/x/index.js"), "foo\n")
	searchPut(t, filepath.Join(ws, "pkg/.gitignore"), "gen.go\n")
	searchPut(t, filepath.Join(ws, "pkg/gen.go"), "foo\n")
	searchPut(t, filepath.Join(ws, "pkg/lib.go"), "foo\n")
	return ws, r
}

func TestSearchRespectsIgnoreFiles(t *testing.T) {
	_, r := ignoreWS(t)
	res := searchRun(r, `{"pattern":"foo"}`)
	want := "keep.log:1: foo\nmain.go:1: foo\npkg/lib.go:1: foo"
	if res.IsError || res.Content != want {
		t.Fatalf("got %+v, want %q", res, want)
	}
	// Starting inside pkg still applies the root and pkg rules.
	if res := searchRun(r, `{"pattern":"foo","path":"pkg"}`); res.Content != "pkg/lib.go:1: foo" {
		t.Fatalf("pkg: %+v", res)
	}
	// An explicit path into an ignored directory is searched.
	if res := searchRun(r, `{"pattern":"foo","path":"node_modules"}`); res.Content != "node_modules/x/index.js:1: foo" {
		t.Fatalf("explicit: %+v", res)
	}
	if res := searchRun(r, `{"pattern":"foo","path":"a.log"}`); res.Content != "a.log:1: foo" {
		t.Fatalf("explicit file: %+v", res)
	}
}

func TestSearchGlobAndFilesMode(t *testing.T) {
	ws, r := ignoreWS(t)
	searchPut(t, filepath.Join(ws, "pkg/lib.go"), "foo\nfoo again\n")
	if res := searchRun(r, `{"pattern":"foo","glob":"*.go"}`); res.Content != "main.go:1: foo\npkg/lib.go:1: foo\npkg/lib.go:2: foo again" {
		t.Fatalf("glob: %+v", res)
	}
	if res := searchRun(r, `{"pattern":"foo","glob":"pkg/*.go","output_mode":"files"}`); res.Content != "pkg/lib.go" {
		t.Fatalf("files: %+v", res)
	}
	// glob is relative to path.
	if res := searchRun(r, `{"pattern":"foo","path":"pkg","glob":"lib.go","output_mode":"files"}`); res.Content != "pkg/lib.go" {
		t.Fatalf("relative glob: %+v", res)
	}
	if res := searchRun(r, `{"pattern":"foo","output_mode":"lines"}`); !res.IsError {
		t.Fatalf("bad mode: %+v", res)
	}
	if res := searchRun(r, `{"pattern":"foo","glob":"{a"}`); !res.IsError {
		t.Fatalf("bad glob: %+v", res)
	}
}

func TestSearchFilesModeCap(t *testing.T) {
	ws, r := searchWS(t)
	for i := 0; i < 205; i++ {
		searchPut(t, filepath.Join(ws, fmt.Sprintf("f%03d.txt", i)), "hit\nhit\n")
	}
	res := searchRun(r, `{"pattern":"hit","output_mode":"files"}`)
	lines := strings.Split(res.Content, "\n")
	if len(lines) != 201 || lines[200] != "[stopped after 200 files]" || lines[0] != "f000.txt" {
		t.Fatalf("got %d lines, last %q", len(lines), lines[len(lines)-1])
	}
}

func TestGlobTool(t *testing.T) {
	ws, r := ignoreWS(t)
	searchPut(t, filepath.Join(ws, "pkg/lib_test.go"), "x\n")
	if res := globRun(r, `{"pattern":"**/*.go"}`); res.IsError || res.Content != "main.go\npkg/lib.go\npkg/lib_test.go" {
		t.Fatalf("all go: %+v", res)
	}
	if res := globRun(r, `{"pattern":"*_test.go"}`); res.Content != "pkg/lib_test.go" {
		t.Fatalf("by name: %+v", res)
	}
	if res := globRun(r, `{"pattern":"*.{log,txt}"}`); res.Content != "keep.log" {
		t.Fatalf("braces: %+v", res)
	}
	if res := globRun(r, `{"pattern":"*.go","path":"pkg"}`); res.Content != "pkg/lib.go\npkg/lib_test.go" {
		t.Fatalf("path: %+v", res)
	}
	if res := globRun(r, `{"pattern":"*.py"}`); res.IsError || res.Content != "no files" {
		t.Fatalf("none: %+v", res)
	}
	for _, in := range []string{`{}`, `{"pattern":"["}`, `{"pattern":"*","path":"../x"}`, `{"pattern":"*","extra":1}`} {
		if res := globRun(r, in); !res.IsError {
			t.Fatalf("%s: %+v", in, res)
		}
	}
}

func TestGlobSkipsSymlinksAndGit(t *testing.T) {
	ws, r := searchWS(t)
	searchPut(t, filepath.Join(ws, ".git/HEAD"), "x\n")
	searchPut(t, filepath.Join(ws, "real.txt"), "x\n")
	out := t.TempDir()
	searchPut(t, filepath.Join(out, "outside.txt"), "x\n")
	if err := os.Symlink(out, filepath.Join(ws, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "real.txt"), filepath.Join(ws, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if res := globRun(r, `{"pattern":"**"}`); res.Content != "real.txt" {
		t.Fatalf("got %+v", res)
	}
}

func TestIgnoreFileSymlinkNotFollowed(t *testing.T) {
	ws, r := searchWS(t)
	out := t.TempDir()
	searchPut(t, filepath.Join(out, "ig"), "*.go\n")
	if err := os.Symlink(filepath.Join(out, "ig"), filepath.Join(ws, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	searchPut(t, filepath.Join(ws, "a.go"), "x\n")
	if res := globRun(r, `{"pattern":"*.go"}`); res.Content != "a.go" {
		t.Fatalf("got %+v", res)
	}
}

func TestGlobCap(t *testing.T) {
	ws, r := searchWS(t)
	for i := 0; i < 505; i++ {
		searchPut(t, filepath.Join(ws, fmt.Sprintf("f%03d.txt", i)), "")
	}
	lines := strings.Split(globRun(r, `{"pattern":"*.txt"}`).Content, "\n")
	if len(lines) != 501 || lines[500] != "[stopped after 500 files]" {
		t.Fatalf("got %d lines, last %q", len(lines), lines[len(lines)-1])
	}
}
