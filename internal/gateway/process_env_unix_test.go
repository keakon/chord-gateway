//go:build unix

package gateway

import (
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"
)

// A login shell whose profile never returns must not wedge spawn: the PATH
// probe is bounded and falls back to the current environment.
func TestLoginShellPathEnvTimeoutFallback(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "slow-shell.sh")
	// Replace the shell so cancellation also reaps the blocking process.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatalf("write slow shell script: %v", err)
	}

	t.Setenv("SHELL", script)
	oldTimeout := loginShellProbeTimeout
	oldWaitDelay := loginShellProbeWaitDelay
	loginShellProbeTimeout = 200 * time.Millisecond
	loginShellProbeWaitDelay = 200 * time.Millisecond
	t.Cleanup(func() {
		loginShellProbeTimeout = oldTimeout
		loginShellProbeWaitDelay = oldWaitDelay
	})

	start := time.Now()
	env := loginShellPathEnv()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("login shell probe took %v, want bounded by the probe timeout and wait delay", elapsed)
	}
	if !reflect.DeepEqual(env, os.Environ()) {
		t.Fatal("probe did not fall back to the current environment")
	}
}

func TestLoginShellPathEnvBoundsInheritedPipes(t *testing.T) {
	dir := t.TempDir()
	fifoPath := filepath.Join(dir, "release")
	readyPath := filepath.Join(dir, "ready")
	finishedPath := filepath.Join(dir, "finished")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatal(err)
	}
	// Keep both ends open so releasing the child never waits for it to open
	// the FIFO, including cleanup after a failed startup assertion.
	release, err := os.OpenFile(fifoPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = release.Close() })
	script := filepath.Join(dir, "inherited-pipes.sh")
	body := "#!/bin/sh\n" +
		"{\n" +
		"  printf ready > " + shellQuote(readyPath) + "\n" +
		"  IFS= read -r release\n" +
		"  exec 1>&- 2>&-\n" +
		"  printf finished > " + shellQuote(finishedPath) + "\n" +
		"} < " + shellQuote(fifoPath) + " &\n" +
		"printf '/probe-only-path\\n'\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", script)
	swapTestValue(t, &loginShellProbeTimeout, 10*time.Second)
	swapTestValue(t, &loginShellProbeWaitDelay, 50*time.Millisecond)
	fallback := os.Environ()
	done := make(chan struct{})
	var env []string
	go func() {
		defer close(done)
		env = loginShellPathEnv()
	}()
	t.Cleanup(func() {
		if _, err := release.WriteString("\n"); err != nil {
			t.Error(err)
		}
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("probe did not finish after releasing inherited pipes")
		}
		waitForCondition(t, 5*time.Second, func() bool {
			_, err := os.Stat(finishedPath)
			return err == nil
		}, "pipe holder to close its output and finish")
	})
	waitForCondition(t, 5*time.Second, func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	}, "descendant to hold the probe output pipes")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("probe waited for the descendant instead of bounding pipe drain")
	}
	if !reflect.DeepEqual(env, fallback) {
		t.Fatal("unclosed inherited pipes did not cause fallback to the current environment")
	}
	if _, err := os.Stat(finishedPath); !os.IsNotExist(err) {
		t.Fatalf("pipe holder finished before being released: %v", err)
	}
}
