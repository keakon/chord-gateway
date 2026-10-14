package main

import (
	"testing"

	"github.com/keakon/chord-gateway/config"
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
		if _, ok := adapter.(*WecomAdapter); !ok || adapter.Type() != "wecom" || adapter.SupportsLoginRenewal() {
			t.Fatalf("unexpected wecom adapter: %T", adapter)
		}
		if _, err = adapter.StartLogin(); err != ErrLoginNotSupported {
			t.Fatalf("StartLogin() error = %v", err)
		}
	})

	t.Run("dingtalk", func(t *testing.T) {
		cfg := &config.Config{IMs: []config.IMAdapterConfig{{Dingtalk: dingtalkIdentityTestConfig()}}}
		adapter, err := NewIMAdapter(cfg, paths, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer adapter.Disconnect()
		if _, ok := adapter.(*DingtalkAdapter); !ok || adapter.Type() != "dingtalk" {
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
	wa, ok := adapter.(*WechatAdapter)
	if !ok {
		t.Fatalf("adapter type = %T", adapter)
	}
	if got := wa.baseURL(); got != "https://example.com" {
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
