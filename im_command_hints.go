package main

import "strings"

type PlatformPresentation struct{ Platform, BotMention, BotUsername string }
type CommandTarget struct {
	ChatID, ChatKind string
	Thread           bool
}

// FormatCommandHint formats generated instructions only, never model output.
func FormatCommandHint(p PlatformPresentation, target CommandTarget, command string) string {
	if !strings.HasPrefix(command, "/") {
		return command
	}
	switch p.Platform {
	case "slack":
		if !target.Thread {
			return "/chord " + strings.TrimPrefix(command, "/")
		}
		name := strings.TrimSpace(p.BotUsername)
		if name == "" {
			name = "Bot"
		}
		return "@" + name + " " + command + " (select the bot from Slack's @ menu)"
	case "telegram":
		name, args, hasArgs := strings.Cut(command, " ")
		name = telegramCommandAlias(name, true)
		if target.ChatKind == "group" && p.BotUsername != "" {
			name += "@" + strings.TrimPrefix(p.BotUsername, "@")
		}
		if hasArgs {
			name += " " + args
		}
		return name
	case "discord", "wecom", "dingtalk":
		if target.ChatKind != "group" && !target.Thread {
			return command
		}
	default:
		return command
	}
	if p.BotMention != "" {
		return p.BotMention + " " + command
	}
	return command
}

func commandHint(command string, hints []func(string) string) string {
	if len(hints) > 0 && hints[0] != nil {
		return hints[0](command)
	}
	return command
}

func (r *NotificationRouter) commandHint(platform string, target CommandTarget, command string) string {
	p := PlatformPresentation{Platform: platform}
	if r.admission != nil {
		pairing := r.admission.pairing
		pairing.mu.Lock()
		if state := pairing.platforms[platform]; state != nil {
			p = state.presentation
		}
		pairing.mu.Unlock()
	}
	return FormatCommandHint(p, target, command)
}

func commandTargetForChat(platform, chat string) CommandTarget {
	target := CommandTarget{ChatID: chat, ChatKind: "group"}
	if platform == "telegram" && !strings.HasPrefix(chat, "-") {
		target.ChatKind = "private"
	}
	if parts, err := DecodeScopedID(chat, "target-v1:", 3); err == nil {
		if parts[0] == "slack-thread" {
			target.Thread = true
		}
		if platform == "dingtalk" && parts[0] == "dingtalk-dm" {
			target.ChatKind = "private"
		}
	}
	return target
}
