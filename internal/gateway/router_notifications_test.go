package gateway

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestNotificationsOnlyDeduplicateDeliveredCompanionPrompts(t *testing.T) {
	for _, kind := range []string{"confirm_request", "handoff_request"} {
		for _, fail := range []bool{false, true} {
			t.Run(kind+"/failed="+strconv.FormatBool(fail), func(t *testing.T) {
				sender := &stubIMAdapter{typ: "wechat"}
				if fail {
					sender.sendFunc = func(_ string, _ string) error {
						if len(sender.sentMessages()) == 1 {
							return errors.New("delivery failed")
						}
						return nil
					}
				}
				r := &NotificationRouter{adapter: sender, outbound: newOutboundDispatcher()}
				key := (processKey{workspaceID: "workspace", imType: "wechat", chatID: "chat"}).String()
				state := ControlState{SessionID: "session", PendingConfirm: &ConfirmPayload{RequestID: "confirm-1", ToolName: "Shell"}, PendingHandoff: &HandoffPayload{RequestID: "handoff-1"}}
				companion := "Chord: Permission confirmation required"
				if kind == "handoff_request" {
					companion = "Chord: Handoff requires your decision"
				}
				r.HandleChordEvent(key, kind, state)
				for _, text := range []string{companion, "Chord: Loop requires your decision", "Chord: notify needs your correction", "Future user-input alert"} {
					state.LastNotification = &NotificationPayload{Reason: notificationReasonUserInputRequired, Message: text}
					r.HandleChordEvent(key, "notification", state)
				}
				r.Close()
				messages := sender.sentMessages()
				want := 4
				if fail {
					want = 5 // Failed prompt plus its visible fallback alert.
				}
				if len(messages) != want {
					t.Fatalf("messages=%+v, want %d", messages, want)
				}
				for i, text := range []string{"Loop requires", "notify needs", "Future user-input"} {
					if !strings.Contains(messages[len(messages)-3+i].text, text) {
						t.Fatalf("unrelated alert hidden: %+v", messages)
					}
				}
			})
		}
	}
}

func TestCompanionPromptRequiresMatchingRequestAndSession(t *testing.T) {
	r := &NotificationRouter{}
	state := ControlState{SessionID: "session-1", PendingConfirm: &ConfirmPayload{RequestID: "confirm-1"}, LastNotification: &NotificationPayload{Reason: notificationReasonUserInputRequired, Message: "Chord: Permission confirmation required"}}
	r.recordDeliveredPrompt("binding", "confirm_request", state)
	if !r.duplicatePromptNotification("binding", state) {
		t.Fatal("delivered companion was not recognized")
	}
	state.PendingConfirm = &ConfirmPayload{RequestID: "confirm-2"}
	if r.duplicatePromptNotification("binding", state) {
		t.Fatal("a new request was hidden")
	}
	state.PendingConfirm = &ConfirmPayload{RequestID: "confirm-1"}
	state.SessionID = "session-2"
	if r.duplicatePromptNotification("binding", state) || r.duplicatePromptNotification("other-binding", state) {
		t.Fatal("delivery evidence leaked across sessions or bindings")
	}
}
