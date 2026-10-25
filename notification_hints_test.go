package main

import (
	"strings"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
)

func TestNotificationHintsPreserveModelContent(t *testing.T) {
	t.Parallel()
	r := &NotificationRouter{}
	hint := func(cmd string) string {
		return im.FormatCommandHint(im.PlatformPresentation{Platform: "slack"}, im.CommandTarget{}, cmd)
	}
	state := ControlState{PendingConfirm: &ConfirmPayload{ToolName: "Done", DoneReport: "Keep /allow and /tmp/path in report"}}
	got := r.formatNotification("confirm_request", state, hint)
	if !strings.Contains(got, "Keep /allow and /tmp/path in report") || !strings.Contains(got, "Reply /chord allow") {
		t.Fatal(got)
	}
	state.LastAssistantText = "Use /allow from /tmp/path"
	if got := r.formatNotification("assistant_message", state, hint); got != state.LastAssistantText {
		t.Fatal(got)
	}
}
