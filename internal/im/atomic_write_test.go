package im

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
	if err := WritePrivateFileAtomically(path, []byte("secret")); err != nil {
		t.Fatalf("WritePrivateFileAtomically: %v", err)
	}
	assertPathMode(t, dir, PrivateDirMode)
	assertPathMode(t, path, PrivateFileMode)

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod existing file: %v", err)
	}
	if err := WritePrivateFileAtomically(path, []byte("updated")); err != nil {
		t.Fatalf("rewrite private file: %v", err)
	}
	assertPathMode(t, path, PrivateFileMode)
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
