// Package main: Feishu wire-format helpers shared between handlers.
package main

import (
	"encoding/json"
	"strings"
)

// parseFeishuMessageText extracts visible text from a Feishu message payload.
// Supports "text" and "post" message types; other types collapse to "".
func parseFeishuMessageText(messageType, contentRaw string) (string, error) {
	var content FeishuMessageContent
	if err := json.Unmarshal([]byte(contentRaw), &content); err != nil {
		return "", err
	}
	switch messageType {
	case "text":
		return content.Text, nil
	case "post":
		var lines []string
		for _, line := range content.Content {
			var b strings.Builder
			for _, item := range line {
				switch item.Tag {
				case "text", "a":
					b.WriteString(item.Text)
				case "at":
					if item.UserName != "" {
						b.WriteString("@")
						b.WriteString(item.UserName)
					}
				}
			}
			lines = append(lines, b.String())
		}
		return strings.TrimSpace(strings.Join(lines, "\n")), nil
	default:
		return "", nil
	}
}

// isValidFeishuCardAction reports whether a card action callback carries a
// known (action_type, action, value) triple. Unknown combinations are rejected
// before being enqueued.
func isValidFeishuCardAction(actionType, action, value string) bool {
	switch actionType {
	case "confirm":
		return action == "allow" || action == "deny"
	case "question":
		return action == "answer" && strings.TrimSpace(value) != ""
	default:
		return false
	}
}

// derefString returns the pointed-to value, or "" if nil. Used for the many
// optional string fields on the Lark SDK event types.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
