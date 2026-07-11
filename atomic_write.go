package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	privateDirMode  = 0o700
	privateFileMode = 0o600
)

func writeFileAtomically(path string, data []byte, perm os.FileMode) error {
	return writeFileAtomicallyInDir(path, data, perm, 0o755, true)
}

func writePrivateFileAtomically(path string, data []byte) error {
	return writeFileAtomicallyInDir(path, data, privateFileMode, privateDirMode, false)
}

func writeFileAtomicallyInDir(path string, data []byte, perm, dirPerm os.FileMode, preserveExistingMode bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("mkdir atomic write dir: %w", err)
	}
	if info, err := os.Stat(path); err == nil {
		if preserveExistingMode {
			perm = info.Mode().Perm()
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat atomic write target: %w", err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}
	cleanup = false
	return nil
}
