package config

import (
	"strings"
	"testing"
)

func TestSlackCredentialsRequiredIndividually(t *testing.T) {
	for _, field := range []string{"app_token", "bot_token", "app_id", "team_id"} {
		for _, blank := range []string{"", " \t "} {
			t.Run(field+"/"+blank, func(t *testing.T) {
				credentials := &SlackConfig{AppToken: "app-token-placeholder", BotToken: "bot-token-placeholder", AppID: "app-example", TeamID: "team-example"}
				switch field {
				case "app_token":
					credentials.AppToken = blank
				case "bot_token":
					credentials.BotToken = blank
				case "app_id":
					credentials.AppID = blank
				case "team_id":
					credentials.TeamID = blank
				}
				cfg := &Config{IMs: []IMAdapterConfig{{Slack: credentials}}}
				err := cfg.Validate()
				if err == nil || !strings.Contains(err.Error(), "ims.slack requires") {
					t.Fatalf("missing %s: %v", field, err)
				}
				if strings.Contains(err.Error(), "placeholder") {
					t.Fatal("validation exposed credentials")
				}
			})
		}
	}
}

func TestSlackConfigurationExplicitBindingAndOwner(t *testing.T) {
	input := []byte(`ims:
  slack:
    app_token: app-token-placeholder
    bot_token: bot-token-placeholder
    app_id: app-example
    team_id: team-example
    owner: {account_id: account-example, user_id: owner-example}
    chat_bindings: {channel-example: project}
workspaces: {project: {path: /project}}
`)
	cfg, doc, _, err := ParseDocument(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if issues := UnsupportedConfigFields(doc); len(issues) != 0 {
		t.Fatal(issues)
	}
	if ws, err := cfg.ResolveWorkspace("slack", "channel-example"); err != nil || ws.ID != "project" {
		t.Fatalf("workspace=%v error=%v", ws, err)
	}
	if _, err := cfg.ResolveWorkspace("slack", "unbound-thread"); err == nil {
		t.Fatal("unbound target inherited the single workspace")
	}
}
