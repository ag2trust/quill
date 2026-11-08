package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newWS(t *testing.T) string {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestResolveInside(t *testing.T) {
	ws := newWS(t)
	for _, tc := range []struct{ in, want string }{
		{"a/b.txt", filepath.Join(ws, "a/b.txt")},
		{".", ws},
		{"new/dir/deep/file.txt", filepath.Join(ws, "new/dir/deep/file.txt")},
		{filepath.Join(ws, "x.txt"), filepath.Join(ws, "x.txt")},
	} {
		got, err := Resolve(ws, tc.in)
		if err != nil || got != tc.want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestResolveEscapes(t *testing.T) {
	ws := newWS(t)
	outside := newWS(t)
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink(outside, filepath.Join(ws, "link")))
	must(os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(ws, "filelink")))
	must(os.Symlink(filepath.Join(outside, "missing.txt"), filepath.Join(ws, "dangling")))
	must(os.Symlink(filepath.Join(outside, "newdir"), filepath.Join(ws, "danglingdir")))
	must(os.Symlink("../../etc/zzz-nope", filepath.Join(ws, "reldangling")))
	must(os.Symlink("loop2", filepath.Join(ws, "loop1")))
	must(os.Symlink("loop1", filepath.Join(ws, "loop2")))

	for _, p := range []string{
		"../x", "/etc/passwd", "link/secret.txt", "link", "filelink", "dangling",
		"danglingdir/f.txt", "reldangling", "a/../../x", "loop1",
		filepath.Join(ws, "link", "secret.txt"), ws + "-sibling/x",
	} {
		_, err := Resolve(ws, p)
		if err == nil || !strings.Contains(err.Error(), "path outside workspace: "+p) {
			t.Errorf("Resolve(%q) err = %v; want outside workspace", p, err)
		}
	}

	must(os.MkdirAll(filepath.Join(ws, "a"), 0o755))
	must(os.Symlink(filepath.Join(ws, "a"), filepath.Join(ws, "inner")))
	must(os.Symlink("a/future.txt", filepath.Join(ws, "okdangling")))
	if got, err := Resolve(ws, "inner/f.txt"); err != nil || got != filepath.Join(ws, "a/f.txt") {
		t.Errorf("inner symlink: %q, %v", got, err)
	}
	if got, err := Resolve(ws, "okdangling"); err != nil || got != filepath.Join(ws, "a/future.txt") {
		t.Errorf("inside dangling symlink: %q, %v", got, err)
	}
}
