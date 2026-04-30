package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keakon/chord-gateway/config"
)

func TestIsAllowedWorkspacePath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "/work/project", want: true},
		{path: "~/work/project", want: true},
		{path: "~", want: true},
		{path: `C:\work\project`, want: true},
		{path: "D:/work/project", want: true},
		{path: `\\server\share\project`, want: true},
		{path: "./project", want: false},
		{path: "project", want: false},
		{path: `C:relative\project`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := config.IsAllowedWorkspacePath(tt.path); got != tt.want {
				t.Fatalf("IsAllowedWorkspacePath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestRenderUpdatedFeishuBindingConfig_AddsWorkspaceAndBindingPreservingComments(t *testing.T) {
	input := "# top\nims:\n  feishu:\n    app_id: cli_xxx\n    app_secret: secret # keep\nworkspaces:\n  default:\n    path: ~/old\n"
	updated, err := renderUpdatedFeishuBindingConfig([]byte(input), "oc_new", "project-b", "~/work/project-b")
	if err != nil {
		t.Fatalf("renderUpdatedFeishuBindingConfig() error = %v", err)
	}
	out := string(updated)
	for _, want := range []string{
		"# top",
		"app_secret: secret # keep",
		"chat_bindings:",
		`oc_new: project-b`,
		`project-b:`,
		`path: ~/work/project-b`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("updated config missing %q:\n%s", want, out)
		}
	}
	cfg, err := parseConfigBytes(updated)
	if err != nil {
		t.Fatalf("parseConfigBytes() error = %v", err)
	}
	imCfg := cfg.IMConfigByType("feishu")
	if imCfg == nil || imCfg.Feishu == nil {
		t.Fatalf("feishu config missing: %#v", cfg.IMs)
	}
	if got := imCfg.Feishu.ChatBindings["oc_new"]; got != "project-b" {
		t.Fatalf("chat binding = %q, want %q", got, "project-b")
	}
	if ws := workspaceByID(cfg, "project-b"); ws == nil || ws.Path != config.Expand("~/work/project-b") {
		t.Fatalf("workspace project-b = %#v", ws)
	}
}

func TestRenderUpdatedFeishuBindingConfig_UpdatesExistingEntries(t *testing.T) {
	input := "ims:\n  feishu:\n    app_id: cli_xxx\n    app_secret: secret\n    chat_bindings:\n      oc_chat: old-ws # trailing\nworkspaces:\n  old-ws:\n    path: ~/old\n  new-ws:\n    path: ~/new\n"
	updated, err := renderUpdatedFeishuBindingConfig([]byte(input), "oc_chat", "new-ws", "~/new")
	if err != nil {
		t.Fatalf("renderUpdatedFeishuBindingConfig() error = %v", err)
	}
	cfg, err := parseConfigBytes(updated)
	if err != nil {
		t.Fatalf("parseConfigBytes() error = %v", err)
	}
	imCfg := cfg.IMConfigByType("feishu")
	if imCfg == nil || imCfg.Feishu == nil {
		t.Fatalf("feishu config missing: %#v", cfg.IMs)
	}
	if got := imCfg.Feishu.ChatBindings["oc_chat"]; got != "new-ws" {
		t.Fatalf("chat binding = %q, want %q", got, "new-ws")
	}
}

func TestRenderUpdatedFeishuBindingConfig_RejectsPathMismatch(t *testing.T) {
	input := "ims:\n  feishu:\n    app_id: cli_xxx\n    app_secret: secret\nworkspaces:\n  ws1:\n    path: ~/original\n"
	_, err := renderUpdatedFeishuBindingConfig([]byte(input), "oc_chat", "ws1", "~/different")
	if err == nil {
		t.Fatal("expected error when path mismatches existing workspace")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRenderUpdatedFeishuBindingConfig_DoesNotQuotePlainHyphenatedScalars(t *testing.T) {
	input := "ims:\n  feishu:\n    app_id: cli_xxx\n    app_secret: secret\nworkspaces: {}\n"
	updated, err := renderUpdatedFeishuBindingConfig([]byte(input), "oc_new", "project-a", "~/work/project-a")
	if err != nil {
		t.Fatalf("renderUpdatedFeishuBindingConfig() error = %v", err)
	}
	out := string(updated)
	for _, want := range []string{`oc_new: project-a`, `project-a:`, `path: ~/work/project-a`} {
		if !strings.Contains(out, want) {
			t.Fatalf("updated config missing %q:\n%s", want, out)
		}
	}
	for _, notWant := range []string{`"project-a"`, `"~/work/project-a"`} {
		if strings.Contains(out, notWant) {
			t.Fatalf("updated config should not contain %q:\n%s", notWant, out)
		}
	}
}

func TestRenderUpdatedFeishuBindingConfig_EmptyMapsAndQuotedScalars(t *testing.T) {
	input := "ims:\n  feishu:\n    app_id: cli_xxx\n    app_secret: secret\n    chat_bindings: {}\nworkspaces: {}\n"
	updated, err := renderUpdatedFeishuBindingConfig([]byte(input), "oc: chat #1", "project one", "~/work/project one")
	if err != nil {
		t.Fatalf("renderUpdatedFeishuBindingConfig() error = %v", err)
	}
	out := string(updated)
	for _, want := range []string{`"oc: chat #1": "project one"`, `"project one":`, `path: "~/work/project one"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("updated config missing %q:\n%s", want, out)
		}
	}
	cfg, err := parseConfigBytes(updated)
	if err != nil {
		t.Fatalf("parseConfigBytes() error = %v", err)
	}
	imCfg := cfg.IMConfigByType("feishu")
	if imCfg == nil || imCfg.Feishu == nil {
		t.Fatalf("feishu config missing: %#v", cfg.IMs)
	}
	if got := imCfg.Feishu.ChatBindings["oc: chat #1"]; got != "project one" {
		t.Fatalf("chat binding = %q", got)
	}
	if ws := workspaceByID(cfg, "project one"); ws == nil || ws.Path != config.Expand("~/work/project one") {
		t.Fatalf("workspace = %#v", ws)
	}
}

func TestRenderUpdatedFeishuBindingConfig_PreservesNoTrailingNewlineAsValidYAML(t *testing.T) {
	input := "ims:\n  feishu:\n    app_id: cli_xxx\n    app_secret: secret\nworkspaces:\n  default:\n    path: ~/project"
	updated, err := renderUpdatedFeishuBindingConfig([]byte(input), "oc_chat", "default", "~/project")
	if err != nil {
		t.Fatalf("renderUpdatedFeishuBindingConfig() error = %v", err)
	}
	if _, err := parseConfigBytes(updated); err != nil {
		t.Fatalf("parseConfigBytes() error = %v\n%s", err, string(updated))
	}
}

func TestUpsertFeishuBindingConfigFile_WritesValidatedConfig(t *testing.T) {
	dir := t.TempDir()
	workspaceDir := filepath.Join(dir, "project")
	if err := os.Mkdir(workspaceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	content := "ims:\n  feishu:\n    app_id: cli_xxx\n    app_secret: secret\nworkspaces:\n  default:\n    path: " + workspaceDir + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := upsertFeishuBindingConfigFile(path, "oc_chat", "default", workspaceDir)
	if err != nil {
		t.Fatalf("upsertFeishuBindingConfigFile() error = %v", err)
	}
	imCfg := cfg.IMConfigByType("feishu")
	if imCfg == nil || imCfg.Feishu == nil {
		t.Fatalf("feishu config missing: %#v", cfg.IMs)
	}
	if got := imCfg.Feishu.ChatBindings["oc_chat"]; got != "default" {
		t.Fatalf("chat binding = %q, want %q", got, "default")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "oc_chat: default") {
		t.Fatalf("updated file missing binding:\n%s", string(data))
	}
}

func TestUpsertFeishuBindingConfigFile_RejectsRelativeWorkspacePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "ims:\n  feishu:\n    app_id: cli_xxx\n    app_secret: secret\nworkspaces:\n  default:\n    path: ./project\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := upsertFeishuBindingConfigFile(path, "oc_chat", "default", "./project")
	if err == nil || !strings.Contains(err.Error(), "must start with") {
		t.Fatalf("expected path prefix error, got %v", err)
	}
}

func TestUpsertFeishuBindingConfigFile_RejectsMissingWorkspacePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "ims:\n  feishu:\n    app_id: cli_xxx\n    app_secret: secret\nworkspaces:\n  default:\n    path: " + filepath.Join(dir, "missing") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := upsertFeishuBindingConfigFile(path, "oc_chat", "default", filepath.Join(dir, "missing"))
	if err == nil || !strings.Contains(err.Error(), "workspace path") {
		t.Fatalf("expected workspace path error, got %v", err)
	}
}
