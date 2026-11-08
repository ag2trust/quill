package tools

import (
	"errors"
	"path"
	"strings"
)

// Glob patterns match slash-separated relative paths segment by segment.
// Within a segment the syntax is path.Match (* ? [...] and \ escapes); a "**"
// segment matches zero or more whole segments.

const maxGlobAlternatives = 64

// splitPattern splits p into segments, collapsing runs of "**".
func splitPattern(p string) []string {
	var segs []string
	for _, s := range strings.Split(p, "/") {
		if s == "**" && len(segs) > 0 && segs[len(segs)-1] == "**" {
			continue
		}
		segs = append(segs, s)
	}
	return segs
}

// validSegments reports whether every segment is valid path.Match syntax.
func validSegments(segs []string) bool {
	for _, s := range segs {
		if _, err := path.Match(s, ""); err != nil {
			return false
		}
	}
	return true
}

// matchSegs reports whether the name segments match the pattern segments.
func matchSegs(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			pat = pat[1:]
			if len(pat) == 0 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegs(pat, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// expandBraces expands {a,b} alternatives, including nested ones.
func expandBraces(p string) ([]string, error) {
	open := strings.IndexByte(p, '{')
	if open < 0 {
		if strings.IndexByte(p, '}') >= 0 {
			return nil, errors.New("unmatched }")
		}
		return []string{p}, nil
	}
	depth, last := 0, open+1
	var alts []string
	for i := open; i < len(p); i++ {
		switch p[i] {
		case '{':
			depth++
		case ',':
			if depth == 1 {
				alts = append(alts, p[last:i])
				last = i + 1
			}
		case '}':
			depth--
			if depth == 0 {
				alts = append(alts, p[last:i])
				var out []string
				for _, a := range alts {
					rest, err := expandBraces(p[:open] + a + p[i+1:])
					if err != nil {
						return nil, err
					}
					out = append(out, rest...)
					if len(out) > maxGlobAlternatives {
						return nil, errors.New("too many {} alternatives")
					}
				}
				return out, nil
			}
		}
	}
	return nil, errors.New("unmatched {")
}

// fileGlob is a compiled glob: a path matches if any alternative matches.
type fileGlob [][]string

// compileGlob compiles a tool-supplied glob. A pattern without a slash matches
// the file name at any depth.
func compileGlob(p string) (fileGlob, error) {
	p = strings.TrimPrefix(strings.TrimPrefix(p, "./"), "/")
	if p == "" {
		return nil, errors.New("empty pattern")
	}
	alts, err := expandBraces(p)
	if err != nil {
		return nil, err
	}
	var g fileGlob
	for _, a := range alts {
		if !strings.Contains(a, "/") {
			a = "**/" + a
		}
		segs := splitPattern(a)
		if !validSegments(segs) {
			return nil, errors.New("syntax error in pattern")
		}
		g = append(g, segs)
	}
	return g, nil
}

// match reports whether the slash-separated relative path rel matches.
func (g fileGlob) match(rel string) bool {
	name := strings.Split(rel, "/")
	for _, segs := range g {
		if matchSegs(segs, name) {
			return true
		}
	}
	return false
}
