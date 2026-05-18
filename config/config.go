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
//
// Preferred YAML shape:
//
//	ims:
//	  wechat:
//	    ...
//	  feishu:
//	    ...
//	workspaces:
//	  default:
//	    path: ~/project
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
	Activity  bool `yaml:"activity,omitempty"`
	AgentDone bool `yaml:"agent_done,omitempty"`
	Info      bool `yaml:"info,omitempty"`
	Toast     bool `yaml:"toast,omitempty"`
	Todos     bool `yaml:"todos,omitempty"`
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

// FeishuConfig holds Feishu application configuration.
type FeishuConfig struct {
	AppID          string            `yaml:"app_id"`
	AppSecret      string            `yaml:"app_secret"`
	OwnerOpenID    string            `yaml:"owner_open_id,omitempty"`
	AllowedOpenIDs []string          `yaml:"allowed_open_ids,omitempty"`
	ChatBindings   map[string]string `yaml:"chat_bindings,omitempty"`
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

type rawConfig struct {
	IMs             yaml.Node       `yaml:"ims"`
	Workspaces      yaml.Node       `yaml:"workspaces"`
	ChordPath       string          `yaml:"chord_path,omitempty"`
	IdleTimeout     string          `yaml:"idle_timeout,omitempty"`
	EventVisibility EventVisibility `yaml:"event_visibility,omitempty"`
	SessionPinsFile string          `yaml:"session_pins_file,omitempty"`
}

type rawWorkspace struct {
	ID   string `yaml:"id,omitempty"`
	Path string `yaml:"path"`
}

// UnmarshalYAML accepts the preferred map-based config shape for ims/workspaces.
func (c *Config) UnmarshalYAML(value *yaml.Node) error {
	var raw rawConfig
	if err := value.Decode(&raw); err != nil {
		return err
	}

	ims, err := parseIMsNode(&raw.IMs)
	if err != nil {
		return err
	}
	workspaces, err := parseWorkspacesNode(&raw.Workspaces)
	if err != nil {
		return err
	}

	*c = Config{
		IMs:             ims,
		Workspaces:      workspaces,
		ChordPath:       raw.ChordPath,
		IdleTimeout:     raw.IdleTimeout,
		EventVisibility: raw.EventVisibility,
		SessionPinsFile: raw.SessionPinsFile,
	}
	return nil
}

func parseIMsNode(node *yaml.Node) ([]IMAdapterConfig, error) {
	if node == nil || node.Kind == 0 || node.Tag == "!!null" {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse ims: expected a YAML mapping (e.g. ims: { wechat: ..., feishu: ... })")
	}

	configs := make([]IMAdapterConfig, 0, len(node.Content)/2)
	seen := make(map[string]bool)
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valueNode := node.Content[i+1]
		if keyNode == nil || valueNode == nil {
			continue
		}
		typ := NormalizeIMType(keyNode.Value)
		if seen[typ] {
			return nil, fmt.Errorf("parse ims: duplicate adapter %q", keyNode.Value)
		}
		seen[typ] = true
		switch typ {
		case "wechat":
			var wc WechatConfig
			if err := valueNode.Decode(&wc); err != nil {
				return nil, fmt.Errorf("parse ims.%s: %w", typ, err)
			}
			configs = append(configs, IMAdapterConfig{Wechat: &wc})
		case "feishu":
			var fc FeishuConfig
			if err := valueNode.Decode(&fc); err != nil {
				return nil, fmt.Errorf("parse ims.%s: %w", typ, err)
			}
			configs = append(configs, IMAdapterConfig{Feishu: &fc})
		default:
			return nil, fmt.Errorf("parse ims: unsupported adapter %q", keyNode.Value)
		}
	}
	return configs, nil
}

func parseWorkspacesNode(node *yaml.Node) ([]Workspace, error) {
	if node == nil || node.Kind == 0 || node.Tag == "!!null" {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse workspaces: expected a YAML mapping keyed by workspace id")
	}

	workspaces := make([]Workspace, 0, len(node.Content)/2)
	seen := make(map[string]bool)
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valueNode := node.Content[i+1]
		if keyNode == nil || valueNode == nil {
			continue
		}
		id := strings.TrimSpace(keyNode.Value)
		if seen[id] {
			return nil, fmt.Errorf("parse workspaces: duplicate workspace %q", id)
		}
		seen[id] = true
		var rawWS rawWorkspace
		if err := valueNode.Decode(&rawWS); err != nil {
			return nil, fmt.Errorf("parse workspaces.%s: %w", id, err)
		}
		if rawWS.ID != "" && strings.TrimSpace(rawWS.ID) != id {
			return nil, fmt.Errorf("parse workspaces.%s: nested id %q does not match map key", id, rawWS.ID)
		}
		workspaces = append(workspaces, Workspace{ID: id, Path: rawWS.Path})
	}
	return workspaces, nil
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

