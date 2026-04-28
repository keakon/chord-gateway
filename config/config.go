// Package config defines the gateway configuration model and path resolution.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level gateway configuration.
type Config struct {
	IM          IMConfig          `yaml:"im,omitempty"`
	IMs         []IMAdapterConfig `yaml:"ims,omitempty"`
	Workspaces  []Workspace       `yaml:"workspaces"`
	ChordPath   string            `yaml:"chord_path,omitempty"`
	IdleTimeout string            `yaml:"idle_timeout,omitempty"`

	// WechatWorkspaceID optionally pins all WeChat traffic to the named workspace.
	// This enables multi-IM deployments where WeChat uses one workspace while
	// Feishu routes multiple chats to different workspaces.
	WechatWorkspaceID string `yaml:"wechat_workspace_id,omitempty"`

	// EventVisibility controls which non-essential headless events the gateway
	// subscribes to and surfaces. Essential events are always subscribed:
	// assistant_message / confirm_request / question_request / idle / error /
	// notification.
	EventVisibility EventVisibility `yaml:"event_visibility,omitempty"`

	// SessionPinsFile is an optional override for the session pin store path.
	// If empty, defaults to <state_dir>/session-pins.json.
	SessionPinsFile string `yaml:"session_pins_file,omitempty"`
}

// EventVisibility controls optional control-plane event subscriptions.
type EventVisibility struct {
	Activity   bool `yaml:"activity,omitempty"`
	AgentDone  bool `yaml:"agent_done,omitempty"`
	Info       bool `yaml:"info,omitempty"`
	Toast      bool `yaml:"toast,omitempty"`
	ToolResult bool `yaml:"tool_result,omitempty"`
	Todos      bool `yaml:"todos,omitempty"`
}

// IMConfig describes a single IM platform configuration (backward compat).
type IMConfig struct {
	Type   string        `yaml:"type,omitempty"`
	Wechat *WechatConfig `yaml:"wechat,omitempty"`
	Feishu *FeishuConfig `yaml:"feishu,omitempty"`
}

// IMAdapterConfig describes one IM adapter in multi-IM mode.
type IMAdapterConfig struct {
	Type   string        `yaml:"type"`
	Wechat *WechatConfig `yaml:"wechat,omitempty"`
	Feishu *FeishuConfig `yaml:"feishu,omitempty"`
}

// WechatConfig holds WeChat iLink Bot configuration.
type WechatConfig struct {
	BaseURL string `yaml:"base_url,omitempty"`
	BotType string `yaml:"bot_type,omitempty"`
}

// FeishuConfig holds Feishu (飞书) application configuration.
type FeishuConfig struct {
	AppID             string   `yaml:"app_id"`
	AppSecret         string   `yaml:"app_secret"`
	VerificationToken string   `yaml:"verification_token,omitempty"`
	EncryptKey        string   `yaml:"encrypt_key,omitempty"`
	Listen            string   `yaml:"listen,omitempty"`
	WebhookPath       string   `yaml:"webhook_path,omitempty"`
	OwnerOpenID       string   `yaml:"owner_open_id,omitempty"`
	AllowedOpenIDs    []string `yaml:"allowed_open_ids,omitempty"`
}

// IsOpenIDAllowed checks if an open_id is allowed to send messages.
// If neither owner_open_id nor allowed_open_ids is configured, all are allowed.
func (fc *FeishuConfig) IsOpenIDAllowed(openID string) bool {
	if fc == nil {
		return true
	}
	if fc.OwnerOpenID == "" && len(fc.AllowedOpenIDs) == 0 {
		return true
	}
	if fc.OwnerOpenID == openID {
		return true
	}
	for _, id := range fc.AllowedOpenIDs {
		if id == openID {
			return true
		}
	}
	return false
}

// Workspace maps an IM chat to a project directory.
type Workspace struct {
	ID       string `yaml:"id"`
	Path     string `yaml:"path"`
	IMChatID string `yaml:"im_chat_id,omitempty"`
}

// IdleTimeoutDuration parses and returns the idle timeout duration.
func (c *Config) IdleTimeoutDuration() time.Duration {
	s := c.IdleTimeout
	if s == "" {
		s = "30m"
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 30 * time.Minute
	}
	return d
}

