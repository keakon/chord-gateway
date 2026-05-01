package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// loginShellEnv returns os.Environ() with PATH replaced by the user's login
// shell PATH.  This ensures that binaries installed in user-local paths
// (e.g. ~/.local/bin, /opt/homebrew/bin) are findable even when the gateway
// is launched from a context that doesn't source .zshrc / .bash_profile.
var loginShellEnv = sync.OnceValue(func() []string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	// Run a login shell that just prints PATH; -l (--login) sources
	// /etc/profile + ~/.profile (or ~/.zprofile / ~/.zshenv etc.).
	out, err := exec.Command(shell, "-l", "-c", "echo $PATH").Output()
	if err != nil {
		slog.Warn("failed to get login shell PATH, using current env", "error", err)
		return os.Environ()
	}
	loginPATH := strings.TrimSpace(string(out))
	if loginPATH == "" {
		return os.Environ()
	}
	// Merge: keep current env PATH entries that aren't in login PATH,
	// then append login PATH entries — this preserves runtime-added
	// paths (e.g. nvm) while also picking up login-only paths.
	currentPATH := os.Getenv("PATH")
	var merged []string
	if currentPATH != "" {
		seen := make(map[string]bool)
		for _, p := range append(
			strings.Split(currentPATH, ":"),
			strings.Split(loginPATH, ":")...,
		) {
			if p != "" && !seen[p] {
				seen[p] = true
				merged = append(merged, p)
			}
		}
	} else {
		merged = strings.Split(loginPATH, ":")
	}
	env := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "PATH=") {
			continue
		}
		env = append(env, e)
	}
	env = append(env, "PATH="+strings.Join(merged, ":"))
	return env
})

// sleepCtx waits for d to elapse or ctx to be cancelled. Returns true if the
// full duration elapsed, false if the context was cancelled. A non-positive d
// defaults to 1 second.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		d = time.Second
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
