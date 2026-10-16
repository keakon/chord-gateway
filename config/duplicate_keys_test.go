package config

import (
	"strings"
	"testing"
)

func TestParseDocumentIgnoresDuplicateKeysWithWarning(t *testing.T) {
	data := `ims:
  feishu:
    app_id: first-app
    app_id: second-app
    app_secret: example-secret
    chat_bindings: {chat-a: one, chat-a: two}
workspaces:
  chord: {path: /tmp/one}
  chord: {path: /tmp/two}
`
	cfg, doc, warnings, err := ParseDocument([]byte(data))
	if err != nil {
		t.Fatalf("duplicate keys must not fail parsing: %v", err)
	}
	if doc == nil {
		t.Fatal("document required")
	}
	if len(warnings) != 3 {
		t.Fatalf("expected 3 duplicate key warnings, got %d: %v", len(warnings), warnings)
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{
		"duplicate key app_id in configuration.ims.feishu",
		"duplicate key chat-a in configuration.ims.feishu.chat_bindings",
		"duplicate key chord in configuration.workspaces",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing warning %q in %v", want, warnings)
		}
	}
	im := cfg.IMConfigByType("feishu")
	if im == nil || im.Feishu == nil || im.Feishu.AppID != "first-app" {
		t.Fatalf("first occurrence must win, got %+v", im)
	}
	if im.Feishu.ChatBindings["chat-a"] != "one" {
		t.Fatalf("first chat binding must win, got %v", im.Feishu.ChatBindings)
	}
	ws := cfg.WorkspaceByID("chord")
	if ws == nil || ws.Path != "/tmp/one" {
		t.Fatalf("first workspace must win, got %+v", cfg.Workspaces)
	}
}

func TestParseDocumentWithoutDuplicateKeysHasNoWarnings(t *testing.T) {
	_, _, warnings, err := ParseDocument([]byte("ims: {wechat: {}}\nworkspaces: {one: {path: /tmp/one}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
}

func TestParseDocumentKeepsMergeKeys(t *testing.T) {
	data := "<<: {chord_state_dir: /tmp/state}\nims: {wechat: {}}\nworkspaces: {one: {path: /tmp/one}}\n"
	cfg, _, warnings, err := ParseDocument([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("merge keys must not be reported as duplicates, got %v", warnings)
	}
	if cfg.ChordStateDir != "/tmp/state" {
		t.Fatalf("merge key lost: %q", cfg.ChordStateDir)
	}
}
