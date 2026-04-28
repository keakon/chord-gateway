//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
)

func TestChordProcess_TerminateGroupKillsProcessGroup(t *testing.T) {
	// Use /bin/sh as a stand-in for chord. It will spawn a long-running child
	// so we can verify process-group termination.
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}

	tmp := t.TempDir()
	ws := config.Workspace{ID: "test", Path: tmp}
	cfg := &config.Config{ChordPath: "/bin/sh", Workspaces: []config.Workspace{ws}}
	paths, err := config.Resolve("", filepath.Join(tmp, "config.yaml"), tmp, filepath.Join(tmp, "gateway.log"), "", "", tmp)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewChordManager(cfg, paths)

	// Spawn: /bin/sh headless -d <dir> <extraArgs...>
	// We abuse extra args to run a shell script.
	p, err := mgr.SpawnWithArgs("test", "-c", "sleep 60 & sleep 60")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		t.Fatalf("spawn returned nil process")
	}

	pgid, err := syscall.Getpgid(p.cmd.Process.Pid)
	if err != nil || pgid <= 0 {
		t.Fatalf("Getpgid: %v pgid=%d", err, pgid)
	}

	// Create a sentinel file in temp dir just to keep linter happy about tmp usage.
	_ = os.WriteFile(filepath.Join(tmp, "sentinel"), []byte("x"), 0o644)

	// Terminate the whole group and ensure the leader dies.
	p.TerminateGroup(200 * time.Millisecond)

	// The process should be gone.
	if err := syscall.Kill(p.cmd.Process.Pid, 0); err == nil {
		t.Fatalf("process still alive after TerminateGroup")
	}

	// And the group should be gone too.
	if err := syscall.Kill(-pgid, 0); err == nil {
		t.Fatalf("process group still alive after TerminateGroup")
	}
}
