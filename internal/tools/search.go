package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
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
// relpath:line: text. It opens only regular files and never follows symlinks.
func (r *Registry) search(ctx context.Context, input json.RawMessage) Result {
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
	re, err := regexp.Compile(*in.Pattern)
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
	walkErr := filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			if path == start {
				return err
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() { // symlinks, FIFOs, devices, sockets
			return nil
		}
		rel, err := filepath.Rel(r.workspace, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		return searchFile(ctx, path, rel, re, &out, &n, &more)
	})
	if walkErr != nil && !errors.Is(walkErr, errSearchDone) {
		return errResult("search: %v", walkErr)
	}
	if n == 0 {
		return Result{Content: "no matches"}
	}
	res := strings.TrimSuffix(out.String(), "\n")
	if more {
		res += "\n[stopped after 200 matches]"
	}
	return Result{Content: res}
}

func searchFile(ctx context.Context, path, rel string, re *regexp.Regexp, out *strings.Builder, n *int, more *bool) error {
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
		out.WriteString(rel + ":" + strconv.Itoa(line) + ": " + clipLine(text) + "\n")
		*n++
	}
	return nil
}
