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
		IM: IMConfig{Type: "wechat"},
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1"},
			{ID: "ws2", Path: "/tmp/ws2"},
		},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "wechat_workspace_id") {
		t.Fatalf("expected wechat_workspace_id validation error, got: %v", err)
	}
}

func TestValidate_WechatMultiWorkspace_WithWorkspaceID(t *testing.T) {
	cfg := &Config{
		IM:                IMConfig{Type: "wechat"},
		WechatWorkspaceID: "ws1",
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1"},
			{ID: "ws2", Path: "/tmp/ws2"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestValidate_WechatWorkspaceID_NotFound(t *testing.T) {
	cfg := &Config{
		IM:                IMConfig{Type: "wechat"},
		WechatWorkspaceID: "missing",
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1"},
		},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), `wechat_workspace_id "missing"`) {
		t.Fatalf("expected missing wechat_workspace_id error, got: %v", err)
	}
}

func TestValidate_FeishuMultiWorkspace_RequiresChatIDs(t *testing.T) {
	cfg := &Config{
		IM: IMConfig{Type: "feishu", Feishu: &FeishuConfig{AppID: "app", AppSecret: "secret"}},
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1", IMChatID: "oc_1"},
			{ID: "ws2", Path: "/tmp/ws2"},
		},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "requires a non-empty im_chat_id") {
		t.Fatalf("expected im_chat_id validation error, got: %v", err)
	}
}

func TestValidate_FeishuMultiWorkspace_DuplicateChatID(t *testing.T) {
	cfg := &Config{
		IM: IMConfig{Type: "feishu", Feishu: &FeishuConfig{AppID: "app", AppSecret: "secret"}},
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1", IMChatID: "oc_dup"},
			{ID: "ws2", Path: "/tmp/ws2", IMChatID: "oc_dup"},
		},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate im_chat_id") {
		t.Fatalf("expected duplicate im_chat_id error, got: %v", err)
	}
}