// ChordBinary returns the chord binary path.
func (c *Config) ChordBinary() string {
	if c.ChordPath != "" {
		return c.ChordPath
	}
	return "chord"
}

// ActiveIMs returns the list of active IM adapter configs.
// If IMs (multi-IM) is set, return those. Otherwise fall back to the
// single IM config for backward compatibility.
func (c *Config) ActiveIMs() []IMAdapterConfig {
	if len(c.IMs) > 0 {
		return c.IMs
	}
	if c.IM.Type == "" {
		return nil
	}
	return []IMAdapterConfig{{
		Type:   c.IM.Type,
		Wechat: c.IM.Wechat,
		Feishu: c.IM.Feishu,
	}}
}

// IsMultiIM returns true if more than one IM adapter is configured.
func (c *Config) IsMultiIM() bool {
	return len(c.ActiveIMs()) > 1
}

// FindWorkspace finds a workspace by chat ID (legacy).
// Deprecated: use ResolveWorkspace(imType, chatID) instead.
// Kept only for backward compatibility; new code must use ResolveWorkspace.
func (c *Config) FindWorkspace(chatID string) *Workspace {
	for _, im := range c.ActiveIMs() {
		if im.Type == "wechat" || im.Type == "feishu" {
			if len(c.Workspaces) > 0 {
				return &c.Workspaces[0]
			}
			return nil
		}
	}
	for i := range c.Workspaces {
		if c.Workspaces[i].IMChatID == chatID {
			return &c.Workspaces[i]
		}
	}
	return nil
}

// ResolveWorkspace resolves a workspace by IM type and chat ID, following
// routing rules:
//
//   - wechat:
//   - if wechat_workspace_id is set, route all WeChat traffic to that workspace.
//   - otherwise, route to the first workspace for backward compatibility.
//   - feishu:
//   - single workspace: returns it regardless of im_chat_id.
//   - multiple workspaces: requires exact chatID match on im_chat_id; returns
//     a clear error if no match is found (no fallback to first workspace).
//   - console:
//   - single workspace: returns it (legacy compatibility for stdin/stdout mode).
//   - multiple workspaces: requires exact chatID match on im_chat_id.
//   - other: exact im_chat_id match.
func (c *Config) ResolveWorkspace(imType, chatID string) (*Workspace, error) {
	switch normalizeIMType(imType) {
	case "wechat":
		if len(c.Workspaces) == 0 {
			return nil, fmt.Errorf("no workspace configured")
		}
		if c.WechatWorkspaceID != "" {
			for i := range c.Workspaces {
				if c.Workspaces[i].ID == c.WechatWorkspaceID {
					return &c.Workspaces[i], nil
				}
			}
			return nil, fmt.Errorf("wechat_workspace_id %q does not match any configured workspace", c.WechatWorkspaceID)
		}
		return &c.Workspaces[0], nil

	case "feishu":
		if len(c.Workspaces) == 1 {
			return &c.Workspaces[0], nil
		}
		// Multi-workspace: require exact chatID match.
		for i := range c.Workspaces {
			if c.Workspaces[i].IMChatID == chatID {
				return &c.Workspaces[i], nil
			}
		}
		return nil, fmt.Errorf("feishu chat_id %q is not bound to any workspace; please configure workspaces[].im_chat_id in config", chatID)

	case "console":
		// Keep console fallback behavior compatible with legacy single-workspace
		// mode used by the WeChat adapter when it runs from stdin/stdout.
		if len(c.Workspaces) == 1 {
			return &c.Workspaces[0], nil
		}
		// Multi-workspace: require explicit binding by im_chat_id.
		for i := range c.Workspaces {
			if c.Workspaces[i].IMChatID == chatID {
				return &c.Workspaces[i], nil
			}
		}
		return nil, fmt.Errorf("console chat_id %q is not bound to any workspace; please configure workspaces[].im_chat_id in config", chatID)

	default:
		// other: exact chatID match.
		for i := range c.Workspaces {
			if c.Workspaces[i].IMChatID == chatID {
				return &c.Workspaces[i], nil
			}
		}
		return nil, fmt.Errorf("no workspace configured for chat_id %q", chatID)
	}
}

// normalizeIMType normalizes an IM type name.
func normalizeIMType(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "weixin", "wx":
		return "wechat"
	case "lark":
		return "feishu"
	default:
		return strings.ToLower(strings.TrimSpace(name))
	}
}

