package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxFileBytes is the largest file read or edit will load.
const maxFileBytes = 10 << 20

// checkRegular rejects non-regular files (e.g. FIFOs) and oversized files before reading.
func checkRegular(st os.FileInfo, path string) (Result, bool) {
	if !st.Mode().IsRegular() {
		return errResult("not a regular file: %s", path), false
	}
	if st.Size() > maxFileBytes {
		return errResult("file too large: %s (%d bytes)", path, st.Size()), false
	}
	return Result{}, true
}

func (r *Registry) read(input []byte) Result {
	var in struct {
		Path   *string `json:"path"`
		Offset *int    `json:"offset"`
		Limit  *int    `json:"limit"`
	}
	if err := decode(input, &in); err != nil {
		return errResult("invalid input: %v", err)
	}
	if in.Path == nil {
		return errResult("invalid input: path is required")
	}
	if in.Offset != nil && *in.Offset < 1 {
		return errResult("invalid input: offset must be >= 1")
	}
	if in.Limit != nil && *in.Limit < 1 {
		return errResult("invalid input: limit must be >= 1")
	}
	p, err := Resolve(r.workspace, *in.Path)
	if err != nil {
		return errResult("%v", err)
	}
	st, err := os.Stat(p)
	if err != nil {
		return errResult("%v", err)
	}
	if st.IsDir() {
		return errResult("%s is a directory", *in.Path)
	}
	if res, ok := checkRegular(st, *in.Path); !ok {
		return res
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return errResult("%v", err)
	}
	if in.Offset == nil && in.Limit == nil {
		return Result{Content: string(b)}
	}
	lines := strings.SplitAfter(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	start := 0
	if in.Offset != nil {
		start = *in.Offset - 1
	}
	if start > len(lines) {
		start = len(lines)
	}
	end := len(lines)
	if in.Limit != nil && *in.Limit < end-start {
		end = start + *in.Limit
	}
	return Result{Content: strings.Join(lines[start:end], "")}
}

func (r *Registry) write(input []byte) Result {
	var in struct {
		Path    *string `json:"path"`
		Content *string `json:"content"`
	}
	if err := decode(input, &in); err != nil {
		return errResult("invalid input: %v", err)
	}
	if in.Path == nil || in.Content == nil {
		return errResult("invalid input: path and content are required")
	}
	p, err := Resolve(r.workspace, *in.Path)
	if err != nil {
		return errResult("%v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return errResult("%v", err)
	}
	if err := os.WriteFile(p, []byte(*in.Content), 0o644); err != nil {
		return errResult("%v", err)
	}
	return Result{Content: fmt.Sprintf("wrote %d bytes to %s", len(*in.Content), *in.Path)}
}

func (r *Registry) edit(input []byte) Result {
	var in struct {
		Path       *string `json:"path"`
		OldText    *string `json:"old_text"`
		NewText    *string `json:"new_text"`
		ReplaceAll bool    `json:"replace_all"`
	}
	if err := decode(input, &in); err != nil {
		return errResult("invalid input: %v", err)
	}
	if in.Path == nil || in.OldText == nil || in.NewText == nil {
		return errResult("invalid input: path, old_text and new_text are required")
	}
	if *in.OldText == "" {
		return errResult("old_text must not be empty")
	}
	p, err := Resolve(r.workspace, *in.Path)
	if err != nil {
		return errResult("%v", err)
	}
	st, err := os.Stat(p)
	if err != nil {
		return errResult("%v", err)
	}
	if res, ok := checkRegular(st, *in.Path); !ok {
		return res
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return errResult("%v", err)
	}
	s := string(b)
	n := strings.Count(s, *in.OldText)
	switch {
	case n == 0:
		return errResult("old_text not found in %s", *in.Path)
	case n > 1 && !in.ReplaceAll:
		return errResult("old_text matches %d places in %s; add surrounding context or set replace_all", n, *in.Path)
	}
	out := strings.ReplaceAll(s, *in.OldText, *in.NewText)
	if err := os.WriteFile(p, []byte(out), st.Mode().Perm()); err != nil {
		return errResult("%v", err)
	}
	return Result{Content: fmt.Sprintf("replaced %d occurrence(s)", n)}
}

func (r *Registry) list(input []byte) Result {
	var in struct {
		Path *string `json:"path"`
	}
	if err := decode(input, &in); err != nil {
		return errResult("invalid input: %v", err)
	}
	path := "."
	if in.Path != nil && *in.Path != "" {
		path = *in.Path
	}
	p, err := Resolve(r.workspace, path)
	if err != nil {
		return errResult("%v", err)
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		return errResult("%v", err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() {
			n += "/"
		} else if e.Type()&os.ModeSymlink != 0 {
			if st, err := os.Stat(filepath.Join(p, n)); err == nil && st.IsDir() {
				n += "/"
			}
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return Result{Content: strings.Join(names, "\n")}
}
