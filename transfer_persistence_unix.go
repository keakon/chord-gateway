//go:build !windows

package main

import (
	"fmt"
	"os"
)

func syncTransferDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open transfer directory: %w", err)
	}
	defer f.Close()
	if err = f.Sync(); err != nil {
		return fmt.Errorf("sync transfer directory: %w", err)
	}
	return nil
}
