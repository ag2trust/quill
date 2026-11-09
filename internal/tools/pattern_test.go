package tools

import (
	"reflect"
	"strings"
	"testing"
)

func TestCompileGlobMatch(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "internal/tools/a.go", true}, // no slash: name at any depth
		{"*.go", "a.gox", false},
		{"**/*_test.go", "a_test.go", true},
		{"**/*_test.go", "x/y/a_test.go", true},
		{"internal/*.go", "internal/a.go", true},
		{"internal/*.go", "internal/tools/a.go", false},
		{"internal/**/*.go", "internal/a.go", true},
		{"internal/**/*.go", "internal/tools/x/a.go", true},
		{"internal/**", "internal/tools/a.go", true},
		{"src/**/*.{ts,tsx}", "src/a/b.tsx", true},
		{"src/**/*.{ts,tsx}", "src/a/b.js", false},
		{"{cmd,internal}/**/*.go", "cmd/quill/main.go", true},
		{"*.{a,{b,c}}", "x.c", true},
		{"./cmd/*", "cmd/x", true},
		{"[ab].txt", "b.txt", true},
		{"?.txt", "ab.txt", false},
		{"**/**/*.md", "README.md", true},
	}
	for _, c := range cases {
		g, err := compileGlob(c.pat)
		if err != nil {
			t.Fatalf("%q: %v", c.pat, err)
		}
		if got := g.match(c.path); got != c.want {
			t.Errorf("%q vs %q = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}

func TestCompileGlobErrors(t *testing.T) {
	for _, p := range []string{"", "{a,b", "a}", "[", "x/[a"} {
		if _, err := compileGlob(p); err == nil {
			t.Errorf("%q: want error", p)
		}
	}
	if _, err := compileGlob(strings.Repeat("{a,b}", 7)); err == nil {
		t.Error("128 alternatives: want error")
	}
}

func TestExpandBraces(t *testing.T) {
	got, err := expandBraces("a{b,c{d,e}}f")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"abf", "acdf", "acef"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
