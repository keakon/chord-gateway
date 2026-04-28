package main

import (
	"testing"

	"github.com/keakon/chord-gateway/config"
)

func TestNewIMAdapter_UnsupportedType(t *testing.T) {
	cfg := &config.Config{IM: config.IMConfig{Type: "unsupported"}}
	adapter, err := NewIMAdapter(cfg, testPaths(t), nil)
	if err == nil || err.Error() != "unsupported IM type: unsupported" {
		t.Fatalf("NewIMAdapter() adapter=%#v err=%v", adapter, err)
	}
}

func TestNewIMAdapter_CreatesSingleAdapters(t *testing.T) {
	paths := testPaths(t)

	t.Run("wechat", func(t *testing.T) {
		cfg := &config.Config{IM: config.IMConfig{Type: "wechat", Wechat: &config.WechatConfig{}}}
		adapter, err := NewIMAdapter(cfg, paths, nil)
		if err != nil {
			t.Fatalf("NewIMAdapter() error = %v", err)
		}
		if got := adapter.Type(); got != "wechat" {
			t.Fatalf("adapter.Type() = %q", got)
		}
	})

	t.Run("feishu", func(t *testing.T) {
		cfg := &config.Config{IM: config.IMConfig{Type: "feishu", Feishu: &config.FeishuConfig{AppID: "app", AppSecret: "secret"}}}
		adapter, err := NewIMAdapter(cfg, paths, nil)
		if err != nil {
			t.Fatalf("NewIMAdapter() error = %v", err)
		}
		defer adapter.Disconnect()
		if got := adapter.Type(); got != "feishu" {
			t.Fatalf("adapter.Type() = %q", got)
		}
	})
}

func TestNewIMAdapter_WithSingleActiveIMUsesAdapterConfig(t *testing.T) {
	paths := testPaths(t)
	cfg := &config.Config{IMs: []config.IMAdapterConfig{{Type: "wechat", Wechat: &config.WechatConfig{BaseURL: "https://example.com"}}}}
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
	adapter, err := newAdapterFromConfig(config.IMAdapterConfig{Type: "bad"}, testPaths(t), nil)
	if err == nil || err.Error() != "unsupported IM type: bad" {
		t.Fatalf("newAdapterFromConfig() adapter=%#v err=%v", adapter, err)
	}
}