func TestValidate_DedupesAllowedOpenIDs_AndAddsOwner(t *testing.T) {
	cfg := &Config{
		IM: IMConfig{Type: "feishu", Feishu: &FeishuConfig{
			AppID:          "app",
			AppSecret:      "secret",
			OwnerOpenID:    "ou_owner",
			AllowedOpenIDs: []string{"ou_a", "ou_a"},
		}},
		Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	got := cfg.IM.Feishu.AllowedOpenIDs
	if len(got) != 2 || got[0] != "ou_a" || got[1] != "ou_owner" {
		t.Fatalf("AllowedOpenIDs = %v", got)
	}
}

func TestResolveWorkspace_Wechat_UsesPinnedWorkspace(t *testing.T) {
	cfg := &Config{
		WechatWorkspaceID: "ws2",
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

func TestResolveWorkspace_Wechat_PinnedWorkspaceMissing(t *testing.T) {
	cfg := &Config{
		WechatWorkspaceID: "missing",
		Workspaces:        []Workspace{{ID: "ws1", Path: "/tmp/ws1"}},
	}
	_, err := cfg.ResolveWorkspace("wechat", "any-chat-id")
	if err == nil || !strings.Contains(err.Error(), `wechat_workspace_id "missing"`) {
		t.Fatalf("expected missing workspace error, got: %v", err)
	}
}

func TestResolveWorkspace_Feishu_SingleWorkspace(t *testing.T) {
	cfg := &Config{Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}
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
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1", IMChatID: "oc_1"},
			{ID: "ws2", Path: "/tmp/ws2", IMChatID: "oc_2"},
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
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1", IMChatID: "oc_1"},
			{ID: "ws2", Path: "/tmp/ws2", IMChatID: "oc_2"},
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
			{ID: "ws1", Path: "/tmp/ws1", IMChatID: "oc_1"},
			{ID: "ws2", Path: "/tmp/ws2", IMChatID: "oc_2"},
		},
	}
	_, err := cfg.ResolveWorkspace("console", "console-chat")
	if err == nil || !strings.Contains(err.Error(), "console chat_id") {
		t.Fatalf("expected console no-match error, got: %v", err)
	}
}

func TestResolveWorkspace_OtherByChatID(t *testing.T) {
	cfg := &Config{Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1", IMChatID: "custom-chat"}}}
	ws, err := cfg.ResolveWorkspace("custom", "custom-chat")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ws.ID != "ws1" {
		t.Fatalf("got workspace %q, want %q", ws.ID, "ws1")
	}
}

func TestResolveWorkspace_OtherNoMatch(t *testing.T) {
	cfg := &Config{Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1", IMChatID: "custom-chat"}}}
	_, err := cfg.ResolveWorkspace("custom", "missing")
	if err == nil || !strings.Contains(err.Error(), `no workspace configured for chat_id "missing"`) {
		t.Fatalf("expected other no-match error, got: %v", err)
	}
}

func TestLoad_UnknownFieldErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`
workspaces:
  - id: ws1
    path: ~/project
storage_dir: /tmp/x
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "storage_dir") {
		t.Fatalf("expected unknown field error mentioning storage_dir, got: %v", err)
	}
}

func TestLoad_RejectsNonYAMLFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	content := []byte(`{"workspaces":[{"id":"ws1","path":"~/project"}]}`)
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
		cfg.IdleTimeout = "bad"
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
		cfg.IM = IMConfig{Type: "wechat", Wechat: &WechatConfig{BaseURL: "https://api"}}
		got := cfg.ActiveIMs()
		if len(got) != 1 || got[0].Type != "wechat" || got[0].Wechat.BaseURL != "https://api" {
			t.Fatalf("ActiveIMs fallback = %#v", got)
		}
		if cfg.IsMultiIM() {
			t.Fatal("single IM should not be multi")
		}
		cfg.IMs = []IMAdapterConfig{{Type: "wechat"}, {Type: "feishu"}}
		if !cfg.IsMultiIM() {
			t.Fatal("two IMs should be multi")
		}
	})

	t.Run("FindWorkspace backward compatibility", func(t *testing.T) {
		cfg := &Config{IM: IMConfig{Type: "wechat"}, Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1"}, {ID: "ws2", Path: "/tmp/ws2"}}}
		if ws := cfg.FindWorkspace("chat"); ws == nil || ws.ID != "ws1" {
			t.Fatalf("FindWorkspace wechat = %#v", ws)
		}
		cfg = &Config{Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1", IMChatID: "chat-1"}, {ID: "ws2", Path: "/tmp/ws2", IMChatID: "chat-2"}}}
		if ws := cfg.FindWorkspace("chat-2"); ws == nil || ws.ID != "ws2" {
			t.Fatalf("FindWorkspace by chatID = %#v", ws)
		}
		if ws := cfg.FindWorkspace("missing"); ws != nil {
			t.Fatalf("FindWorkspace missing = %#v", ws)
		}
	})

	t.Run("ExpandPath expands chord path and workspaces", func(t *testing.T) {
		cfg := &Config{ChordPath: "~/bin/chord", Workspaces: []Workspace{{ID: "ws1", Path: "~/project"}}}
		cfg.ExpandPath(func(p string) string { return strings.ReplaceAll(p, "~", "/home/test") })
		if cfg.ChordPath != "/home/test/bin/chord" || cfg.Workspaces[0].Path != "/home/test/project" {
			t.Fatalf("expanded cfg = %#v", cfg)
		}
	})
}

func TestFeishuConfig_IsOpenIDAllowed(t *testing.T) {
	var fc *FeishuConfig
	if !fc.IsOpenIDAllowed("any") {
		t.Fatal("nil config should allow everyone")
	}
	fc = &FeishuConfig{}
	if !fc.IsOpenIDAllowed("any") {
		t.Fatal("empty allowlist should allow everyone")
	}
	fc = &FeishuConfig{OwnerOpenID: "ou_owner", AllowedOpenIDs: []string{"ou_a", "ou_b"}}
	if !fc.IsOpenIDAllowed("ou_owner") {
		t.Fatal("owner should be allowed even if not in allowlist")
	}
	if !fc.IsOpenIDAllowed("ou_a") {
		t.Fatal("allowlisted user should be allowed")
	}
	if fc.IsOpenIDAllowed("ou_x") {
		t.Fatal("non-allowlisted user should be denied")
	}
}
