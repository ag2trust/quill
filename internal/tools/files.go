package tools

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// maxFileBytes is the largest file read or edit will load.
const maxFileBytes = 10 << 20

// openRegular opens p without blocking and without following a final
// symlink, then rejects anything but a regular file, so a FIFO or device
// cannot hang a tool. display is the path as the model gave it.
func openRegular(p, display string, flag int, perm os.FileMode) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(p, flag|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, perm)
	if errors.Is(err, syscall.ENXIO) {
		// A FIFO with no reader, opened for writing.
		return nil, nil, fmt.Errorf("not a regular file: %s", display)
	}
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	switch {
	case err != nil:
	case st.IsDir():
		err = fmt.Errorf("%s is a directory", display)
	case !st.Mode().IsRegular():
		err = fmt.Errorf("not a regular file: %s", display)
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, st, nil
}

// readRegular reads a regular file of at most maxFileBytes.
func readRegular(p, display string) ([]byte, error) {
	f, st, err := openRegular(p, display, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st.Size() > maxFileBytes {
		return nil, fmt.Errorf("file too large: %s (%d bytes)", display, st.Size())
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFileBytes {
		return nil, fmt.Errorf("file too large: %s (over %d bytes)", display, maxFileBytes)
	}
	return b, nil
}

// writeRegular creates or overwrites a regular file with data.
func writeRegular(p, display string, data []byte) error {
	f, _, err := openRegular(p, display, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
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
	b, err := readRegular(p, *in.Path)
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
	if err := writeRegular(p, *in.Path, []byte(*in.Content)); err != nil {
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
	b, err := readRegular(p, *in.Path)
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
	if err := writeRegular(p, *in.Path, []byte(out)); err != nil {
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
