package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
)

func TestInstanceLockContentionAndRelease(t *testing.T) {
	resources := []string{filepath.Join(t.TempDir(), "config.yaml")}
	first, err := im.AcquireInstanceLocks(resources)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := im.AcquireInstanceLocks(resources)
	if second != nil {
		second.Close()
	}
	if !errors.Is(err, im.ErrInstanceRunning) {
		t.Fatal("second instance not rejected")
	}
	first.Close()
	third, err := im.AcquireInstanceLocks(resources)
	if err != nil {
		t.Fatal(err)
	}
	third.Close()
}
