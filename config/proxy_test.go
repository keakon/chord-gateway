package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseProxyURL_AcceptsSupportedSchemes(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:1087",
		"https://proxy.example:8080",
		"socks5://127.0.0.1:1080",
		"socks5h://127.0.0.1:1080",
		"http://user:pass@127.0.0.1:1087",
	} {
		if _, err := ParseProxyURL(raw); err != nil {
			t.Fatalf("ParseProxyURL(%q) error = %v", raw, err)
		}
	}
}

func TestParseProxyURL_RejectsInvalid(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1:1087",
		"ftp://127.0.0.1:21",
		"http://",
		"http:///no-host",
	} {
		if _, err := ParseProxyURL(raw); err == nil {
			t.Fatalf("ParseProxyURL(%q) expected error", raw)
		}
	}
	if got, err := ParseProxyURL(""); err != nil || got != nil {
		t.Fatalf("empty proxy should be disabled, got %v, %v", got, err)
	}
}

func TestEffectiveProxyURL_Priority(t *testing.T) {
	cfg := &Config{ProxyURL: "http://127.0.0.1:1087"}
	perIM := IMAdapterConfig{Telegram: &TelegramConfig{ProxyURL: "socks5h://127.0.0.1:1080"}}
	if got := cfg.EffectiveProxyURL(perIM); got != "socks5h://127.0.0.1:1080" {
		t.Fatalf("per-IM should win, got %q", got)
	}
	globalOnly := IMAdapterConfig{Telegram: &TelegramConfig{}}
	if got := cfg.EffectiveProxyURL(globalOnly); got != "http://127.0.0.1:1087" {
		t.Fatalf("global fallback failed, got %q", got)
	}
	empty := &Config{}
	if got := empty.EffectiveProxyURL(globalOnly); got != "" {
		t.Fatalf("unset should be disabled, got %q", got)
	}
}

func TestValidate_RejectsInvalidProxy(t *testing.T) {
	cfg := &Config{
		ProxyURL: "ftp://127.0.0.1:21",
		IMs:      []IMAdapterConfig{{Telegram: &TelegramConfig{BotToken: "x"}}},
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1"},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected global proxy error")
	}
	cfg2 := &Config{
		IMs: []IMAdapterConfig{{Telegram: &TelegramConfig{BotToken: "x", ProxyURL: "no-scheme"}}},
		Workspaces: []Workspace{
			{ID: "ws1", Path: "/tmp/ws1"},
		},
	}
	if err := cfg2.Validate(); err == nil {
		t.Fatal("expected per-IM proxy error")
	}
}

func TestParseProxyURL_ErrorOmitsCredentials(t *testing.T) {
	for _, raw := range []string{
		"http://sample-user:sample-secret@proxy.invalid:bad",
		"ftp://sample-user:sample-secret@proxy.invalid:21",
		"http://sample-user:sample-secret@/no-host",
	} {
		_, err := ParseProxyURL(raw)
		if err == nil {
			t.Fatalf("ParseProxyURL(%q) expected error", raw)
		}
		if strings.Contains(err.Error(), "sample-secret") || strings.Contains(err.Error(), "sample-user") {
			t.Fatalf("ParseProxyURL(%q) error leaks credentials: %v", raw, err)
		}
	}
}

func TestWebsocketProxyPlatformsMatchAdapterConfigs(t *testing.T) {
	// Platforms that only ever use a plain HTTP client, never a WebSocket
	// long-connection. Every other adapter must be listed as websocket-backed
	// so the https-proxy rejection in Validate applies to it.
	httpOnly := map[string]bool{"telegram": true, "wechat": true}
	typ := reflect.TypeOf(IMAdapterConfig{})
	fields := make(map[string]bool, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		fields[strings.ToLower(typ.Field(i).Name)] = true
	}
	for platform := range websocketProxyPlatforms {
		if !fields[platform] {
			t.Errorf("websocketProxyPlatforms lists %q with no IMAdapterConfig field", platform)
		}
		if httpOnly[platform] {
			t.Errorf("websocketProxyPlatforms lists HTTP-only platform %q", platform)
		}
	}
	for platform := range fields {
		if !websocketProxyPlatforms[platform] && !httpOnly[platform] {
			t.Errorf("IMAdapterConfig field %q is unclassified; add it to websocketProxyPlatforms or the httpOnly set", platform)
		}
	}
}

func TestValidateProxyForWebsocket(t *testing.T) {
	for _, raw := range []string{"", "http://127.0.0.1:1087", "socks5://127.0.0.1:1080", "socks5h://127.0.0.1:1080"} {
		if err := ValidateProxyForWebsocket(raw); err != nil {
			t.Fatalf("ValidateProxyForWebsocket(%q) error = %v", raw, err)
		}
	}
	if err := ValidateProxyForWebsocket("https://proxy.example:8443"); err == nil {
		t.Fatal("https must be rejected for WebSocket long-connections")
	}
}

func TestValidate_RejectsHTTPSProxyForWebsocketPlatform(t *testing.T) {
	base := func() *Config {
		return &Config{
			IMs:        []IMAdapterConfig{{Telegram: &TelegramConfig{BotToken: "x"}}},
			Workspaces: []Workspace{{ID: "ws1", Path: "/tmp/ws1"}},
		}
	}
	// Telegram uses a plain HTTP client, so an https proxy is still valid.
	telegramOnly := base()
	telegramOnly.ProxyURL = "https://proxy.example:8443"
	if err := telegramOnly.Validate(); err != nil {
		t.Fatalf("https proxy for an HTTP-only platform should validate: %v", err)
	}
	// Adding a WebSocket platform makes the same global proxy unusable.
	withDiscord := base()
	withDiscord.ProxyURL = "https://proxy.example:8443"
	withDiscord.IMs = append(withDiscord.IMs, IMAdapterConfig{Discord: &DiscordConfig{BotToken: "x", ApplicationID: "1"}})
	if err := withDiscord.Validate(); err == nil {
		t.Fatal("global https proxy must be rejected once a WebSocket platform is active")
	}
	// A per-IM https override is rejected even without a global proxy.
	perIM := base()
	perIM.IMs = append(perIM.IMs, IMAdapterConfig{Discord: &DiscordConfig{BotToken: "x", ApplicationID: "1", ProxyURL: "https://proxy.example:8443"}})
	if err := perIM.Validate(); err == nil {
		t.Fatal("per-IM https proxy must be rejected for a WebSocket platform")
	}
}
