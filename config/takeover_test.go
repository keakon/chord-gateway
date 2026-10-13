package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestChordStateDirectoryParsesAndExpands(t *testing.T) {
	var cfg Config
	data := []byte("chord_state_dir: ~/chord-state\nworkspaces:\n  project:\n    path: ./workspace\n")
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ChordStateDir != "~/chord-state" {
		t.Fatalf("state directory=%q", cfg.ChordStateDir)
	}
	cfg.ExpandPath(func(value string) string { return strings.Replace(value, "~/", "/home/example/", 1) })
	if cfg.ChordStateDir != "/home/example/chord-state" {
		t.Fatalf("expanded state directory=%q", cfg.ChordStateDir)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if issues := UnsupportedConfigFields(&doc); len(issues) != 0 {
		t.Fatalf("valid takeover config reported unsupported: %v", issues)
	}
}
