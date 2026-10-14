// Package config defines the gateway configuration model and path resolution.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/keakon/golog/log"

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
	ChordStateDir   string            `yaml:"chord_state_dir,omitempty"`
	ChordPath       string            `yaml:"chord_path,omitempty"`
	IdleTimeout     string            `yaml:"idle_timeout,omitempty"`
	EventVisibility EventVisibility   `yaml:"event_visibility,omitempty"`
	SessionPinsFile string            `yaml:"session_pins_file,omitempty"`
}

// EventVisibility controls control-plane event visibility. Most flags are
// opt-in booleans that default to false. BackgroundResult is an opt-out
// pointer instead: an omitted value keeps background results enabled, and only
// an explicit false turns them off.
type EventVisibility struct {
	AgentStarted     bool  `yaml:"agent_started,omitempty"`
	AgentNotify      bool  `yaml:"agent_notify,omitempty"`
	BackgroundResult *bool `yaml:"background_result,omitempty"`
	ContextNotice    bool  `yaml:"context_notice,omitempty"`
	Info             bool  `yaml:"info,omitempty"`
	Toast            bool  `yaml:"toast,omitempty"`
	Todos            bool  `yaml:"todos,omitempty"`
}

// BackgroundResultEnabled reports whether finished background job results are
// pushed to IM. They are delivered by default because a job usually finishes
// after the chat went idle and Chord has no other delivery channel for it;
// event_visibility.background_result: false opts a noisy chat out.
func (v EventVisibility) BackgroundResultEnabled() bool {
	return v.BackgroundResult == nil || *v.BackgroundResult
}

// IMAdapterConfig describes one IM adapter in the gateway config.
type IMAdapterConfig struct {
	Telegram *TelegramConfig `yaml:"telegram,omitempty"`
	Discord  *DiscordConfig  `yaml:"discord,omitempty"`
	Wecom    *WecomConfig    `yaml:"wecom,omitempty"`
	Dingtalk *DingtalkConfig `yaml:"dingtalk,omitempty"`
	Slack    *SlackConfig    `yaml:"slack,omitempty"`

	Wechat *WechatConfig `yaml:"wechat,omitempty"`
	Feishu *FeishuConfig `yaml:"feishu,omitempty"`
}

// Type returns the normalized adapter type name for this config entry.
func (im IMAdapterConfig) Type() string {
	switch {
	case im.Telegram != nil:
		return "telegram"
	case im.Discord != nil:
		return "discord"
	case im.Wecom != nil:
		return "wecom"
	case im.Dingtalk != nil:
		return "dingtalk"
	case im.Slack != nil:
		return "slack"
	case im.Wechat != nil:
		return "wechat"
	case im.Feishu != nil:
		return "feishu"
	default:
		return ""
	}
}

// WechatConfig holds WeChat iLink Bot configuration.
type WechatConfig struct {
	Owner        *OwnerIdentity    `yaml:"owner,omitempty"`
	ChatBindings map[string]string `yaml:"chat_bindings,omitempty"`
	fields       accessFields

	BaseURL   string `yaml:"base_url,omitempty"`
	TokenPath string `yaml:"token_path,omitempty"`
}

// FeishuConfig holds Feishu application configuration.
type FeishuConfig struct {
	Owner  *OwnerIdentity `yaml:"owner,omitempty"`
	fields accessFields

	AppID     string `yaml:"app_id"`
	AppSecret string `yaml:"app_secret"`

	ChatBindings map[string]string `yaml:"chat_bindings,omitempty"`
}

type TelegramConfig struct {
	Owner        *OwnerIdentity    `yaml:"owner,omitempty"`
	ChatBindings map[string]string `yaml:"chat_bindings,omitempty"`
	fields       accessFields
	BotToken     string `yaml:"bot_token"`
}

type DiscordConfig struct {
	Owner         *OwnerIdentity    `yaml:"owner,omitempty"`
	ChatBindings  map[string]string `yaml:"chat_bindings,omitempty"`
	fields        accessFields
	BotToken      string `yaml:"bot_token"`
	ApplicationID string `yaml:"application_id"`
}

type WecomConfig struct {
	Owner        *OwnerIdentity    `yaml:"owner,omitempty"`
	ChatBindings map[string]string `yaml:"chat_bindings,omitempty"`
	fields       accessFields
	BotID        string `yaml:"bot_id"`
	Secret       string `yaml:"secret"`
}

type DingtalkConfig struct {
	Owner        *OwnerIdentity    `yaml:"owner,omitempty"`
	ChatBindings map[string]string `yaml:"chat_bindings,omitempty"`
	fields       accessFields
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	RobotCode    string `yaml:"robot_code"`
	CorpID       string `yaml:"corp_id"`
}

type SlackConfig struct {
	Owner        *OwnerIdentity    `yaml:"owner,omitempty"`
	ChatBindings map[string]string `yaml:"chat_bindings,omitempty"`
	fields       accessFields
	AppToken     string `yaml:"app_token"`
	BotToken     string `yaml:"bot_token"`
	AppID        string `yaml:"app_id"`
	TeamID       string `yaml:"team_id"`
}

