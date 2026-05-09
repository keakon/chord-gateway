package main

import "testing"

func TestFormatLongRunningNotificationSuppressesPendingInput(t *testing.T) {
	r := &NotificationRouter{}
	tests := []struct {
		name  string
		state ControlState
	}{
		{
			name: "pending confirm",
			state: ControlState{
				Busy:           true,
				PendingConfirm: &ConfirmPayload{ToolName: "Shell"},
			},
		},
		{
			name: "pending question",
			state: ControlState{
				Busy:            true,
				PendingQuestion: &QuestionPayload{Question: "Continue?"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.formatLongRunningNotification(tt.state); got != "" {
				t.Fatalf("formatLongRunningNotification() = %q, want empty", got)
			}
		})
	}
}

func TestFormatLongRunningNotificationIncludesInternalEvents(t *testing.T) {
	r := &NotificationRouter{}
	state := ControlState{Busy: true, InternalEventsSinceLastPush: 3}
	if got := r.formatLongRunningNotification(state); got != "⏳ Still working (3 internal events)" {
		t.Fatalf("formatLongRunningNotification() = %q", got)
	}
}
