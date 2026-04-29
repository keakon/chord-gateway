package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/keakon/chord-gateway/config"
)

func TestChordManager_SpawnArgsForWorkspace_UsesPinnedSessionID(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{IMs: []config.IMAdapterConfig{{Wechat: &config.WechatConfig{}}}, SessionPinsFile: filepath.Join(dir, "pins.json")}
	paths, err := config.Resolve("", filepath.Join(dir, "config.yaml"), dir, filepath.Join(dir, "gateway.log"), "", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	m := NewChordManager(cfg, paths)

	if got := m.spawnArgsForKey(processKey{workspaceID: "default", imType: "wechat", chatID: "chat"}.String()); got != nil {
		t.Fatalf("expected nil args when no pin, got %v", got)
	}

	if err := m.pins.Set(processKey{workspaceID: "default", imType: "wechat", chatID: "chat"}.String(), "123"); err != nil {
		t.Fatalf("set pin: %v", err)
	}
	got := m.spawnArgsForKey(processKey{workspaceID: "default", imType: "wechat", chatID: "chat"}.String())
	want0, want1 := "--resume", "123"
	if len(got) != 2 || got[0] != want0 || got[1] != want1 {
		t.Fatalf("args = %v, want [%q %q]", got, want0, want1)
	}

	// Ensure persistence file exists.
	if _, err := os.Stat(filepath.Join(dir, "pins.json")); err != nil {
		t.Fatalf("pins file missing: %v", err)
	}
}
