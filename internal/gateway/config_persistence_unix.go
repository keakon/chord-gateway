//go:build !windows

package gateway

import (
	"fmt"
	"os"
	"path/filepath"
)

func protectConfigTemporary(f *os.File, _ string, mode os.FileMode) error {
	return f.Chmod(mode.Perm() & 0o600)
}
func replaceConfigFile(temp, path string) (CommitOutcome, error) {
	if err := os.Rename(temp, path); err != nil {
		return CommitOutcome{BeforeReplace}, fmt.Errorf("replace configuration: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return CommitOutcome{Replaced}, fmt.Errorf("open configuration directory: %w", err)
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return CommitOutcome{Replaced}, fmt.Errorf("sync configuration directory: %w", err)
	}
	return CommitOutcome{Durable}, nil
}