// ResolveWorkspace resolves a workspace by IM type and chat ID using routing
// declared under each IM adapter config.
func (c *Config) ResolveWorkspace(imType, chatID string) (*Workspace, error) {
	if len(c.Workspaces) == 0 {
		return nil, fmt.Errorf("no workspace configured")
	}
	imType = NormalizeIMType(imType)

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
			return nil, fmt.Errorf("wechat with multiple workspaces requires ims.wechat.workspace_id to select a single workspace")
		}
		return c.workspaceByID(workspaceID)

	case "feishu":
		if len(c.Workspaces) == 1 && len(imCfg.Feishu.ChatBindings) == 0 {
			return &c.Workspaces[0], nil
		}
		workspaceID := imCfg.Feishu.ChatBindings[chatID]
		if workspaceID == "" {
			return nil, fmt.Errorf("feishu chat_id %q is not bound to any workspace; please configure ims.feishu.chat_bindings in config", chatID)
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
	imType = NormalizeIMType(imType)
	for i := range c.IMs {
		if c.IMs[i].Type() == imType {
			return &c.IMs[i]
		}
	}
	return nil
}

// WorkspaceByID returns the workspace with the given ID, or nil if not found.
func (c *Config) WorkspaceByID(id string) *Workspace {
	for i := range c.Workspaces {
		if c.Workspaces[i].ID == id {
			return &c.Workspaces[i]
		}
	}
	return nil
}

func (c *Config) workspaceByID(id string) (*Workspace, error) {
	ws := c.WorkspaceByID(id)
	if ws == nil {
		return nil, fmt.Errorf("workspace_id %q does not match any configured workspace", id)
	}
	return ws, nil
}

// NormalizeIMType normalizes an IM type name (lowercase + trimmed). Use this
// helper everywhere a user-supplied or config-supplied IM identifier is
// compared so the gateway has one canonical form.
func NormalizeIMType(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// IsAllowedWorkspacePath reports whether a workspace path uses an accepted
// absolute/home-rooted form: Unix absolute, ~, Windows drive absolute, or UNC.
func IsAllowedWorkspacePath(path string) bool {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, "~") || strings.HasPrefix(path, `\\`) {
		return true
	}
	if len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':' && (path[2] == '\\' || path[2] == '/') {
		return true
	}
	return false
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
		if ws.Path == "" {
			return fmt.Errorf("workspace %q requires a non-empty path", ws.ID)
		}
		if !IsAllowedWorkspacePath(ws.Path) {
			return fmt.Errorf("workspace %q path %q must start with /, ~, a Windows drive prefix, or a UNC prefix", ws.ID, ws.Path)
		}
		if prev, ok := workspaceByID[ws.ID]; ok {
			return fmt.Errorf("duplicate workspace id %q in workspaces[%d] and workspaces[%d]", ws.ID, prev, i)
		}
		workspaceByID[ws.ID] = i
	}

	imCounts := make(map[string]int)
	for i := range c.IMs {
		im := &c.IMs[i]
		typ := im.Type()
		if typ == "" {
			return fmt.Errorf("ims entries must contain exactly one platform config")
		}
		if im.Wechat != nil && im.Feishu != nil {
			return fmt.Errorf("ims entries must contain exactly one platform config")
		}
		imCounts[typ]++
		if imCounts[typ] > 1 {
			return fmt.Errorf("ims supports at most one %s adapter", typ)
		}

		switch typ {
		case "wechat":
			if im.Wechat.WorkspaceID == "" {
				if len(c.Workspaces) > 1 {
					return fmt.Errorf("wechat with multiple workspaces requires ims.wechat.workspace_id to select a single workspace")
				}
			} else if _, ok := workspaceByID[im.Wechat.WorkspaceID]; !ok {
				return fmt.Errorf("ims.wechat.workspace_id %q does not match any configured workspace", im.Wechat.WorkspaceID)
			}

		case "feishu":
			if im.Feishu.AppID == "" {
				return fmt.Errorf("ims.feishu.app_id is required")
			}
			if im.Feishu.AppSecret == "" {
				return fmt.Errorf("ims.feishu.app_secret is required")
			}
			if len(c.Workspaces) > 1 && len(im.Feishu.ChatBindings) == 0 {
				return fmt.Errorf("feishu with multiple workspaces requires ims.feishu.chat_bindings")
			}
			for chatID, workspaceID := range im.Feishu.ChatBindings {
				if strings.TrimSpace(chatID) == "" {
					return fmt.Errorf("ims.feishu.chat_bindings contains an empty chat_id")
				}
				if _, ok := workspaceByID[workspaceID]; !ok {
					return fmt.Errorf("ims.feishu.chat_bindings[%q] references unknown workspace %q", chatID, workspaceID)
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
