package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIgnoreRules(t *testing.T) {
	root := parseIgnore("", []byte("# comment\n\nnode_modules/\n*.log\n!keep.log\n/dist\nbuild/out\ndocs/**/*.tmp\n\\#hash\ntrail   \r\n"))
	sub := parseIgnore("pkg", []byte("gen.go\n/local\n"))
	rules := &ruleSet{parent: &ruleSet{rules: root}, rules: sub}
	cases := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"node_modules", true, true},
		{"web/node_modules", true, true},
		{"node_modules", false, false}, // dir-only rule
		{"a.log", false, true},
		{"x/y/a.log", false, true},
		{"keep.log", false, false}, // negated
		{"dist", true, true},
		{"web/dist", true, false}, // anchored to root
		{"build/out", false, true},
		{"x/build/out", false, false},
		{"docs/a/b.tmp", false, true},
		{"docs/b.tmp", false, true},
		{"#hash", false, true},
		{"trail", false, true},
		{"# comment", false, false},
		{"pkg/gen.go", false, true},
		{"pkg/deep/gen.go", false, true},
		{"gen.go", false, false}, // rule lives in pkg/
		{"pkg/local", true, true},
		{"pkg/x/local", true, false},
		{"main.go", false, false},
	}
	for _, c := range cases {
		if got := ignored(rules, c.rel, c.isDir); got != c.want {
			t.Errorf("%q dir=%v: got %v, want %v", c.rel, c.isDir, got, c.want)
		}
	}
}

func TestIgnoreDeeperFileWins(t *testing.T) {
	set := &ruleSet{parent: &ruleSet{rules: parseIgnore("", []byte("*.go\n"))}, rules: parseIgnore("pkg", []byte("!keep.go\n"))}
	if ignored(set, "pkg/keep.go", false) || !ignored(set, "pkg/other.go", false) || !ignored(set, "keep.go", false) {
		t.Fatal("nested negation precedence wrong")
	}
}

func TestMatchSegsManyDoubleStarsIsFast(t *testing.T) {
	pat := splitPattern(strings.Repeat("**/a/", 20) + "b")
	name := strings.Split(strings.Repeat("a/", 60)+"c", "/")
	start := time.Now()
	if matchSegs(pat, name) {
		t.Fatal("should not match")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestIgnoreRuleBudget(t *testing.T) {
	ws, r := searchWS(t)
	// 60 nested directories, each ignoring 100 names: 6,000 rules, over the cap.
	dir := ws
	var lines strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&lines, "n%d\n", i)
	}
	for i := 0; i < 60; i++ {
		dir = filepath.Join(dir, "d")
		searchPut(t, filepath.Join(dir, ".gitignore"), lines.String())
	}
	searchPut(t, filepath.Join(dir, "x.txt"), "hit\n")
	res := globRun(r, `{"pattern":"x.txt"}`)
	if res.IsError || !strings.HasSuffix(res.Content, "/x.txt") {
		t.Fatalf("got %+v", res)
	}
}

func TestIgnoreGitEdgeSemantics(t *testing.T) {
	set := &ruleSet{rules: parseIgnore("", []byte("vendor/**\n!vendor/keep.txt\n*.[!o]\nsp\\  \n"))}
	cases := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"vendor", true, false}, // foo/** matches only what is inside
		{"vendor/x.go", false, true},
		{"vendor/keep.txt", false, false},
		{"a.c", false, true}, // [!o] is a negated class
		{"a.o", false, false},
		{"sp ", false, true}, // escaped trailing space kept, the rest trimmed
	}
	for _, c := range cases {
		if got := ignored(set, c.rel, c.isDir); got != c.want {
			t.Errorf("%q dir=%v: got %v, want %v", c.rel, c.isDir, got, c.want)
		}
	}
}

func TestExcludeNotReadThroughSymlinkedGitDir(t *testing.T) {
	ws, r := searchWS(t)
	out := t.TempDir()
	searchPut(t, filepath.Join(out, "info/exclude"), "*.go\n")
	if err := os.Symlink(out, filepath.Join(ws, ".git")); err != nil {
		t.Fatal(err)
	}
	searchPut(t, filepath.Join(ws, "a.go"), "x\n")
	if res := globRun(r, `{"pattern":"*.go"}`); res.Content != "a.go" {
		t.Fatalf("got %+v", res)
	}
}

func TestGlobLengthCap(t *testing.T) {
	if _, err := compileGlob(strings.Repeat("{a}", 10000)); err == nil {
		t.Fatal("want error for a 30KB pattern")
	}
}
