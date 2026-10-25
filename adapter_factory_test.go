package main

import (
	"testing"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
	dingtalk "github.com/keakon/chord-gateway/internal/platform/dingtalk"
	slack "github.com/keakon/chord-gateway/internal/platform/slack"
	telegram "github.com/keakon/chord-gateway/internal/platform/telegram"
	wechat "github.com/keakon/chord-gateway/internal/platform/wechat"
	wecom "github.com/keakon/chord-gateway/internal/platform/wecom"
)

func TestNewIMAdapter_UnsupportedType(t *testing.T) {
	cfg := &config.Config{IMs: []config.IMAdapterConfig{{}}}
	adapter, err := NewIMAdapter(cfg, testPaths(t), nil)
	if err == nil || err.Error() != "unsupported IM type: " {
		t.Fatalf("NewIMAdapter() adapter=%#v err=%v", adapter, err)
	}
}

func TestNewIMAdapter_CreatesSingleAdapters(t *testing.T) {
	paths := testPaths(t)

	t.Run("wechat", func(t *testing.T) {
		cfg := &config.Config{IMs: []config.IMAdapterConfig{{Wechat: &config.WechatConfig{}}}}
		adapter, err := NewIMAdapter(cfg, paths, nil)
		if err != nil {
			t.Fatalf("NewIMAdapter() error = %v", err)
		}
		if got := adapter.Type(); got != "wechat" {
			t.Fatalf("adapter.Type() = %q", got)
		}
	})

	t.Run("telegram", func(t *testing.T) {
		cfg := &config.Config{IMs: []config.IMAdapterConfig{{Telegram: &config.TelegramConfig{BotToken: "123:test-token"}}}}
		adapter, err := NewIMAdapter(cfg, paths, nil)
		if err != nil {
			t.Fatalf("NewIMAdapter() error = %v", err)
		}
		defer adapter.Disconnect()
		if _, ok := adapter.(*telegram.TelegramAdapter); !ok || adapter.Type() != "telegram" {
			t.Fatalf("adapter = %T, type = %q", adapter, adapter.Type())
		}
	})

	t.Run("feishu", func(t *testing.T) {
		cfg := &config.Config{IMs: []config.IMAdapterConfig{{Feishu: &config.FeishuConfig{AppID: "app", AppSecret: "secret"}}}}
		adapter, err := NewIMAdapter(cfg, paths, nil)
		if err != nil {
			t.Fatalf("NewIMAdapter() error = %v", err)
		}
		defer adapter.Disconnect()
		if got := adapter.Type(); got != "feishu" {
			t.Fatalf("adapter.Type() = %q", got)
		}
	})

	t.Run("wecom", func(t *testing.T) {
		cfg := &config.Config{IMs: []config.IMAdapterConfig{{Wecom: &config.WecomConfig{BotID: "example-bot", Secret: "example-secret"}}}}
		adapter, err := NewIMAdapter(cfg, paths, nil)
		if err != nil {
			t.Fatalf("NewIMAdapter() error = %v", err)
		}
		defer adapter.Disconnect()
		if _, ok := adapter.(*wecom.WecomAdapter); !ok || adapter.Type() != "wecom" || adapter.SupportsLoginRenewal() {
			t.Fatalf("unexpected wecom adapter: %T", adapter)
		}
		if _, err = adapter.StartLogin(); err != im.ErrLoginNotSupported {
			t.Fatalf("StartLogin() error = %v", err)
		}
	})

	t.Run("dingtalk", func(t *testing.T) {
		cfg := &config.Config{IMs: []config.IMAdapterConfig{{Dingtalk: dingtalk.IdentityTestConfigForTest()}}}
		adapter, err := NewIMAdapter(cfg, paths, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer adapter.Disconnect()
		if _, ok := adapter.(*dingtalk.DingtalkAdapter); !ok || adapter.Type() != "dingtalk" {
			t.Fatalf("adapter type = %T", adapter)
		}
	})

	t.Run("discord", func(t *testing.T) {
		cfg := &config.Config{IMs: []config.IMAdapterConfig{{Discord: &config.DiscordConfig{BotToken: "example-token", ApplicationID: "123456789012345678"}}}}
		adapter, err := NewIMAdapter(cfg, paths, nil)
		if err != nil {
			t.Fatalf("NewIMAdapter() error = %v", err)
		}
		defer adapter.Disconnect()
		if got := adapter.Type(); got != "discord" {
			t.Fatalf("adapter.Type() = %q", got)
		}
	})
}

func TestNewIMAdapter_WithSingleActiveIMUsesAdapterConfig(t *testing.T) {
	paths := testPaths(t)
	cfg := &config.Config{IMs: []config.IMAdapterConfig{{Wechat: &config.WechatConfig{BaseURL: "https://example.com"}}}}
	adapter, err := NewIMAdapter(cfg, paths, nil)
	if err != nil {
		t.Fatalf("NewIMAdapter() error = %v", err)
	}
	wa, ok := adapter.(*wechat.WechatAdapter)
	if !ok {
		t.Fatalf("adapter type = %T", adapter)
	}
	if got := wa.BaseURLForTest(); got != "https://example.com" {
		t.Fatalf("baseURL = %q", got)
	}
}

func TestNewAdapterFromConfig_UnsupportedType(t *testing.T) {
	cfg := &config.Config{IMs: []config.IMAdapterConfig{{}}}
	adapter, err := newAdapterFromConfig(config.IMAdapterConfig{}, cfg, testPaths(t), nil)
	if err == nil || err.Error() != "unsupported IM type: " {
		t.Fatalf("newAdapterFromConfig() adapter=%#v err=%v", adapter, err)
	}
}

func TestNewIMAdapter_CreatesSlackWithoutConnecting(t *testing.T) {
	cfg := &config.Config{IMs: []config.IMAdapterConfig{{Slack: &config.SlackConfig{
		AppToken: "app-token-placeholder", BotToken: "bot-token-placeholder", AppID: "app-example", TeamID: "team-example",
	}}}}
	adapter, err := NewIMAdapter(cfg, testPaths(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := adapter.(*slack.SlackAdapter); !ok || adapter.Type() != "slack" {
		t.Fatalf("adapter = %T", adapter)
	}
}
