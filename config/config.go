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
	IMs             []IMAdapterConfig `yaml:"ims"`
	Workspaces      []Workspace       `yaml:"workspaces"`
	ChordPath       string            `yaml:"chord_path,omitempty"`
	IdleTimeout     string            `yaml:"idle_timeout,omitempty"`
	EventVisibility EventVisibility   `yaml:"event_visibility,omitempty"`
	SessionPinsFile string            `yaml:"session_pins_file,omitempty"`
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

// IMAdapterConfig describes one IM adapter in the gateway config.
type IMAdapterConfig struct {
	Wechat *WechatConfig `yaml:"wechat,omitempty"`
	Feishu *FeishuConfig `yaml:"feishu,omitempty"`
}

// Type returns the normalized adapter type name for this config entry.
func (c IMAdapterConfig) Type() string {
	switch {
	case c.Wechat != nil:
		return "wechat"
	case c.Feishu != nil:
		return "feishu"
	default:
		return ""
	}
}

// WechatConfig holds WeChat iLink Bot configuration.
type WechatConfig struct {
	BaseURL     string `yaml:"base_url,omitempty"`
	WorkspaceID string `yaml:"workspace_id,omitempty"`
	TokenPath   string `yaml:"token_path,omitempty"`
}

// FeishuConfig holds Feishu (飞书) application configuration.
type FeishuConfig struct {
	AppID             string            `yaml:"app_id"`
	AppSecret         string            `yaml:"app_secret"`
	VerificationToken string            `yaml:"verification_token,omitempty"`
	EncryptKey        string            `yaml:"encrypt_key,omitempty"`
	Listen            string            `yaml:"listen,omitempty"`
	WebhookPath       string            `yaml:"webhook_path,omitempty"`
	OwnerOpenID       string            `yaml:"owner_open_id,omitempty"`
	AllowedOpenIDs    []string          `yaml:"allowed_open_ids,omitempty"`
	ChatBindings      map[string]string `yaml:"chat_bindings,omitempty"`
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

// Workspace defines a project directory.
type Workspace struct {
	ID   string `yaml:"id"`
	Path string `yaml:"path"`
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

// ActiveIMs returns the configured IM adapter list.
func (c *Config) ActiveIMs() []IMAdapterConfig {
	if len(c.IMs) == 0 {
		return nil
	}
	return c.IMs
}

// IsMultiIM returns true if more than one IM adapter is configured.
func (c *Config) IsMultiIM() bool {
	return len(c.IMs) > 1
}

// FindWorkspace returns the single configured workspace when unambiguous.
// Deprecated: use ResolveWorkspace(imType, chatID) instead.
func (c *Config) FindWorkspace(chatID string) *Workspace {
	if len(c.Workspaces) == 1 {
		return &c.Workspaces[0]
	}
	return nil
}

// ResolveWorkspace resolves a workspace by IM type and chat ID using routing
// declared under each IM adapter config.
func (c *Config) ResolveWorkspace(imType, chatID string) (*Workspace, error) {
	if len(c.Workspaces) == 0 {
		return nil, fmt.Errorf("no workspace configured")
	}
	imType = normalizeIMType(imType)

	if imType == "console" {
		if len(c.Workspaces) == 1 {
			return &c.Workspaces[0], nil
		}
		return nil, fmt.Errorf("console chat_id %q is not bound to any workspace; console mode requires exactly one workspace", chatID)
	}

	imCfg := c.IMConfigByType(imType)
	if imCfg == nil {
		if len(c.Workspaces) == 1 {
			return &c.Workspaces[0], nil
		}
		return nil, fmt.Errorf("no %s adapter configured for workspace routing", imType)
	}

	switch imType {
	case "wechat":
		workspaceID := ""
		if imCfg.Wechat != nil {
			workspaceID = imCfg.Wechat.WorkspaceID
		}
		if workspaceID == "" {
			if len(c.Workspaces) == 1 {
				return &c.Workspaces[0], nil
			}
			return nil, fmt.Errorf("wechat with multiple workspaces requires ims[].wechat.workspace_id to select a single workspace")
		}
		return c.workspaceByID(workspaceID)

	case "feishu":
		if len(c.Workspaces) == 1 && len(imCfg.Feishu.ChatBindings) == 0 {
			return &c.Workspaces[0], nil
		}
		workspaceID := imCfg.Feishu.ChatBindings[chatID]
		if workspaceID == "" {
			return nil, fmt.Errorf("feishu chat_id %q is not bound to any workspace; please configure ims[].feishu.chat_bindings in config", chatID)
		}
		return c.workspaceByID(workspaceID)

	default:
		if len(c.Workspaces) == 1 {
			return &c.Workspaces[0], nil
		}
		return nil, fmt.Errorf("no workspace routing configured for IM type %q and chat_id %q", imType, chatID)
	}
}

// IMConfigByType returns the first configured adapter matching imType.
func (c *Config) IMConfigByType(imType string) *IMAdapterConfig {
	imType = normalizeIMType(imType)
	for i := range c.IMs {
		if c.IMs[i].Type() == imType {
			return &c.IMs[i]
		}
	}
	return nil
}

func (c *Config) workspaceByID(id string) (*Workspace, error) {
	for i := range c.Workspaces {
		if c.Workspaces[i].ID == id {
			return &c.Workspaces[i], nil
		}
	}
	return nil, fmt.Errorf("workspace_id %q does not match any configured workspace", id)
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

	if len(activeIMs) == 0 {
		return fmt.Errorf("at least one IM adapter must be configured in ims")
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

	for i := range c.IMs {
		im := &c.IMs[i]
		typ := im.Type()
		if typ == "" {
			return fmt.Errorf("ims[%d] must contain exactly one platform config", i)
		}
		if im.Wechat != nil && im.Feishu != nil {
			return fmt.Errorf("ims[%d] must contain exactly one platform config", i)
		}

		switch typ {
		case "wechat":
			if im.Wechat.WorkspaceID == "" {
				if len(c.Workspaces) > 1 {
					return fmt.Errorf("wechat with multiple workspaces requires ims[%d].wechat.workspace_id to select a single workspace", i)
				}
			} else if _, ok := workspaceByID[im.Wechat.WorkspaceID]; !ok {
				return fmt.Errorf("ims[%d].wechat.workspace_id %q does not match any configured workspace", i, im.Wechat.WorkspaceID)
			}

		case "feishu":
			if im.Feishu.AppID == "" {
				return fmt.Errorf("ims[%d].feishu.app_id is required", i)
			}
			if im.Feishu.AppSecret == "" {
				return fmt.Errorf("ims[%d].feishu.app_secret is required", i)
			}
			if len(c.Workspaces) > 1 && len(im.Feishu.ChatBindings) == 0 {
				return fmt.Errorf("feishu with multiple workspaces requires ims[%d].feishu.chat_bindings", i)
			}
			for chatID, workspaceID := range im.Feishu.ChatBindings {
				if strings.TrimSpace(chatID) == "" {
					return fmt.Errorf("ims[%d].feishu.chat_bindings contains an empty chat_id", i)
				}
				if _, ok := workspaceByID[workspaceID]; !ok {
					return fmt.Errorf("ims[%d].feishu.chat_bindings[%q] references unknown workspace %q", i, chatID, workspaceID)
				}
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

// ExpandPath expands ~ in workspace paths, chord path, session pin path, and
// adapter-specific state paths using XDG paths.
func (c *Config) ExpandPath(pathsResolve func(string) string) {
	c.ChordPath = pathsResolve(c.ChordPath)
	c.SessionPinsFile = pathsResolve(c.SessionPinsFile)
	for i := range c.Workspaces {
		c.Workspaces[i].Path = pathsResolve(c.Workspaces[i].Path)
	}
	for i := range c.IMs {
		if c.IMs[i].Wechat != nil {
			c.IMs[i].Wechat.TokenPath = pathsResolve(c.IMs[i].Wechat.TokenPath)
		}
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
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.ExpandPath(Expand)
	return &cfg, nil
}
