// Package main is the chord gateway — a lightweight control plane
// that connects IM platforms (WeChat, Feishu, Slack, etc.) to chord headless
// processes via stdio JSON protocol.
package main

import (
	"fmt"

	"github.com/keakon/chord-gateway/config"
)

// NewIMAdapter creates the appropriate IM adapter based on config.
func NewIMAdapter(cfg *config.Config, paths *config.Paths, router *NotificationRouter) (IMAdapter, error) {
	activeIMs := cfg.ActiveIMs()
	if len(activeIMs) != 1 {
		return nil, fmt.Errorf("expected exactly one IM adapter, got %d", len(activeIMs))
	}
	return newAdapterFromConfig(activeIMs[0], cfg, paths, router)
}

// newAdapterFromConfig creates an adapter from an IMAdapterConfig.
func newAdapterFromConfig(imCfg config.IMAdapterConfig, cfg *config.Config, paths *config.Paths, router *NotificationRouter) (IMAdapter, error) {
	switch imCfg.Type() {
	case "wechat":
		return NewWechatAdapter(cfg, imCfg, paths, router)
	case "feishu":
		return NewFeishuAdapter(cfg, imCfg, paths, router)
	case "wecom":
		return NewWecomAdapter(cfg, imCfg, paths, router)
	case "dingtalk":
		return NewDingtalkAdapter(cfg, imCfg, paths, router)
	case "discord":
		return NewDiscordAdapter(cfg, imCfg, paths, router)
	case "slack":
		return NewSlackAdapter(cfg, imCfg, paths, router)
	case "telegram":
		return NewTelegramAdapter(cfg, imCfg, paths, router)
	default:
		return nil, fmt.Errorf("unsupported IM type: %s", imCfg.Type())
	}
}
