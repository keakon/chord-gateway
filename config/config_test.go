package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidate_WechatMultiWorkspace_RequiresWorkspaceID(t *testing.T) {
	cfg := &Config{
		IMs: []IMAdapterConfig{{Wechat: &WechatConfig{}}},
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1"},
			{ID: "ws2", Path: "/tmp/ws2"},
		},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "wechat.workspace_id") {
		t.Fatalf("expected wechat workspace_id validation error, got: %v", err)
	}
}

func TestValidate_WechatWorkspaceID_OK(t *testing.T) {
	cfg := &Config{
		IMs: []IMAdapterConfig{{Wechat: &WechatConfig{WorkspaceID: "ws1"}}},
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1"},
			{ID: "ws2", Path: "/tmp/ws2"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidate_WechatWorkspaceID_NotFound(t *testing.T) {
	cfg := &Config{
		IMs:        []IMAdapterConfig{{Wechat: &WechatConfig{WorkspaceID: "missing"}}},
		Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1"}},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), `workspace_id "missing"`) {
		t.Fatalf("expected missing workspace_id error, got: %v", err)
	}
}

func TestValidate_RejectsInvalidWorkspacePathPrefix(t *testing.T) {
	cfg := &Config{
		IMs:        []IMAdapterConfig{{Feishu: &FeishuConfig{AppID: "app", AppSecret: "secret"}}},
		Workspaces: []Workspace{{ID: "ws1", Path: "./relative"}},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "must start with") {
		t.Fatalf("Validate() error = %v, want workspace path prefix error", err)
	}
}

func TestValidate_AcceptsSupportedWorkspacePathPrefixes(t *testing.T) {
	for _, path := range []string{"/tmp/ws1", "~/ws1", `C:\work\ws1`, "D:/work/ws1", `\\server\share\ws1`} {
		t.Run(path, func(t *testing.T) {
			cfg := &Config{
				IMs:        []IMAdapterConfig{{Feishu: &FeishuConfig{AppID: "app", AppSecret: "secret"}}},
				Workspaces: []Workspace{{ID: "ws1", Path: path}},
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestValidate_RejectsDuplicateIMTypes(t *testing.T) {
	cfg := &Config{
		IMs: []IMAdapterConfig{
			{Wechat: &WechatConfig{}},
			{Wechat: &WechatConfig{WorkspaceID: "ws1"}},
		},
		Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1"}},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "at most one wechat adapter") {
		t.Fatalf("expected duplicate wechat adapter error, got: %v", err)
	}
}

func TestValidate_FeishuMultiWorkspace_RequiresChatBindings(t *testing.T) {
	cfg := &Config{
		IMs: []IMAdapterConfig{{Feishu: &FeishuConfig{AppID: "app", AppSecret: "secret"}}},
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1"},
			{ID: "ws2", Path: "/tmp/ws2"},
		},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "feishu.chat_bindings") {
		t.Fatalf("expected chat_bindings validation error, got: %v", err)
	}
}

func TestValidate_FeishuChatBindingsUnknownWorkspace(t *testing.T) {
	cfg := &Config{
		IMs: []IMAdapterConfig{{Feishu: &FeishuConfig{AppID: "app", AppSecret: "secret", ChatBindings: map[string]string{"oc_1": "missing"}}}},
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1"},
			{ID: "ws2", Path: "/tmp/ws2"},
		},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "unknown workspace") {
		t.Fatalf("expected unknown workspace error, got: %v", err)
	}
}

func TestValidate_DedupesAllowedOpenIDs_AndAddsOwner(t *testing.T) {
	cfg := &Config{
		IMs: []IMAdapterConfig{{Feishu: &FeishuConfig{
			AppID:          "app",
			AppSecret:      "secret",
			OwnerOpenID:    "ou_owner",
			AllowedOpenIDs: []string{"ou_a", "ou_a"},
		}}},
		Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	got := cfg.IMs[0].Feishu.AllowedOpenIDs
	if len(got) != 2 || got[0] != "ou_a" || got[1] != "ou_owner" {
		t.Fatalf("AllowedOpenIDs = %v", got)
	}
}

func TestResolveWorkspace_Wechat_UsesConfiguredWorkspace(t *testing.T) {
	cfg := &Config{
		IMs: []IMAdapterConfig{{Wechat: &WechatConfig{WorkspaceID: "ws2"}}},
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1"},
			{ID: "ws2", Path: "/tmp/ws2"},
		},
	}
	ws, err := cfg.ResolveWorkspace("wechat", "any-chat-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ws.ID != "ws2" {
		t.Fatalf("got workspace %q, want %q", ws.ID, "ws2")
	}
}

func TestResolveWorkspace_Wechat_ConfiguredWorkspaceMissing(t *testing.T) {
	cfg := &Config{
		IMs:        []IMAdapterConfig{{Wechat: &WechatConfig{WorkspaceID: "missing"}}},
		Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1"}},
	}
	_, err := cfg.ResolveWorkspace("wechat", "any-chat-id")
	if err == nil || !strings.Contains(err.Error(), `workspace_id "missing"`) {
		t.Fatalf("expected missing workspace error, got: %v", err)
	}
}

func TestResolveWorkspace_Feishu_SingleWorkspace(t *testing.T) {
	cfg := &Config{IMs: []IMAdapterConfig{{Feishu: &FeishuConfig{AppID: "app", AppSecret: "secret"}}}, Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}
	ws, err := cfg.ResolveWorkspace("feishu", "any-chat-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ws.ID != "ws1" {
		t.Fatalf("got workspace %q, want %q", ws.ID, "ws1")
	}
}

func TestResolveWorkspace_Feishu_MultiWorkspace(t *testing.T) {
	cfg := &Config{
		IMs: []IMAdapterConfig{{Feishu: &FeishuConfig{AppID: "app", AppSecret: "secret", ChatBindings: map[string]string{"oc_2": "ws2"}}}},
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1"},
			{ID: "ws2", Path: "/tmp/ws2"},
		},
	}
	ws, err := cfg.ResolveWorkspace("feishu", "oc_2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ws.ID != "ws2" {
		t.Fatalf("got workspace %q, want %q", ws.ID, "ws2")
	}
}

func TestResolveWorkspace_Feishu_NoMatch(t *testing.T) {
	cfg := &Config{
		IMs: []IMAdapterConfig{{Feishu: &FeishuConfig{AppID: "app", AppSecret: "secret", ChatBindings: map[string]string{"oc_1": "ws1"}}}},
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1"},
			{ID: "ws2", Path: "/tmp/ws2"},
		},
	}
	_, err := cfg.ResolveWorkspace("feishu", "oc_missing")
	if err == nil || !strings.Contains(err.Error(), "feishu chat_id") {
		t.Fatalf("expected feishu no-match error, got: %v", err)
	}
}

func TestResolveWorkspace_Console_SingleWorkspace(t *testing.T) {
	cfg := &Config{Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}
	ws, err := cfg.ResolveWorkspace("console", "any-chat-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ws.ID != "ws1" {
		t.Fatalf("got workspace %q, want %q", ws.ID, "ws1")
	}
}

func TestResolveWorkspace_Console_NoMatch(t *testing.T) {
	cfg := &Config{
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1"},
			{ID: "ws2", Path: "/tmp/ws2"},
		},
	}
	_, err := cfg.ResolveWorkspace("console", "console-chat")
	if err == nil || !strings.Contains(err.Error(), "console chat_id") {
		t.Fatalf("expected console no-match error, got: %v", err)
	}
}

func TestResolveWorkspace_OtherSingleWorkspace(t *testing.T) {
	cfg := &Config{Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}
	ws, err := cfg.ResolveWorkspace("custom", "custom-chat")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ws.ID != "ws1" {
		t.Fatalf("got workspace %q, want %q", ws.ID, "ws1")
	}
}

func TestResolveWorkspace_OtherNoRoute(t *testing.T) {
	cfg := &Config{Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1"}, {ID: "ws2", Path: "/tmp/ws2"}}}
	_, err := cfg.ResolveWorkspace("custom", "missing")
	if err == nil || !strings.Contains(err.Error(), `no custom adapter configured`) {
		t.Fatalf("expected other no-route error, got: %v", err)
	}
}

func TestWorkspaceByID(t *testing.T) {
	cfg := &Config{Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}
	if ws := cfg.WorkspaceByID("ws1"); ws == nil || ws.Path != "/tmp/ws1" {
		t.Fatalf("WorkspaceByID(ws1) = %#v", ws)
	}
	if ws := cfg.WorkspaceByID("missing"); ws != nil {
		t.Fatalf("WorkspaceByID(missing) = %#v", ws)
	}
}

func TestLoad_IgnoresUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`
ims:
  wechat: {}
workspaces:
  ws1:
    path: ~/project
storage_dir: /tmp/x
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.IMs) != 1 || cfg.IMs[0].Type() != "wechat" {
		t.Fatalf("loaded IMs = %#v", cfg.IMs)
	}
	if len(cfg.Workspaces) != 1 || cfg.Workspaces[0].ID != "ws1" {
		t.Fatalf("loaded Workspaces = %#v", cfg.Workspaces)
	}
}

func TestLoad_ExpandsWechatTokenPath(t *testing.T) {
	dir := t.TempDir()
	oldHome := os.Getenv("HOME")
	t.Setenv("HOME", dir)
	t.Cleanup(func() { _ = os.Setenv("HOME", oldHome) })
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`
ims:
  wechat:
    token_path: ~/secrets/wechat-token.json
workspaces:
  ws1:
    path: ~/project
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := filepath.Join(dir, "secrets", "wechat-token.json")
	if got := cfg.IMs[0].Wechat.TokenPath; got != want {
		t.Fatalf("TokenPath = %q, want %q", got, want)
	}
}

func TestLoad_RejectsLegacyListForms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`
ims:
  - feishu:
      app_id: app
      app_secret: secret
workspaces:
  - id: ws1
    path: ~/project
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "expected a YAML mapping") {
		t.Fatalf("Load() error = %v, want a YAML mapping rejection", err)
	}
}

func TestLoad_RejectsWorkspaceMapIDMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`
ims:
  wechat: {}
workspaces:
  ws1:
    id: other
    path: ~/project
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "does not match map key") {
		t.Fatalf("expected map-key mismatch error, got: %v", err)
	}
}

func TestLoad_IgnoresRemovedConfigFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`
im:
  type: wechat
  wechat:
    bot_type: "3"
ims:
  feishu:
    app_id: app
    app_secret: secret
workspaces:
  ws1:
    path: ~/project
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.IMs) != 1 || cfg.IMs[0].Type() != "feishu" {
		t.Fatalf("loaded IMs = %#v", cfg.IMs)
	}
}

func TestLoad_RejectsNonYAMLFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	content := []byte(`{"workspaces":{"ws1":{"path":"~/project"}}}`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "only .yaml and .yml are supported") {
		t.Fatalf("expected non-YAML extension error, got: %v", err)
	}
}

func TestConfigHelpers(t *testing.T) {
	t.Run("IdleTimeoutDuration default valid and invalid", func(t *testing.T) {
		cfg := &Config{}
		if got := cfg.IdleTimeoutDuration(); got != 30*time.Minute {
			t.Fatalf("default idle timeout = %v", got)
		}
		cfg.IdleTimeout = "45s"
		if got := cfg.IdleTimeoutDuration(); got != 45*time.Second {
			t.Fatalf("custom idle timeout = %v", got)
		}
		cfg.IdleTimeout = "not-a-duration"
		if got := cfg.IdleTimeoutDuration(); got != 30*time.Minute {
			t.Fatalf("invalid idle timeout fallback = %v", got)
		}
	})

	t.Run("ChordBinary default and custom", func(t *testing.T) {
		cfg := &Config{}
		if got := cfg.ChordBinary(); got != "chord" {
			t.Fatalf("default ChordBinary = %q", got)
		}
		cfg.ChordPath = "/usr/local/bin/chord"
		if got := cfg.ChordBinary(); got != "/usr/local/bin/chord" {
			t.Fatalf("custom ChordBinary = %q", got)
		}
	})

	t.Run("ActiveIMs and IsMultiIM", func(t *testing.T) {
		cfg := &Config{}
		if got := cfg.ActiveIMs(); got != nil {
			t.Fatalf("ActiveIMs nil config = %v", got)
		}
		cfg.IMs = []IMAdapterConfig{{Wechat: &WechatConfig{BaseURL: "https://api"}}}
		got := cfg.ActiveIMs()
		if len(got) != 1 || got[0].Type() != "wechat" || got[0].Wechat.BaseURL != "https://api" {
			t.Fatalf("ActiveIMs = %#v", got)
		}
		if cfg.IsMultiIM() {
			t.Fatal("single IM adapter should not be multi")
		}
		cfg.IMs = append(cfg.IMs, IMAdapterConfig{Feishu: &FeishuConfig{AppID: "app", AppSecret: "secret"}})
		if !cfg.IsMultiIM() {
			t.Fatal("two IM adapters should be multi")
		}
	})

	t.Run("Feishu allowlist", func(t *testing.T) {
		fc := &FeishuConfig{}
		if !fc.IsOpenIDAllowed("any") {
			t.Fatal("empty allowlist should allow")
		}
		fc.OwnerOpenID = "ou_owner"
		if !fc.IsOpenIDAllowed("ou_owner") {
			t.Fatal("owner should be allowed")
		}
		if fc.IsOpenIDAllowed("ou_x") {
			t.Fatal("non-owner should be denied")
		}
		fc.AllowedOpenIDs = []string{"ou_a"}
		if !fc.IsOpenIDAllowed("ou_a") {
			t.Fatal("allowlisted user should be allowed")
		}
		if fc.IsOpenIDAllowed("ou_x") {
			t.Fatal("non-allowlisted user should be denied")
		}
	})
}
