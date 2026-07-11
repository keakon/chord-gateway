package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPrivateRotatingLogWriterUsesPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not enforced on Windows")
	}

	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "gateway.log")
	writer, err := newPrivateRotatingLogWriter(path, 16, 2)
	if err != nil {
		t.Fatalf("newPrivateRotatingLogWriter: %v", err)
	}

	assertPathMode(t, dir, privateDirMode)
	assertPathMode(t, path, privateFileMode)

	if _, err := writer.Write([]byte("0123456789abcdef")); err != nil {
		_ = writer.Close()
		t.Fatalf("Write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	assertPathMode(t, path, privateFileMode)
	assertPathMode(t, path+".1", privateFileMode)
}

func TestPrivateRotatingLogWriterRestrictsExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not enforced on Windows")
	}

	path := filepath.Join(t.TempDir(), "gateway.log")
	if err := os.WriteFile(path, []byte("existing"), 0o644); err != nil {
		t.Fatalf("write existing log: %v", err)
	}
	writer, err := newPrivateRotatingLogWriter(path, 1024, 2)
	if err != nil {
		t.Fatalf("newPrivateRotatingLogWriter: %v", err)
	}
	defer writer.Close()

	assertPathMode(t, path, privateFileMode)
}
