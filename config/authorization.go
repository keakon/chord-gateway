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
	return nil
}
