package main

import (
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/keakon/chord-gateway/config"
)

func TestProxyFuncFor_PriorityAndDisabled(t *testing.T) {
	cfg := &config.Config{ProxyURL: "http://127.0.0.1:1087"}
	im := config.IMAdapterConfig{Telegram: &config.TelegramConfig{ProxyURL: "socks5h://127.0.0.1:1080"}}
	fn, err := proxyFuncFor(cfg, im)
	if err != nil {
		t.Fatal(err)
	}
	if fn == nil {
		t.Fatal("expected per-IM proxy func")
	}
	u, _ := fn(&http.Request{})
	if u.Scheme != "socks5h" {
		t.Fatalf("per-IM should win, got %v", u)
	}

	globalOnly := config.IMAdapterConfig{Telegram: &config.TelegramConfig{}}
	fn, err = proxyFuncFor(cfg, globalOnly)
	if err != nil || fn == nil {
		t.Fatalf("global fallback failed: %v", err)
	}
	u, _ = fn(&http.Request{})
	if u.Scheme != "http" {
		t.Fatalf("global fallback wrong: %v", u)
	}

	fn, err = proxyFuncFor(&config.Config{}, globalOnly)
	if err != nil || fn != nil {
		t.Fatalf("unset should be disabled, got %v", err)
	}
}

func TestProxyFuncFor_Invalid(t *testing.T) {
	im := config.IMAdapterConfig{Telegram: &config.TelegramConfig{ProxyURL: "ftp://x"}}
	if _, err := proxyFuncFor(&config.Config{}, im); err == nil {
		t.Fatal("expected error")
	}
}

func TestProxiedTransport_DisabledIsDirect(t *testing.T) {
	tr := proxiedTransport(nil)
	u, err := tr.Proxy(&http.Request{})
	if err != nil || u != nil {
		t.Fatalf("disabled proxy must be direct, got %v %v", u, err)
	}
}

func TestWebsocketProxyFunc_NormalizesSocks5H(t *testing.T) {
	// Nil stays an explicit direct connection without consulting the environment.
	direct := websocketProxyFunc(nil)
	if u, err := direct(&http.Request{}); err != nil || u != nil {
		t.Fatalf("nil proxy must stay direct, got %v %v", u, err)
	}

	proxyURL, err := url.Parse("socks5h://user:pass@127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := websocketProxyFunc(func(*http.Request) (*url.URL, error) { return proxyURL, nil })
	got, err := wrapped(&http.Request{})
	if err != nil || got == nil || got.Scheme != "socks5" {
		t.Fatalf("socks5h must downgrade to socks5, got %v %v", got, err)
	}
	if got.Host != "127.0.0.1:1080" || got.User.String() != "user:pass" {
		t.Fatalf("proxy authority must survive downgrade, got %v", got)
	}
	if proxyURL.Scheme != "socks5h" {
		t.Fatalf("shared config URL must not be mutated, got %v", proxyURL)
	}

	socks5URL, err := url.Parse("socks5://127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	unchanged := websocketProxyFunc(func(*http.Request) (*url.URL, error) { return socks5URL, nil })
	if got, err := unchanged(&http.Request{}); err != nil || got != socks5URL {
		t.Fatalf("socks5 must pass through untouched, got %v %v", got, err)
	}

	httpURL, err := url.Parse("http://127.0.0.1:1087")
	if err != nil {
		t.Fatal(err)
	}
	plain := websocketProxyFunc(func(*http.Request) (*url.URL, error) { return httpURL, nil })
	if got, err := plain(&http.Request{}); err != nil || got != httpURL {
		t.Fatalf("http proxy must pass through untouched, got %v %v", got, err)
	}

	want := errors.New("sample proxy failure")
	failing := websocketProxyFunc(func(*http.Request) (*url.URL, error) { return nil, want })
	if _, err := failing(&http.Request{}); !errors.Is(err, want) {
		t.Fatalf("proxy errors must propagate, got %v", err)
	}
}

func TestDiscordProxyDialer_NormalizesSocks5H(t *testing.T) {
	proxyURL, err := url.Parse("socks5h://127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	dialer := discordProxyDialer(func(*http.Request) (*url.URL, error) { return proxyURL, nil })
	got, err := dialer.Proxy(&http.Request{})
	if err != nil || got == nil || got.Scheme != "socks5" {
		t.Fatalf("discord dialer must downgrade socks5h, got %v %v", got, err)
	}
	direct := discordProxyDialer(nil)
	if u, err := direct.Proxy(&http.Request{}); err != nil || u != nil {
		t.Fatalf("discord nil proxy must stay direct, got %v %v", u, err)
	}
}
