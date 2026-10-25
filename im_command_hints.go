package main

import (
	"github.com/keakon/chord-gateway/internal/im"
)

func commandHint(command string, hints []func(string) string) string {
	if len(hints) > 0 && hints[0] != nil {
		return hints[0](command)
	}
	return command
}

func (r *NotificationRouter) commandHint(platform string, target im.CommandTarget, command string) string {
	p := im.PlatformPresentation{Platform: platform}
	if r.admission != nil {
		pairing := r.admission.pairing
		pairing.mu.Lock()
		if state := pairing.platforms[platform]; state != nil {
			p = state.presentation
		}
		pairing.mu.Unlock()
	}
	return im.FormatCommandHint(p, target, command)
}
