package config

import (
	"os"
	"path/filepath"
)

// CanonicalResource resolves a configured path to its stable filesystem
// identity so two configurations naming the same file through different
// paths (env vars, symlinks, non-existent parents) share one instance lock.
func CanonicalResource(path string) (string, error) {
	p, err := filepath.Abs(Expand(path))
	if err != nil {
		return "", err
	}
	p = filepath.Clean(p)
	var suffix []string
	for {
		resolved, e := filepath.EvalSymlinks(p)
		if e == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(e) {
			return "", e
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", e
		}
		suffix = append(suffix, filepath.Base(p))
		p = parent
	}
}
