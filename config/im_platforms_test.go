package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestNewPlatformConfiguration(t *testing.T) {
	platforms := map[string]string{"telegram": "bot_token: placeholder", "discord": "bot_token: placeholder, application_id: app", "wecom": "bot_id: bot, secret: placeholder", "dingtalk": "client_id: client, client_secret: placeholder, robot_code: robot, corp_id: corp", "slack": "app_token: placeholder, bot_token: placeholder, app_id: app, team_id: team"}
	for platform, fields := range platforms {
		t.Run(platform, func(t *testing.T) {
			input := "ims: {" + platform + ": {" + fields + ", owner: {account_id: account, user_id: user}, chat_bindings: {chat: project}}}\nworkspaces: {project: {path: /project}}"
			cfg, doc, err := ParseDocument([]byte(input))
			if err != nil {
				t.Fatal(err)
			}
			if err = cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			if issues := UnsupportedConfigFields(doc); len(issues) != 0 {
				t.Fatal(issues)
			}
			im := cfg.IMConfigByType(platform)
			if im == nil || im.OwnerIdentity().UserID != "user" || im.Bindings()["chat"] != "project" {
				t.Fatal(im)
			}
			var missing Config
			if err = yaml.Unmarshal([]byte("ims: {"+platform+": {}}"), &missing); err != nil {
				t.Fatal(err)
			}
			if err = missing.Validate(); err == nil || strings.Contains(err.Error(), "placeholder") {
				t.Fatalf("credential validation: %v", err)
			}
			for _, access := range []string{"owner: null", "owner: {account_id: account, user_id: user, extra: invalid}", "allowed_open_ids: [user]", "chat_bindings: {chat: project}"} {
				candidate, _, e := ParseDocument([]byte("ims: {" + platform + ": {" + fields + ", " + access + "}}\nworkspaces: {project: {path: /project}}"))
				if e == nil {
					e = candidate.Validate()
				}
				if e == nil {
					t.Fatalf("accepted invalid access %s", access)
				}
			}
		})
	}
}

func TestDiscordRequiresBothCredentials(t *testing.T) {
	for _, fields := range []string{
		"bot_token: example-token",
		"application_id: '123456789012345678'",
		"bot_token: ' ', application_id: '123456789012345678'",
		"bot_token: example-token, application_id: ' '",
	} {
		cfg, _, err := ParseDocument([]byte("ims: {discord: {" + fields + "}}"))
		if err != nil {
			t.Fatal(err)
		}
		if err = cfg.Validate(); err == nil || err.Error() != "ims.discord requires bot_token, application_id" {
			t.Fatalf("credential fields %q: %v", fields, err)
		}
	}
}
