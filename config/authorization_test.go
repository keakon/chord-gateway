package config

import (
	"errors"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOwnerAuthorizationFields(t *testing.T) {
	for _, tt := range []struct {
		name, data string
		valid      bool
	}{
		{"new", "ims: {wechat: {}}", true},
		{"unregistered with workspaces", "ims: {feishu: {app_id: app, app_secret: secret}}\nworkspaces: {main: {path: /tmp/project}}", true},
		{"legacy owner", "ims: {feishu: {app_id: app, app_secret: secret, owner_open_id: owner}}\nworkspaces: {main: {path: /tmp/project}}", false},
		{"legacy list", "ims: {feishu: {app_id: app, app_secret: secret, allowed_open_ids: [first, second]}}\nworkspaces: {main: {path: /tmp/project}}", false},
		{"legacy fixed", "ims: {wechat: {workspace_id: main}}\nworkspaces: {main: {path: /tmp/project}}", false},
		{"damaged legacy", "ims: {wechat: {workspace_id: main}}", false},
		{"empty legacy field", "ims: {wechat: {workspace_id: ''}}", false},
		{"owner null", "ims: {wechat: {owner: null}}", false},
		{"owner typo", "ims: {wechat: {owner: {account_id: bot, user_id: user, user_ids: other}}}", false},
		{"conflict", "ims: {wechat: {access_mode: paired, workspace_id: ''}}", false},
		{"explicit legacy", "ims: {wechat: {access_mode: legacy, owner: {account_id: bot, user_id: user}}}", false},
		{"owned", "ims: {wechat: {owner: {account_id: bot, user_id: user}}}", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			err := yaml.Unmarshal([]byte(tt.data), &cfg)
			if err == nil {
				err = cfg.Validate()
			}
			if (err == nil) != tt.valid {
				t.Fatalf("validation = %v", err)
			}
		})
	}
}

func TestChatBindingNeverFallsBack(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("ims: {wechat: {}, feishu: {app_id: app, app_secret: secret}}"), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Workspaces = append(cfg.Workspaces, Workspace{ID: "main", Path: "/tmp/project"})
	for _, platform := range []string{"wechat", "feishu"} {
		if _, err := cfg.ResolveWorkspace(platform, "chat"); !errors.Is(err, ErrChatUnbound) {
			t.Fatalf("routing = %v", err)
		}
	}
}
