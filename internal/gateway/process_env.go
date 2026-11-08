package gateway

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/keakon/golog/log"
)

// loginShellProbeTimeout bounds the login-shell PATH probe. A blocking shell
// profile (network mount, prompt hook) must not wedge spawn, which runs under
// lifecycle locks; on timeout the current environment is used instead.
var loginShellProbeTimeout = 5 * time.Second

// loginShellProbeWaitDelay bounds the pipe drain after the shell is killed or
// exits, so descendants that inherit the output pipes cannot extend the wait.
var loginShellProbeWaitDelay = 2 * time.Second

// loginShellEnv returns os.Environ() with PATH replaced by the user's login
// shell PATH.  This ensures that binaries installed in user-local paths
// (e.g. ~/.local/bin, /opt/homebrew/bin) are findable even when the gateway
// is launched from a context that doesn't source .zshrc / .bash_profile.
var loginShellEnv = sync.OnceValue(loginShellPathEnv)

func loginShellPathEnv() []string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	// Run a login shell that just prints PATH; -l (--login) sources
	// /etc/profile + ~/.profile (or ~/.zprofile / ~/.zshenv etc.).
	// WaitDelay stops the wait even when profile-spawned background processes
	// inherit the output pipes, which would otherwise block until they exit.
	ctx, cancel := context.WithTimeout(context.Background(), loginShellProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-l", "-c", "echo $PATH")
	cmd.WaitDelay = loginShellProbeWaitDelay
	out, err := cmd.Output()
	if err != nil {
		log.Warnf("failed to get login shell PATH, using current env error=%v", err)
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
}
