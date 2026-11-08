// Package tools implements the agent's workspace tools.
package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const maxSymlinkHops = 40

// Resolve maps p to an absolute path inside workspace, which must already be
// absolute and symlink-free. Symlinks are resolved on the longest existing
// prefix (including dangling final symlinks), so no symlink can escape.
func Resolve(workspace, p string) (string, error) {
	outside := fmt.Errorf("path outside workspace: %s", p)
	full := p
	if !filepath.IsAbs(p) {
		full = filepath.Join(workspace, p)
	}
	full = filepath.Clean(full)
	if !within(workspace, full) {
		return "", outside
	}
	real, err := resolveFull(full, 0)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", p, err)
	}
	if !within(workspace, real) {
		return "", outside
	}
	return real, nil
}

func within(ws, p string) bool {
	return p == ws || strings.HasPrefix(p, strings.TrimSuffix(ws, string(filepath.Separator))+string(filepath.Separator))
}

// resolveFull resolves symlinks on the longest existing prefix of p and
// appends the non-existent remainder. A dangling symlink is followed manually.
func resolveFull(p string, hops int) (string, error) {
	cur, rest := p, ""
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(real, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if fi, lerr := os.Lstat(cur); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			if hops >= maxSymlinkHops {
				return "", errors.New("too many symlinks")
			}
			target, err := os.Readlink(cur)
			if err != nil {
				return "", err
			}
			if !filepath.IsAbs(target) {
				dir, err := filepath.EvalSymlinks(filepath.Dir(cur))
				if err != nil {
					return "", err
				}
				target = filepath.Join(dir, target)
			}
			return resolveFull(filepath.Join(target, rest), hops+1)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}