// Workspace defines a project directory.
type Workspace struct {
	ID   string `yaml:"id"`
	Path string `yaml:"path"`
}

type rawConfig struct {
	IMs             yaml.Node       `yaml:"ims"`
	Workspaces      yaml.Node       `yaml:"workspaces"`
	ChordStateDir   string          `yaml:"chord_state_dir,omitempty"`
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
		ChordStateDir:   raw.ChordStateDir,
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
		case "telegram":
			var entry TelegramConfig
			if err := valueNode.Decode(&entry); err != nil {
				return nil, fmt.Errorf("parse ims.telegram: %w", err)
			}
			var err error
			entry.fields, err = parseAccessFields(valueNode)
			if err != nil {
				return nil, err
			}
			configs = append(configs, IMAdapterConfig{Telegram: &entry})
		case "discord":
			var entry DiscordConfig
			if err := valueNode.Decode(&entry); err != nil {
				return nil, fmt.Errorf("parse ims.discord: %w", err)
			}
			var err error
			entry.fields, err = parseAccessFields(valueNode)
			if err != nil {
				return nil, err
			}
			configs = append(configs, IMAdapterConfig{Discord: &entry})
		case "wecom":
			var entry WecomConfig
			if err := valueNode.Decode(&entry); err != nil {
				return nil, fmt.Errorf("parse ims.wecom: %w", err)
			}
			var err error
			entry.fields, err = parseAccessFields(valueNode)
			if err != nil {
				return nil, err
			}
			configs = append(configs, IMAdapterConfig{Wecom: &entry})
		case "dingtalk":
			var entry DingtalkConfig
			if err := valueNode.Decode(&entry); err != nil {
				return nil, fmt.Errorf("parse ims.dingtalk: %w", err)
			}
			var err error
			entry.fields, err = parseAccessFields(valueNode)
			if err != nil {
				return nil, err
			}
			configs = append(configs, IMAdapterConfig{Dingtalk: &entry})
		case "slack":
			var entry SlackConfig
			if err := valueNode.Decode(&entry); err != nil {
				return nil, fmt.Errorf("parse ims.slack: %w", err)
			}
			var err error
			entry.fields, err = parseAccessFields(valueNode)
			if err != nil {
				return nil, err
			}
			configs = append(configs, IMAdapterConfig{Slack: &entry})
		case "wechat":
			var wc WechatConfig
			var err error
			if err := valueNode.Decode(&wc); err != nil {
				return nil, fmt.Errorf("parse ims.%s: %w", typ, err)
			}
			wc.fields, err = parseAccessFields(valueNode)
			if err != nil {
				return nil, err
			}
			configs = append(configs, IMAdapterConfig{Wechat: &wc})
		case "feishu":
			var fc FeishuConfig
			var err error
			if err := valueNode.Decode(&fc); err != nil {
				return nil, fmt.Errorf("parse ims.%s: %w", typ, err)
			}
			fc.fields, err = parseAccessFields(valueNode)
			if err != nil {
				return nil, err
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
	imType = NormalizeIMType(imType)
	if imType == "console" {
		if len(c.Workspaces) == 1 {
			return &c.Workspaces[0], nil
		}
		return nil, fmt.Errorf("console requires exactly one workspace")
	}
	im := c.IMConfigByType(imType)
	if im == nil {
		return nil, fmt.Errorf("no %s adapter configured", imType)
	}
	id := im.Bindings()[chatID]
	if id == "" {
		return nil, ErrChatUnbound
	}
	return c.workspaceByID(id)
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
		count := 0
		if im.Wechat != nil {
			count++
		}
		if im.Feishu != nil {
			count++
		}
		if im.Telegram != nil {
			count++
		}
		if im.Discord != nil {
			count++
		}
		if im.Wecom != nil {
			count++
		}
		if im.Dingtalk != nil {
			count++
		}
		if im.Slack != nil {
			count++
		}
		if count != 1 {
			return fmt.Errorf("ims entries must contain exactly one platform config")
		}
		imCounts[typ]++
		if imCounts[typ] > 1 {
			return fmt.Errorf("ims supports at most one %s adapter", typ)
		}

		if err := c.validateAccess(im); err != nil {
			return err
		}
	}

	return nil
}

// ExpandPath expands ~ in workspace paths, chord path, session pin path, and
// adapter-specific state paths using XDG paths.
func (c *Config) ExpandPath(pathsResolve func(string) string) {
	c.ChordPath = pathsResolve(c.ChordPath)
	c.ChordStateDir = pathsResolve(c.ChordStateDir)
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
	cfg, doc, err := ParseDocument(data)
	if err != nil {
		return nil, err
	}
	for _, issue := range UnsupportedConfigFields(doc) {
		log.Warnf("config %s: unsupported field ignored: %s", path, issue)
	}
	return cfg, nil
}