// Validate checks the configuration for correctness, returning an error
// for invalid combinations that would cause ambiguous routing.
func (c *Config) Validate() error {
	activeIMs := c.ActiveIMs()

	// Collect which IM types are active.
	ims := make(map[string]bool)
	for _, im := range activeIMs {
		ims[im.Type] = true
	}

	if len(c.Workspaces) == 0 {
		return fmt.Errorf("at least one workspace must be configured")
	}

	workspaceByID := make(map[string]int)
	for i, ws := range c.Workspaces {
		if ws.ID == "" {
			return fmt.Errorf("workspaces[%d] requires a non-empty id", i)
		}
		if prev, ok := workspaceByID[ws.ID]; ok {
			return fmt.Errorf("duplicate workspace id %q in workspaces[%d] and workspaces[%d]", ws.ID, prev, i)
		}
		workspaceByID[ws.ID] = i
	}

	// 1. WeChat routing.
	if ims["wechat"] {
		if len(c.Workspaces) > 1 {
			if c.WechatWorkspaceID == "" {
				return fmt.Errorf("wechat with multiple workspaces requires wechat_workspace_id to select a single workspace")
			}
			if _, ok := workspaceByID[c.WechatWorkspaceID]; !ok {
				return fmt.Errorf("wechat_workspace_id %q does not match any configured workspace", c.WechatWorkspaceID)
			}
		} else if c.WechatWorkspaceID != "" {
			if _, ok := workspaceByID[c.WechatWorkspaceID]; !ok {
				return fmt.Errorf("wechat_workspace_id %q does not match any configured workspace", c.WechatWorkspaceID)
			}
		}
	}

	// 2. Feishu: multi-workspace rules.
	if ims["feishu"] && len(c.Workspaces) > 1 {
		seen := make(map[string]int) // im_chat_id -> workspace index
		for i, ws := range c.Workspaces {
			if ws.IMChatID == "" {
				return fmt.Errorf("feishu multi-workspace: workspaces[%d] (%s) requires a non-empty im_chat_id", i, ws.ID)
			}
			if prev, ok := seen[ws.IMChatID]; ok {
				return fmt.Errorf("feishu multi-workspace: duplicate im_chat_id %q in workspaces[%d] (%s) and workspaces[%d] (%s)",
					ws.IMChatID, prev, c.Workspaces[prev].ID, i, ws.ID)
			}
			seen[ws.IMChatID] = i
		}
	}

	// 3. Feishu: validate owner/allowlist.
	if ims["feishu"] {
		for _, im := range activeIMs {
			if im.Type != "feishu" || im.Feishu == nil {
				continue
			}
			// Deduplicate allowed_open_ids.
			if len(im.Feishu.AllowedOpenIDs) > 0 {
				seen := make(map[string]bool)
				deduped := make([]string, 0, len(im.Feishu.AllowedOpenIDs))
				for _, id := range im.Feishu.AllowedOpenIDs {
					if !seen[id] {
						seen[id] = true
						deduped = append(deduped, id)
					}
				}
				im.Feishu.AllowedOpenIDs = deduped
			}
			// If owner_open_id is set, ensure it's also in allowed_open_ids.
			if im.Feishu.OwnerOpenID != "" && len(im.Feishu.AllowedOpenIDs) > 0 {
				found := false
				for _, id := range im.Feishu.AllowedOpenIDs {
					if id == im.Feishu.OwnerOpenID {
						found = true
						break
					}
				}
				if !found {
					im.Feishu.AllowedOpenIDs = append(im.Feishu.AllowedOpenIDs, im.Feishu.OwnerOpenID)
				}
			}
		}
	}

	return nil
}

// ExpandPath expands ~ in workspace paths and chord path using XDG paths.
func (c *Config) ExpandPath(pathsResolve func(string) string) {
	c.ChordPath = pathsResolve(c.ChordPath)
	for i := range c.Workspaces {
		c.Workspaces[i].Path = pathsResolve(c.Workspaces[i].Path)
	}
}

// Load reads a YAML config file.
func Load(path string) (*Config, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".yaml" && ext != ".yml" {
		return nil, fmt.Errorf("unsupported config file %q: only .yaml and .yml are supported", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.ExpandPath(Expand)
	return &cfg, nil
}
