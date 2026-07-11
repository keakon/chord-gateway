package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWritePrivateFileAtomicallyUsesPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not enforced on Windows")
	}

	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "secret.json")
	if err := writePrivateFileAtomically(path, []byte("secret")); err != nil {
		t.Fatalf("writePrivateFileAtomically: %v", err)
	}
	assertPathMode(t, dir, privateDirMode)
	assertPathMode(t, path, privateFileMode)

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod existing file: %v", err)
	}
	if err := writePrivateFileAtomically(path, []byte("updated")); err != nil {
		t.Fatalf("rewrite private file: %v", err)
	}
	assertPathMode(t, path, privateFileMode)
}

func TestWriteFileAtomicallyPreservesExistingMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not enforced on Windows")
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
		t.Fatalf("write existing file: %v", err)
	}
	if err := writeFileAtomically(path, []byte("new"), 0o644); err != nil {
		t.Fatalf("writeFileAtomically: %v", err)
	}
	assertPathMode(t, path, 0o640)
}

func assertPathMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode(%s) = %04o, want %04o", path, got, want)
	}
}
