package main

// TextSendMetrics forwards shared sender diagnostics for multi-adapter metrics.
func (a *DiscordAdapter) TextSendMetrics() IMTextMetricsSnapshot {
	return a.sender.TextSendMetrics()
}

func (a *DiscordAdapter) TextSendTargetSnapshot(target string) IMTextTargetSnapshot {
	return a.sender.TextSendTargetSnapshot(target)
}
