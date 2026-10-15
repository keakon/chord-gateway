package main

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/config"
)

// proxyFuncFor resolves per-IM > global priority. Empty (both unset) means
// disabled: it returns nil so callers use a direct connection and must not
// fall back to environment proxy variables. The returned closure shares the
// parsed config URL read-only; transports must never mutate it.
func proxyFuncFor(cfg *config.Config, im config.IMAdapterConfig) (func(*http.Request) (*url.URL, error), error) {
	var raw string
	if cfg != nil {
		raw = cfg.EffectiveProxyURL(im)
	} else {
		raw = im.ProxyURLValue()
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parsed, err := config.ParseProxyURL(raw)
	if err != nil {
		return nil, err
	}
	return func(*http.Request) (*url.URL, error) { return parsed, nil }, nil
}

// websocketProxyFuncFor is proxyFuncFor plus the WebSocket scheme check. Use
// it in adapters that open a long-connection so an unsupported scheme fails at
// startup instead of only on the first dial.
func websocketProxyFuncFor(cfg *config.Config, im config.IMAdapterConfig) (func(*http.Request) (*url.URL, error), error) {
	proxyFunc, err := proxyFuncFor(cfg, im)
	if err != nil {
		return nil, err
	}
	raw := im.ProxyURLValue()
	if cfg != nil {
		raw = cfg.EffectiveProxyURL(im)
	}
	if err := config.ValidateProxyForWebsocket(raw); err != nil {
		return nil, err
	}
	return proxyFunc, nil
}

// proxiedTransport clones the default transport and pins its proxy behavior:
// explicit config proxy when set, otherwise a direct connection. Environment
// proxy variables are never consulted so unset means disabled.
func proxiedTransport(proxyFunc func(*http.Request) (*url.URL, error)) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxyFunc == nil {
		proxyFunc = func(*http.Request) (*url.URL, error) { return nil, nil }
	}
	transport.Proxy = proxyFunc
	return transport
}

// logEffectiveProxies reports each configured outbound proxy once at startup,
// with credentials stripped, so operators can confirm the value the gateway
// actually resolved without grepping the config.
func logEffectiveProxies(cfg *config.Config) {
	if cfg.ProxyURL != "" {
		log.Infof("outbound proxy global=%s", redactProxyURL(cfg.ProxyURL))
	}
	for _, im := range cfg.ActiveIMs() {
		if override := im.ProxyURLValue(); override != "" {
			log.Infof("outbound proxy ims.%s=%s", im.Type(), redactProxyURL(override))
		}
	}
}

// redactProxyURL drops any userinfo so proxy credentials never reach the logs.
func redactProxyURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "(configured)"
	}
	u.User = nil
	return u.String()
}

// websocketProxyFunc adapts an HTTP proxy func for gorilla/websocket dialers.
// The vendored dialer only understands "socks5", not "socks5h"; Go's HTTP
// transport treats both identically (remote DNS), so rewrite the scheme here.
// Nil stays an explicit direct connection so environment proxy variables are
// never consulted. The returned URL is a copy; the shared config URL is kept.
func websocketProxyFunc(proxyFunc func(*http.Request) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	if proxyFunc == nil {
		return func(*http.Request) (*url.URL, error) { return nil, nil }
	}
	return func(request *http.Request) (*url.URL, error) {
		u, err := proxyFunc(request)
		if err != nil || u == nil {
			return u, err
		}
		if strings.EqualFold(u.Scheme, "socks5h") {
			converted := *u
			converted.Scheme = "socks5"
			return &converted, nil
		}
		return u, nil
	}
}
