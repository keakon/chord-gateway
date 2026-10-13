package main

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestInstanceLockContentionAndRelease(t *testing.T) {
	resources := []string{filepath.Join(t.TempDir(), "config.yaml")}
	first, err := acquireInstanceLocks(resources)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := acquireInstanceLocks(resources)
	if second != nil {
		second.Close()
	}
	if !errors.Is(err, errInstanceRunning) {
		t.Fatal("second instance not rejected")
	}
	first.Close()
	third, err := acquireInstanceLocks(resources)
	if err != nil {
		t.Fatal(err)
	}
	third.Close()
}
