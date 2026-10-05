package main

import (
	"strings"
	"testing"
)

func TestTransportFailuresReachChatWithoutDuplicateExit(t *testing.T) {
	for _, fatal := range []bool{false, true} {
		t.Run(map[bool]string{false: "malformed frame", true: "failed stream"}[fatal], func(t *testing.T) {
			adapter := &stubIMAdapter{typ: "wechat"}
			r := &NotificationRouter{adapter: adapter}
			key := (processKey{workspaceID: "workspace", imType: "wechat", chatID: "chat"}).String()
			state := ControlState{LastError: "sample error", LastTransportError: &TransportErrorPayload{Message: "sample error", Fatal: fatal}}
			if fatal {
				state.UnconfirmedInputs = 2
			}
			r.HandleChordEvent(key, "transport_error", state)
			message := adapter.lastMessage().text
			if !strings.Contains(message, "sample error") {
				t.Fatalf("transport error did not reach chat: %q", message)
			}
			if fatal {
				if !strings.Contains(message, "process was stopped") || !strings.Contains(message, "2 inputs") {
					t.Fatalf("disconnect guidance missing: %q", message)
				}
				if got := r.formatNotification("exit", state); got != "" {
					t.Fatalf("duplicate disconnect notification: %q", got)
				}
			} else if !strings.Contains(message, "reading continues") {
				t.Fatalf("recoverable failure guidance missing: %q", message)
			}
		})
	}
}
