package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Transfer admission must survive restart before the preparation ACK is sent.
func writePrivateFileSynced(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, privateDirMode); err != nil {
		return fmt.Errorf("create transfer directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".transfer-*")
	if err != nil {
		return fmt.Errorf("create transfer file: %w", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(privateFileMode); err != nil {
		return fmt.Errorf("protect transfer file: %w", err)
	}
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return fmt.Errorf("write transfer file: %w", err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("sync transfer file: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close transfer file: %w", err)
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("replace transfer file: %w", err)
	}
	return syncTransferDirectory(dir)
}
