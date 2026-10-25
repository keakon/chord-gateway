package main

import "github.com/keakon/chord-gateway/internal/im"

// TextSendMetrics forwards shared sender diagnostics for multi-adapter metrics.
func (a *DiscordAdapter) TextSendMetrics() im.IMTextMetricsSnapshot {
	return a.sender.TextSendMetrics()
}

func (a *DiscordAdapter) TextSendTargetSnapshot(target string) im.IMTextTargetSnapshot {
	return a.sender.TextSendTargetSnapshot(target)
}
