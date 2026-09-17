package main

import (
	"strings"
	"testing"
)

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

func TestFormatNotificationBackgroundResult(t *testing.T) {
	r := &NotificationRouter{}

	if got := r.formatNotification("background_result", ControlState{}); got != "" {
		t.Fatalf("missing payload should format to empty, got %q", got)
	}
	if got := r.formatNotification("background_result", ControlState{LastBackgroundResult: &BackgroundResultPayload{Content: "   "}}); got != "" {
		t.Fatalf("blank content should format to empty, got %q", got)
	}

	got := r.formatNotification("background_result", ControlState{LastBackgroundResult: &BackgroundResultPayload{TargetAgentID: "agent-1", Content: "job done"}})
	if !strings.Contains(got, "job done") || !strings.Contains(got, "agent-1") {
		t.Fatalf("background_result message = %q, want content and target agent", got)
	}

	// Without a target agent the result is still delivered, just unlabelled.
	got = r.formatNotification("background_result", ControlState{LastBackgroundResult: &BackgroundResultPayload{Content: "job done"}})
	if !strings.Contains(got, "job done") || strings.Contains(got, " · ") {
		t.Fatalf("background_result without target = %q", got)
	}
}

func TestFormatNotificationContextNotice(t *testing.T) {
	r := &NotificationRouter{}

	if got := r.formatNotification("context_notice", ControlState{}); got != "" {
		t.Fatalf("missing payload should format to empty, got %q", got)
	}
	if got := r.formatNotification("context_notice", ControlState{LastContextNotice: &ContextNoticePayload{Level: "warning", Message: "  "}}); got != "" {
		t.Fatalf("blank message should format to empty, got %q", got)
	}

	for _, tc := range []struct{ level, icon string }{
		{"pressure", "📈"},
		{"imminent", "⚠️"},
		{"warning", "⏳"},
	} {
		got := r.formatNotification("context_notice", ControlState{LastContextNotice: &ContextNoticePayload{Level: tc.level, Message: "context is filling"}})
		if !strings.HasPrefix(got, tc.icon) || !strings.Contains(got, "context is filling") {
			t.Fatalf("level %s message = %q, want prefix %q", tc.level, got, tc.icon)
		}
	}

	// An unknown level still surfaces the notice instead of dropping it.
	got := r.formatNotification("context_notice", ControlState{LastContextNotice: &ContextNoticePayload{Level: "future", Message: "context is filling"}})
	if got != "context is filling" {
		t.Fatalf("unknown level message = %q, want the bare message", got)
	}
}
