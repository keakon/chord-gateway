// Package main is the chord gateway — a lightweight control plane
// that connects IM platforms (WeChat, Feishu, etc.) to chord headless
// processes via stdio JSON protocol.
package main

import (
	"fmt"

	"github.com/keakon/chord-gateway/config"
)

// NewIMAdapter creates the appropriate IM adapter based on config.
func NewIMAdapter(cfg *config.Config, paths *config.Paths, router *NotificationRouter) (IMAdapter, error) {
	activeIMs := cfg.ActiveIMs()
	if len(activeIMs) == 1 {
		return newAdapterFromConfig(activeIMs[0], paths, router)
	}
	switch cfg.IM.Type {
	case "wechat":
		return NewWechatAdapter(cfg, paths, router)
	case "feishu":
		return NewFeishuAdapter(cfg, paths, router)
	default:
		return nil, fmt.Errorf("unsupported IM type: %s", cfg.IM.Type)
	}
}

// newAdapterFromConfig creates an adapter from an IMAdapterConfig.
func newAdapterFromConfig(imCfg config.IMAdapterConfig, paths *config.Paths, router *NotificationRouter) (IMAdapter, error) {
	// Build a single-IM Config for the adapter constructors.
	cfg := &config.Config{
		IM: config.IMConfig(imCfg),
	}

	switch imCfg.Type {
	case "wechat":
		return NewWechatAdapter(cfg, paths, router)
	case "feishu":
		return NewFeishuAdapter(cfg, paths, router)
	default:
		return nil, fmt.Errorf("unsupported IM type: %s", imCfg.Type)
	}
}
