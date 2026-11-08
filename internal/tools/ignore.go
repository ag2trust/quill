package tools

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Ignore rules follow .gitignore semantics for the files Quill reads itself:
// .git/info/exclude and every .gitignore inside the workspace. Git is never
// run: a hostile repository's config could make git execute commands in this
// process, which holds the API key. Global excludes are not read.

const maxIgnoreFile = 1 << 20

type ignoreRule struct {
	base    string // workspace-relative directory of the ignore file; "" for the root
	segs    []string
	negate  bool
	dirOnly bool
}

// parseIgnore parses ignore-file data whose patterns are relative to base.
func parseIgnore(base string, data []byte) []ignoreRule {
	var rules []ignoreRule
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if !strings.HasSuffix(line, `\ `) {
			line = strings.TrimRight(line, " ")
		}
		if line == "" || line[0] == '#' {
			continue
		}
		r := ignoreRule{base: base}
		switch {
		case line[0] == '!':
			r.negate = true
			line = line[1:]
		case strings.HasPrefix(line, `\!`), strings.HasPrefix(line, `\#`):
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly = true
			line = strings.TrimRight(line, "/")
		}
		if line == "" {
			continue
		}
		if strings.Contains(line, "/") {
			line = strings.TrimPrefix(line, "/") // anchored to base
		} else {
			line = "**/" + line // a bare name matches at any depth
		}
		r.segs = splitPattern(line)
		if !validSegments(r.segs) {
			continue
		}
		rules = append(rules, r)
	}
	return rules
}

// ignored reports whether rel (workspace-relative, slash-separated) is ignored.
// The last matching rule wins.
func ignored(rules []ignoreRule, rel string, isDir bool) bool {
	out := false
	for _, r := range rules {
		if r.dirOnly && !isDir {
			continue
		}
		sub := rel
		if r.base != "" {
			if !strings.HasPrefix(rel, r.base+"/") {
				continue
			}
			sub = rel[len(r.base)+1:]
		}
		if matchSegs(r.segs, strings.Split(sub, "/")) {
			out = !r.negate
		}
	}
	return out
}

// loadIgnore reads an ignore file if it is a small regular file; otherwise nil.
func loadIgnore(p, base string) []ignoreRule {
	f, fi, err := openRegular(p, p, os.O_RDONLY, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	if fi.Size() > maxIgnoreFile {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(f, maxIgnoreFile))
	if err != nil {
		return nil
	}
	return parseIgnore(base, data)
}

// withDirIgnore returns parent plus the rules of the directory's .gitignore,
// without aliasing parent.
func withDirIgnore(parent []ignoreRule, dirPath, rel string) []ignoreRule {
	own := loadIgnore(filepath.Join(dirPath, ".gitignore"), rel)
	if len(own) == 0 {
		return parent
	}
	return append(parent[:len(parent):len(parent)], own...)
}

// startRules returns the rules in force inside dir, a workspace-relative directory.
func startRules(ws, dir string) []ignoreRule {
	rules := loadIgnore(filepath.Join(ws, ".git", "info", "exclude"), "")
	rules = withDirIgnore(rules, ws, "")
	if dir == "" {
		return rules
	}
	parts := strings.Split(dir, "/")
	for i := range parts {
		rel := strings.Join(parts[:i+1], "/")
		rules = withDirIgnore(rules, filepath.Join(ws, filepath.FromSlash(rel)), rel)
	}
	return rules
}

// walkFiles calls visit for each regular file under start, in lexical order,
// with its absolute path and workspace-relative slash path. It skips .git
// directories, symlinks, non-regular files, and ignored paths. start itself
// is never skipped, so an explicit path into an ignored directory still works.
func walkFiles(ctx context.Context, ws, start string, visit func(path, rel string) error) error {
	relOf := func(p string) (string, bool) {
		rel, err := filepath.Rel(ws, p)
		if err != nil {
			return "", false
		}
		if rel == "." {
			return "", true
		}
		return filepath.ToSlash(rel), true
	}
	rules := map[string][]ignoreRule{}
	return filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			if p == start {
				return err
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, ok := relOf(p)
		if !ok {
			return nil
		}
		if p == start {
			if d.IsDir() {
				rules[rel] = startRules(ws, rel)
				return nil
			}
			if d.Type().IsRegular() {
				return visit(p, rel)
			}
			return nil
		}
		parent, _ := relOf(filepath.Dir(p))
		if d.IsDir() {
			if d.Name() == ".git" || ignored(rules[parent], rel, true) {
				return fs.SkipDir
			}
			rules[rel] = withDirIgnore(rules[parent], p, rel)
			return nil
		}
		if !d.Type().IsRegular() || ignored(rules[parent], rel, false) {
			return nil
		}
		return visit(p, rel)
	})
}
