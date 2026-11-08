package tools

import "testing"

func TestIgnoreRules(t *testing.T) {
	root := parseIgnore("", []byte("# comment\n\nnode_modules/\n*.log\n!keep.log\n/dist\nbuild/out\ndocs/**/*.tmp\n\\#hash\ntrail   \r\n"))
	sub := parseIgnore("pkg", []byte("gen.go\n/local\n"))
	rules := append(root, sub...)
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
