package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidate_WechatMultiWorkspace_AllowsUnboundChats(t *testing.T) {
	cfg := &Config{
		IMs: []IMAdapterConfig{{Wechat: &WechatConfig{}}},
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1"},
			{ID: "ws2", Path: "/tmp/ws2"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error, got: %v", err)
	}
}

func TestValidate_WechatWorkspaceID_OK(t *testing.T) {
	cfg := &Config{
		IMs: []IMAdapterConfig{{Wechat: &WechatConfig{Owner: &OwnerIdentity{AccountID: "bot", UserID: "user"}, ChatBindings: map[string]string{"chat": "ws1"}}}},
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
		IMs:        []IMAdapterConfig{{Wechat: &WechatConfig{Owner: &OwnerIdentity{AccountID: "bot", UserID: "user"}, ChatBindings: map[string]string{"chat": "missing"}}}},
		Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1"}},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "invalid chat binding") {
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
			{Wechat: &WechatConfig{Owner: &OwnerIdentity{AccountID: "bot", UserID: "user"}, ChatBindings: map[string]string{"chat": "ws1"}}},
		},
		Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1"}},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "at most one wechat adapter") {
		t.Fatalf("expected duplicate wechat adapter error, got: %v", err)
	}
}

func TestValidate_FeishuMultiWorkspace_AllowsUnboundChats(t *testing.T) {
	cfg := &Config{
		IMs: []IMAdapterConfig{{Feishu: &FeishuConfig{AppID: "app", AppSecret: "secret"}}},
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1"},
			{ID: "ws2", Path: "/tmp/ws2"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unbound configuration rejected: %v", err)
	}
}

func TestValidate_FeishuChatBindingsUnknownWorkspace(t *testing.T) {
	cfg := &Config{
		IMs: []IMAdapterConfig{{Feishu: &FeishuConfig{AppID: "app", AppSecret: "secret", Owner: &OwnerIdentity{AccountID: "app", UserID: "user"}, ChatBindings: map[string]string{"oc_1": "missing"}}}},
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1"},
			{ID: "ws2", Path: "/tmp/ws2"},
		},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "invalid chat binding") {
		t.Fatalf("expected unknown workspace error, got: %v", err)
	}
}

func TestResolveWorkspace_Wechat_UsesConfiguredWorkspace(t *testing.T) {
	cfg := &Config{
		IMs: []IMAdapterConfig{{Wechat: &WechatConfig{Owner: &OwnerIdentity{AccountID: "bot", UserID: "user"}, ChatBindings: map[string]string{"chat": "ws2"}}}},
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1"},
			{ID: "ws2", Path: "/tmp/ws2"},
		},
	}
	ws, err := cfg.ResolveWorkspace("wechat", "chat")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ws.ID != "ws2" {
		t.Fatalf("got workspace %q, want %q", ws.ID, "ws2")
	}
}

func TestResolveWorkspace_Wechat_ConfiguredWorkspaceMissing(t *testing.T) {
	cfg := &Config{
		IMs:        []IMAdapterConfig{{Wechat: &WechatConfig{Owner: &OwnerIdentity{AccountID: "bot", UserID: "user"}, ChatBindings: map[string]string{"chat": "missing"}}}},
		Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1"}},
	}
	_, err := cfg.ResolveWorkspace("wechat", "chat")
	if err == nil || !strings.Contains(err.Error(), `workspace_id "missing"`) {
		t.Fatalf("expected missing workspace error, got: %v", err)
	}
}

func TestResolveWorkspace_Feishu_SingleWorkspaceRequiresBinding(t *testing.T) {
	cfg := &Config{IMs: []IMAdapterConfig{{Feishu: &FeishuConfig{AppID: "app", AppSecret: "secret"}}}, Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}
	if _, err := cfg.ResolveWorkspace("feishu", "new-chat"); err == nil {
		t.Fatal("unbound chat reached the only workspace")
	}
	cfg.IMs[0].Feishu.ChatBindings = map[string]string{"known-chat": "ws1"}
	ws, err := cfg.ResolveWorkspace("feishu", "known-chat")
	if err != nil || ws.ID != "ws1" {
		t.Fatal("existing binding was lost")
	}
}

func TestResolveWorkspace_Feishu_MultiWorkspace(t *testing.T) {
	cfg := &Config{
		IMs: []IMAdapterConfig{{Feishu: &FeishuConfig{AppID: "app", AppSecret: "secret", Owner: &OwnerIdentity{AccountID: "app", UserID: "user"}, ChatBindings: map[string]string{"oc_2": "ws2"}}}},
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
		IMs: []IMAdapterConfig{{Feishu: &FeishuConfig{AppID: "app", AppSecret: "secret", Owner: &OwnerIdentity{AccountID: "app", UserID: "user"}, ChatBindings: map[string]string{"oc_1": "ws1"}}}},
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1"},
			{ID: "ws2", Path: "/tmp/ws2"},
		},
	}
	_, err := cfg.ResolveWorkspace("feishu", "oc_missing")
	if err == nil || !strings.Contains(err.Error(), "chat is not bound") {
		t.Fatalf("expected feishu no-match error, got: %v", err)
	}
}

func TestResolveWorkspace_Console_SingleWorkspace(t *testing.T) {
	cfg := &Config{Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}
	ws, err := cfg.ResolveWorkspace("console", "chat")
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
	if err == nil || !strings.Contains(err.Error(), "exactly one workspace") {
		t.Fatalf("expected console no-match error, got: %v", err)
	}
}

func TestResolveWorkspace_UnknownAdapterRejected(t *testing.T) {
	cfg := &Config{Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}
	if _, err := cfg.ResolveWorkspace("custom", "chat"); err == nil {
		t.Fatal("unknown adapter routed")
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

func TestLoad_EventVisibilitySubAgentEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `ims:
  wechat: {}
workspaces:
  ws1:
    path: /tmp/ws1
event_visibility:
  agent_started: true
  agent_notify: true
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.EventVisibility.AgentStarted || !cfg.EventVisibility.AgentNotify {
		t.Fatalf("EventVisibility = %#v", cfg.EventVisibility)
	}
}

func TestLoad_EventVisibilityBackgroundResult(t *testing.T) {
	load := func(t *testing.T, eventVisibility string) EventVisibility {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		content := "ims:\n  wechat: {}\nworkspaces:\n  ws1:\n    path: ~/project\n" + eventVisibility
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		return cfg.EventVisibility
	}

	// background_result is an opt-out flag: omitted means enabled.
	if v := load(t, ""); !v.BackgroundResultEnabled() || v.BackgroundResult != nil {
		t.Fatalf("omitted background_result = %#v, want enabled", v)
	}
	if v := load(t, "event_visibility:\n  background_result: false\n"); v.BackgroundResultEnabled() {
		t.Fatalf("background_result: false = %#v, want disabled", v)
	}
	if v := load(t, "event_visibility:\n  background_result: true\n"); !v.BackgroundResultEnabled() {
		t.Fatalf("background_result: true = %#v, want enabled", v)
	}
}

func TestLoad_ExampleConfig(t *testing.T) {
	path := filepath.Join("..", "config.example.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) error = %v", path, err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate example config: %v", err)
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

}
