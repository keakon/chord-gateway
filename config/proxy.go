package config

import (
	"fmt"
	"net/url"
	"strings"
)

// Proxy scheme support differs by transport. HTTP clients and WebSocket
// long-connections both accept http and socks5/socks5h. HTTPS proxies are
// only usable by the HTTP clients: the vendored gorilla/websocket dialer has
// no TLS-to-proxy support, so the gateway rejects https for WebSocket
// transports at startup and documents the limitation. gorilla/websocket also
// only understands socks5, not socks5h, so socks5h is normalized to socks5
// (identical remote-DNS behavior in Go) for WebSocket dialers.
var allowedProxySchemes = map[string]bool{
	"http":    true,
	"https":   true,
	"socks5":  true,
	"socks5h": true,
}

// websocketProxyPlatforms lists adapters that open a WebSocket long-connection.
// They share the gorilla/websocket dialer, which has no HTTPS-proxy support.
var websocketProxyPlatforms = map[string]bool{
	"discord":  true,
	"slack":    true,
	"feishu":   true,
	"wecom":    true,
	"dingtalk": true,
}

// ValidateProxyForWebsocket rejects a proxy scheme that the WebSocket dialer
// cannot use. Call it at startup for adapters that open WebSocket
// long-connections so a config-time value never fails only on the first dial.
func ValidateProxyForWebsocket(raw string) error {
	u, err := ParseProxyURL(raw)
	if err != nil {
		return fmt.Errorf("proxy_url: %w", err)
	}
	if u == nil {
		return nil
	}
	if u.Scheme == "https" {
		return fmt.Errorf("proxy_url scheme %q is not supported for WebSocket long-connections; use http, socks5 or socks5h", u.Scheme)
	}
	return nil
}

// ParseProxyURL validates a proxy URL. Empty (after trim) means disabled and
// returns nil without error. The scheme must be explicit.
func ParseProxyURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	u, err := url.Parse(trimmed)
	if err != nil || u == nil {
		return nil, fmt.Errorf("invalid URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if !allowedProxySchemes[scheme] {
		return nil, fmt.Errorf("scheme %q is not supported: must be http, https, socks5 or socks5h", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("missing host")
	}
	u.Scheme = scheme
	return u, nil
}

// ValidateProxyURL returns an error when raw is non-empty but invalid.
func ValidateProxyURL(field, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	if _, err := ParseProxyURL(raw); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}

// ProxyURLValue returns the per-adapter proxy URL, if any.
func (im *IMAdapterConfig) ProxyURLValue() string {
	if im == nil {
		return ""
	}
	switch {
	case im.Telegram != nil:
		return im.Telegram.ProxyURL
	case im.Discord != nil:
		return im.Discord.ProxyURL
	case im.Wecom != nil:
		return im.Wecom.ProxyURL
	case im.Dingtalk != nil:
		return im.Dingtalk.ProxyURL
	case im.Slack != nil:
		return im.Slack.ProxyURL
	case im.Wechat != nil:
		return im.Wechat.ProxyURL
	case im.Feishu != nil:
		return im.Feishu.ProxyURL
	default:
		return ""
	}
}

// EffectiveProxyURL resolves per-IM > global priority. Empty means disabled:
// no proxy is used even when environment proxy variables are set.
func (c *Config) EffectiveProxyURL(im IMAdapterConfig) string {
	if v := strings.TrimSpace(im.ProxyURLValue()); v != "" {
		return v
	}
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.ProxyURL)
}
