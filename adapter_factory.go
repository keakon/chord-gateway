// Package main is the chord gateway — a lightweight control plane
// that connects IM platforms (WeChat, Feishu, Slack, etc.) to chord headless
// processes via stdio JSON protocol.
package main

import (
	"fmt"

	dingtalk "github.com/keakon/chord-gateway/internal/platform/dingtalk"
	discord "github.com/keakon/chord-gateway/internal/platform/discord"
	feishu "github.com/keakon/chord-gateway/internal/platform/feishu"
	slack "github.com/keakon/chord-gateway/internal/platform/slack"
	telegram "github.com/keakon/chord-gateway/internal/platform/telegram"
	wechat "github.com/keakon/chord-gateway/internal/platform/wechat"
	wecom "github.com/keakon/chord-gateway/internal/platform/wecom"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

// NewIMAdapter creates the appropriate IM adapter based on config.
func NewIMAdapter(cfg *config.Config, paths *config.Paths, router *NotificationRouter) (im.IMAdapter, error) {
	activeIMs := cfg.ActiveIMs()
	if len(activeIMs) != 1 {
		return nil, fmt.Errorf("expected exactly one IM adapter, got %d", len(activeIMs))
	}
	return newAdapterFromConfig(activeIMs[0], cfg, paths, router)
}

// newAdapterFromConfig creates an adapter from an IMAdapterConfig.
func newAdapterFromConfig(imCfg config.IMAdapterConfig, cfg *config.Config, paths *config.Paths, router *NotificationRouter) (im.IMAdapter, error) {
	switch imCfg.Type() {
	case "wechat":
		return wechat.NewWechatAdapter(cfg, imCfg, paths, router)
	case "feishu":
		return feishu.NewFeishuAdapter(cfg, imCfg, paths, router)
	case "wecom":
		return wecom.NewWecomAdapter(cfg, imCfg, paths, router)
	case "dingtalk":
		return dingtalk.NewDingtalkAdapter(cfg, imCfg, paths, router)
	case "discord":
		return discord.NewDiscordAdapter(cfg, imCfg, paths, router)
	case "slack":
		return slack.NewSlackAdapter(cfg, imCfg, paths, router)
	case "telegram":
		return telegram.NewTelegramAdapter(cfg, imCfg, paths, router)
	default:
		return nil, fmt.Errorf("unsupported IM type: %s", imCfg.Type())
	}
}
