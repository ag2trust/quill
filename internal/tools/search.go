package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

const (
	searchMaxMatches  = 200
	searchMaxFile     = 1 << 20
	searchBinaryProbe = 8 << 10
)

const searchMaxLine = 500

var errSearchDone = errors.New("search done")

// clipLine cuts s to at most searchMaxLine bytes on a rune boundary.
func clipLine(s string) string {
	if len(s) <= searchMaxLine {
		return s
	}
	cut := searchMaxLine
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// search walks from path in lexical order and reports matching lines as
// relpath:line: text, or with output_mode "files" each matching file once.
// It opens only regular files, never follows symlinks, and skips ignored paths.
func (r *Registry) search(ctx context.Context, input json.RawMessage) Result {
	var in struct {
		Pattern    *string `json:"pattern"`
		Path       *string `json:"path"`
		Glob       *string `json:"glob"`
		OutputMode *string `json:"output_mode"`
	}
	if err := decode(input, &in); err != nil {
		return errResult("invalid input: %v", err)
	}
	if in.Pattern == nil {
		return errResult("invalid input: pattern is required")
	}
	re, err := regexp.Compile(*in.Pattern)
	if err != nil {
		return errResult("invalid pattern: %v", err)
	}
	files := false
	if in.OutputMode != nil {
		switch *in.OutputMode {
		case "content":
		case "files":
			files = true
		default:
			return errResult("invalid input: output_mode must be content or files")
		}
	}
	var g fileGlob
	if in.Glob != nil {
		if g, err = compileGlob(*in.Glob); err != nil {
			return errResult("invalid glob: %v", err)
		}
	}
	p := "."
	if in.Path != nil {
		p = *in.Path
	}
	start, err := Resolve(r.workspace, p)
	if err != nil {
		return errResult("%v", err)
	}

	var out strings.Builder
	n := 0
	more := false
	walkErr := walkFiles(ctx, r.workspace, start, func(path, rel string) error {
		if g != nil && !g.match(relTo(start, path)) {
			return nil
		}
		return searchFile(ctx, path, rel, re, files, &out, &n, &more)
	})
	if walkErr != nil && !errors.Is(walkErr, errSearchDone) {
		return errResult("search: %v", walkErr)
	}
	if n == 0 {
		return Result{Content: "no matches"}
	}
	res := strings.TrimSuffix(out.String(), "\n")
	if more {
		if files {
			res += "\n[stopped after 200 files]"
		} else {
			res += "\n[stopped after 200 matches]"
		}
	}
	return Result{Content: res}
}

// relTo returns path relative to start as a slash path; for a file start, its name.
func relTo(start, path string) string {
	if path == start {
		return filepath.Base(path)
	}
	rel, err := filepath.Rel(start, path)
	if err != nil {
		return filepath.Base(path)
	}
	return filepath.ToSlash(rel)
}

func searchFile(ctx context.Context, path, rel string, re *regexp.Regexp, files bool, out *strings.Builder, n *int, more *bool) error {
	// O_NONBLOCK and O_NOFOLLOW guard against a file swapped for a FIFO or symlink after the walk saw it.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > searchMaxFile {
		return nil
	}
	probe := make([]byte, searchBinaryProbe)
	k, err := io.ReadFull(f, probe)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil
	}
	if bytes.IndexByte(probe[:k], 0) >= 0 {
		return nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), searchMaxFile+1)
	for line := 1; sc.Scan(); line++ {
		if line%1024 == 0 {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
		}
		text := sc.Text()
		if !re.MatchString(text) {
			continue
		}
		if *n >= searchMaxMatches {
			*more = true
			return errSearchDone
		}
		*n++
		if files {
			out.WriteString(rel + "\n")
			return nil
		}
		out.WriteString(rel + ":" + strconv.Itoa(line) + ": " + clipLine(text) + "\n")
	}
	return nil
}

const globMaxFiles = 500

// glob lists regular files under path whose path relative to it matches pattern,
// in lexical walk order, skipping .git, symlinks, and ignored paths.
func (r *Registry) glob(ctx context.Context, input json.RawMessage) Result {
	var in struct {
		Pattern *string `json:"pattern"`
		Path    *string `json:"path"`
	}
	if err := decode(input, &in); err != nil {
		return errResult("invalid input: %v", err)
	}
	if in.Pattern == nil {
		return errResult("invalid input: pattern is required")
	}
	g, err := compileGlob(*in.Pattern)
	if err != nil {
		return errResult("invalid pattern: %v", err)
	}
	p := "."
	if in.Path != nil {
		p = *in.Path
	}
	start, err := Resolve(r.workspace, p)
	if err != nil {
		return errResult("%v", err)
	}
	var out strings.Builder
	n := 0
	more := false
	walkErr := walkFiles(ctx, r.workspace, start, func(path, rel string) error {
		if !g.match(relTo(start, path)) {
			return nil
		}
		if n >= globMaxFiles {
			more = true
			return errSearchDone
		}
		n++
		out.WriteString(rel + "\n")
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errSearchDone) {
		return errResult("glob: %v", walkErr)
	}
	if n == 0 {
		return Result{Content: "no files"}
	}
	res := strings.TrimSuffix(out.String(), "\n")
	if more {
		res += "\n[stopped after 500 files]"
	}
	return Result{Content: res}
}
