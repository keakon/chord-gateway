package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CommitPhase records whether the configuration may already have been replaced.
type CommitPhase uint8

const (
	BeforeReplace CommitPhase = iota
	Replaced
	Durable
)

type CommitOutcome struct{ Phase CommitPhase }

type configWriteOps struct {
	create  func(string) (*os.File, error)
	write   func(*os.File, []byte) (int, error)
	sync    func(*os.File) error
	close   func(*os.File) error
	replace func(string, string) (CommitOutcome, error)
}

func defaultConfigWriteOps() configWriteOps {
	return configWriteOps{
		create:  func(dir string) (*os.File, error) { return os.CreateTemp(dir, ".gateway-config-*") },
		write:   func(f *os.File, b []byte) (int, error) { return f.Write(b) },
		sync:    func(f *os.File) error { return f.Sync() },
		close:   func(f *os.File) error { return f.Close() },
		replace: replaceConfigFile,
	}
}

func writeConfigDurable(ctx context.Context, path string, data []byte, beforeReplace func() error, ops configWriteOps) (CommitOutcome, error) {
	before := CommitOutcome{BeforeReplace}
	if err := ctx.Err(); err != nil {
		return before, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return before, fmt.Errorf("stat configuration: %w", err)
	}
	f, err := ops.create(filepath.Dir(path))
	if err != nil {
		return before, fmt.Errorf("create configuration temporary file: %w", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = protectConfigTemporary(f, path, info.Mode()); err != nil {
		return before, err
	}
	n, err := ops.write(f, data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return before, fmt.Errorf("write configuration: %w", err)
	}
	if err = ops.sync(f); err != nil {
		return before, fmt.Errorf("sync configuration: %w", err)
	}
	if err = ops.close(f); err != nil {
		return before, fmt.Errorf("close configuration: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return before, err
	}
	if beforeReplace != nil {
		if err = beforeReplace(); err != nil {
			return before, err
		}
	}
	return ops.replace(f.Name(), path)
}
