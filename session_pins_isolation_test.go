package main

import (
	"path/filepath"
	"testing"

	"github.com/keakon/chord-gateway/config"
)

func TestSessionPins_IsolatedByChatAndIMType(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{IMs: []config.IMAdapterConfig{{Wechat: &config.WechatConfig{}}}, SessionPinsFile: filepath.Join(dir, "pins.json")}
	paths, err := config.Resolve("", filepath.Join(dir, "config.yaml"), dir, filepath.Join(dir, "gateway.log"), "", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	m := NewChordManager(cfg, paths)

	k1 := (processKey{workspaceID: "w", imType: "wechat", chatID: "c1"}).String()
	k2 := (processKey{workspaceID: "w", imType: "wechat", chatID: "c2"}).String()
	k3 := (processKey{workspaceID: "w", imType: "feishu", chatID: "c1"}).String()

	if err := m.pins.Set(k1, "s1"); err != nil {
		t.Fatalf("set pin k1: %v", err)
	}
	if err := m.pins.Set(k2, "s2"); err != nil {
		t.Fatalf("set pin k2: %v", err)
	}
	if err := m.pins.Set(k3, "s3"); err != nil {
		t.Fatalf("set pin k3: %v", err)
	}

	if got := m.pins.Get(k1); got != "s1" {
		t.Fatalf("pin k1=%q, want s1", got)
	}
	if got := m.pins.Get(k2); got != "s2" {
		t.Fatalf("pin k2=%q, want s2", got)
	}
	if got := m.pins.Get(k3); got != "s3" {
		t.Fatalf("pin k3=%q, want s3", got)
	}
}
