package gateway

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
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

	assertPathMode(t, dir, im.PrivateDirMode)
	assertPathMode(t, path, im.PrivateFileMode)

	if _, err := writer.Write([]byte("0123456789abcdef")); err != nil {
		_ = writer.Close()
		t.Fatalf("Write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	assertPathMode(t, path, im.PrivateFileMode)
	assertPathMode(t, path+".1", im.PrivateFileMode)
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

	assertPathMode(t, path, im.PrivateFileMode)
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
