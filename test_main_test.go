package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
)

// One root per test process isolates machine-wide locks and immutable fixtures
// without changing HOME or the Go build cache. Tests of mutual exclusion still
// share a namespace deliberately; parallel adapter tests need distinct accounts.
var testProcessRoot string

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "chord-gateway-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testProcessRoot = root
	im.InstanceLockCacheDir = func() (string, error) { return root, nil }
	code := m.Run()
	if err := os.RemoveAll(root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
