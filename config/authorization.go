package config

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// OwnerIdentity binds a trusted sender to the connected bot account.
type OwnerIdentity struct {
	AccountID string `yaml:"account_id"`
	UserID    string `yaml:"user_id"`
}

var ErrChatUnbound = errors.New("chat is not bound")

type accessFields struct{ owner bool }

func parseAccessFields(node *yaml.Node) (accessFields, error) {
	var f accessFields
	var invalid bool
	var removed string
	walkYAMLMapping(node, map[string]bool{}, map[*yaml.Node]bool{}, func(k, v *yaml.Node) {
		switch k.Value {
		case "owner":
			f.owner = true
			n := v
			if n.Kind == yaml.AliasNode {
				n = n.Alias
			}
			if n == nil || n.Kind != yaml.MappingNode {
				invalid = true
				return
			}
			walkYAMLMapping(n, map[string]bool{}, map[*yaml.Node]bool{}, func(key, value *yaml.Node) {
				if (key.Value != "account_id" && key.Value != "user_id") || value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
					invalid = true
				}
			})
		case "owner_open_id", "allowed_open_ids", "workspace_id", "access_mode":
			removed = k.Value
		case "chat_bindings":
			if v.Tag == "!!null" {
				return
			}
			n := v
			if n.Kind == yaml.AliasNode {
				n = n.Alias
			}
			if n == nil || n.Kind != yaml.MappingNode {
				invalid = true
				return
			}
			walkYAMLMapping(n, map[string]bool{}, map[*yaml.Node]bool{}, func(key, value *yaml.Node) {
				if key.Tag != "!!str" || value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
					invalid = true
				}
			})
		}
	})
	if removed != "" {
		return f, fmt.Errorf("authorization field %q was removed; use --pair and explicit /bind (see CHANGELOG)", removed)
	}
	if invalid {
		return f, errors.New("invalid authorization field structure")
	}
	return f, nil
}

// Bindings returns the configured explicit routes.
func (im *IMAdapterConfig) Bindings() map[string]string {
	if im.Telegram != nil {
		return im.Telegram.ChatBindings
	}
	if im.Discord != nil {
		return im.Discord.ChatBindings
	}
	if im.Wecom != nil {
		return im.Wecom.ChatBindings
	}
	if im.Dingtalk != nil {
		return im.Dingtalk.ChatBindings
	}
	if im.Slack != nil {
		return im.Slack.ChatBindings
	}

	if im.Feishu != nil {
		return im.Feishu.ChatBindings
	}
	if im.Wechat != nil {
		return im.Wechat.ChatBindings
	}

	return nil
}

// OwnerIdentity returns the paired owner, if registered.
func (im *IMAdapterConfig) OwnerIdentity() *OwnerIdentity {
	if im.Telegram != nil {
		return im.Telegram.Owner
	}
	if im.Discord != nil {
		return im.Discord.Owner
	}
	if im.Wecom != nil {
		return im.Wecom.Owner
	}
	if im.Dingtalk != nil {
		return im.Dingtalk.Owner
	}
	if im.Slack != nil {
		return im.Slack.Owner
	}

	if im.Feishu != nil {
		return im.Feishu.Owner
	}
	if im.Wechat != nil {
		return im.Wechat.Owner
	}
	return nil
}

func (c *Config) validateAccess(im *IMAdapterConfig) error {
	owner := im.OwnerIdentity()
	fields := accessFields{}
	if im.Telegram != nil {
		fields = im.Telegram.fields
	}
	if im.Discord != nil {
		fields = im.Discord.fields
	}
	if im.Wecom != nil {
		fields = im.Wecom.fields
	}
	if im.Dingtalk != nil {
		fields = im.Dingtalk.fields
	}
	if im.Slack != nil {
		fields = im.Slack.fields
	}

	if im.Feishu != nil {
		fields = im.Feishu.fields
	}
	if im.Wechat != nil {
		fields = im.Wechat.fields
	}
	if fields.owner || owner != nil {
		if owner == nil || strings.TrimSpace(owner.AccountID) == "" || strings.TrimSpace(owner.UserID) == "" {
			return errors.New("owner requires account_id and user_id")
		}
	}
	if owner == nil && len(im.Bindings()) > 0 {
		return errors.New("chat_bindings requires a paired owner")
	}
	for chat, id := range im.Bindings() {
		if strings.TrimSpace(chat) == "" || c.WorkspaceByID(id) == nil {
			return errors.New("invalid chat binding")
		}
	}
	if f := im.Feishu; f != nil && (f.AppID == "" || f.AppSecret == "") {
		return errors.New("ims.feishu.app_id and app_secret are required")
	}
	if f := im.Telegram; f != nil && (strings.TrimSpace(f.BotToken) == "") {
		return errors.New("ims.telegram requires bot_token")
	}
	if f := im.Discord; f != nil && (strings.TrimSpace(f.BotToken) == "" || strings.TrimSpace(f.ApplicationID) == "") {
		return errors.New("ims.discord requires bot_token, application_id")
	}
	if f := im.Wecom; f != nil && (strings.TrimSpace(f.BotID) == "" || strings.TrimSpace(f.Secret) == "") {
		return errors.New("ims.wecom requires bot_id, secret")
	}
	if f := im.Dingtalk; f != nil && (strings.TrimSpace(f.ClientID) == "" || strings.TrimSpace(f.ClientSecret) == "" || strings.TrimSpace(f.RobotCode) == "" || strings.TrimSpace(f.CorpID) == "") {
		return errors.New("ims.dingtalk requires client_id, client_secret, robot_code, corp_id")
	}
	if f := im.Slack; f != nil && (strings.TrimSpace(f.AppToken) == "" || strings.TrimSpace(f.BotToken) == "" || strings.TrimSpace(f.AppID) == "" || strings.TrimSpace(f.TeamID) == "") {
		return errors.New("ims.slack requires app_token, bot_token, app_id, team_id")
	}
	return nil
}
