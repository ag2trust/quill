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
	prefix  string // base + "/", or "" for the root
	depth   int    // number of segments in base
	segs    []string
	negate  bool
	dirOnly bool
}

// parseIgnore parses ignore-file data whose patterns are relative to base.
func parseIgnore(base string, data []byte) []ignoreRule {
	var rules []ignoreRule
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, `\ `) {
			line = line[:len(line)-1] // unescaped trailing spaces only
		}
		if line == "" || line[0] == '#' {
			continue
		}
		r := ignoreRule{base: base}
		if base != "" {
			r.prefix, r.depth = base+"/", strings.Count(base, "/")+1
		}
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

// ruleSet holds one ignore file's rules and links to the rules in force in the
// parent directory, so directories share their ancestors' rules instead of
// copying them.
type ruleSet struct {
	parent *ruleSet
	rules  []ignoreRule
}

// ignored reports whether rel (workspace-relative, slash-separated) is ignored.
// The last matching rule wins, and deeper ignore files come later, so the
// chain is searched from the innermost set and each set from its end.
func ignored(set *ruleSet, rel string, isDir bool) bool {
	segs := strings.Split(rel, "/")
	for ; set != nil; set = set.parent {
		for i := len(set.rules) - 1; i >= 0; i-- {
			r := set.rules[i]
			if r.dirOnly && !isDir {
				continue
			}
			sub := segs
			if r.base != "" {
				if !strings.HasPrefix(rel, r.prefix) {
					continue
				}
				sub = segs[r.depth:]
			}
			if matchSegs(r.segs, sub) {
				return !r.negate
			}
		}
	}
	return false
}

// maxIgnoreRules caps the ignore rules one walk loads, bounding memory and the
// per-path match cost on hostile trees. Past the cap, further ignore files are
// skipped: results get noisier, but the walk still completes.
const maxIgnoreRules = 5000

// loadIgnore reads an ignore file if it is a small regular file, taking at
// most *budget rules; otherwise nil.
func loadIgnore(p, base string, budget *int) []ignoreRule {
	if *budget <= 0 {
		return nil
	}
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
	rules := parseIgnore(base, data)
	if len(rules) > *budget {
		rules = rules[:*budget]
	}
	*budget -= len(rules)
	return rules
}

// withDirIgnore returns the rules in force inside a directory: parent, plus
// the directory's own .gitignore if it has rules.
func withDirIgnore(parent *ruleSet, dirPath, rel string, budget *int) *ruleSet {
	own := loadIgnore(filepath.Join(dirPath, ".gitignore"), rel, budget)
	if len(own) == 0 {
		return parent
	}
	return &ruleSet{parent: parent, rules: own}
}

// realDir reports whether p is a directory and not a symlink. O_NOFOLLOW
// guards only the last component, so .git and .git/info are checked first.
func realDir(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.IsDir()
}

// startRules returns the rules in force inside dir, a workspace-relative directory.
func startRules(ws, dir string, budget *int) *ruleSet {
	var set *ruleSet
	if realDir(filepath.Join(ws, ".git")) && realDir(filepath.Join(ws, ".git", "info")) {
		if ex := loadIgnore(filepath.Join(ws, ".git", "info", "exclude"), "", budget); len(ex) > 0 {
			set = &ruleSet{rules: ex}
		}
	}
	set = withDirIgnore(set, ws, "", budget)
	if dir == "" {
		return set
	}
	parts := strings.Split(dir, "/")
	for i := range parts {
		rel := strings.Join(parts[:i+1], "/")
		set = withDirIgnore(set, filepath.Join(ws, filepath.FromSlash(rel)), rel, budget)
	}
	return set
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
	rules := map[string]*ruleSet{}
	budget := maxIgnoreRules
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
				rules[rel] = startRules(ws, rel, &budget)
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
			rules[rel] = withDirIgnore(rules[parent], p, rel, &budget)
			return nil
		}
		if !d.Type().IsRegular() || ignored(rules[parent], rel, false) {
			return nil
		}
		return visit(p, rel)
	})
}
