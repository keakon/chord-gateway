package im

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
		name = TelegramCommandAlias(name, true)
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

func CommandTargetForChat(platform, chat string) CommandTarget {
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

// TelegramCommandAlias maps between Telegram's hyphen-free command shape and
// the canonical hyphenated commands. Explicit aliases avoid rewriting arbitrary
// paths, model commands or args.
func TelegramCommandAlias(command string, outbound bool) string {
	for _, canonical := range []string{"/handoff-deny", "/question-interact", "/question-withdraw", "/question-replace"} {
		alias := strings.ReplaceAll(canonical, "-", "_")
		if outbound && strings.EqualFold(command, canonical) {
			return alias
		}
		if !outbound && strings.EqualFold(command, alias) {
			return canonical
		}
	}
	return command
}
