package gateway

import (
	"strings"
	"testing"
	"time"
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
				Phase:     "waiting_input",
				Busy:      true,
				Questions: map[string]*QuestionPayload{"q-test": {Question: "Continue?", RequestID: "q-test", Version: 1}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.formatLongRunningNotification(tt.state, time.Now()); got != "" {
				t.Fatalf("formatLongRunningNotification() = %q, want empty", got)
			}
		})
	}
}

func TestAsynchronousQuestionsDoNotSuppressActiveWork(t *testing.T) {
	r := &NotificationRouter{}
	state := ControlState{Busy: true, Phase: "thinking", Questions: map[string]*QuestionPayload{"q-1": {RequestID: "q-1", Version: 1}}, LastNotification: &NotificationPayload{Reason: notificationReasonUserInputRequired, Message: "Chord: notify needs your correction"}}
	if r.formatLongRunningNotification(state, time.Now()) == "" || r.formatHeadlessNotification(state) == "" {
		t.Fatal("an asynchronous question suppressed unrelated work")
	}
	state.Phase = "waiting_input"
	if r.formatLongRunningNotification(state, time.Now()) != "" || r.formatHeadlessNotification(state) == "" {
		t.Fatal("waiting for a question should pause reminders without hiding unrelated alerts")
	}
}

func TestDefaultedQuestionCardIsASystemDecision(t *testing.T) {
	title, message, template := questionResolvedCardStatus(QuestionResolvedDefaulted)
	if title != "Question defaulted" || template != "grey" || !strings.Contains(message, "system decision") {
		t.Fatalf("defaulted card = %q, %q, %q", title, message, template)
	}
}

func TestFormatLongRunningNotificationIncludesInternalEvents(t *testing.T) {
	r := &NotificationRouter{}
	state := ControlState{Busy: true, InternalEventsSinceLastPush: 3}
	if got := r.formatLongRunningNotification(state, time.Now()); got != "⏳ Still working (3 internal events)" {
		t.Fatalf("formatLongRunningNotification() = %q", got)
	}
}

func TestFormatLongRunningNotificationIncludesTurnProgress(t *testing.T) {
	r := &NotificationRouter{}
	now := time.Now()
	state := ControlState{
		Busy:                        true,
		Phase:                       "executing",
		TurnStartedAt:               now.Add(-12*time.Minute - 3*time.Second),
		TurnToolCalls:               7,
		TurnRetries:                 2,
		InternalEventsSinceLastPush: 4,
	}
	want := "⏳ Still working (12m03s · running tools · 7 tools · 2 retries · 4 internal events)"
	if got := r.formatLongRunningNotification(state, now); got != want {
		t.Fatalf("formatLongRunningNotification() = %q, want %q", got, want)
	}

	// Transport-level phases stay hidden; the elapsed turn time alone is
	// still reported.
	state.Phase = "connecting"
	state.TurnToolCalls = 0
	state.TurnRetries = 0
	state.InternalEventsSinceLastPush = 0
	if got := r.formatLongRunningNotification(state, now); got != "⏳ Still working (12m03s)" {
		t.Fatalf("transport phase leaked into reminder: %q", got)
	}
}

func TestFormatTurnStageRetryPhases(t *testing.T) {
	// Both retry activity types label the reminder "retrying"; they are also
	// what isRetryPhase counts, so the label and the counter cannot drift.
	for _, phase := range []string{"retrying", "retrying_key"} {
		if got := formatTurnStage(phase); got != "retrying" {
			t.Fatalf("formatTurnStage(%q) = %q, want retrying", phase, got)
		}
		if !isRetryPhase(phase) {
			t.Fatalf("isRetryPhase(%q) = false, want true", phase)
		}
	}
	if formatTurnStage("connecting") != "" || isRetryPhase("connecting") {
		t.Fatal("transport phases must stay hidden and uncounted")
	}
}

func TestFormatLocalShellProgress(t *testing.T) {
	r := &NotificationRouter{}
	now := time.Now()
	state := ControlState{
		Busy: true, PendingLocalShells: 1, LocalShellStartedAt: now.Add(-6 * time.Minute),
		PendingConfirm: &ConfirmPayload{RequestID: "confirm-1"},
	}
	if got := r.formatLongRunningNotification(state, now); got != "⏳ Still working (6m · running local shell)" {
		t.Fatalf("shell reminder = %q", got)
	}
	state.TurnStartedAt = now.Add(-10 * time.Minute)
	state.TurnToolCalls = 7
	state.TurnRetries = 2
	if got := r.formatLongRunningNotification(state, now); got != "⏳ Still working (6m · running local shell)" {
		t.Fatalf("agent waiting for input changed shell reminder: %q", got)
	}
	state.LastTurnDuration = time.Minute
	state.LastTurnToolCalls = 7
	want := "⏳ Chord: Ready for input; local shell commands still running: 1 (1m · 7 tools)"
	if got := r.formatIdleNotification(state); got != want {
		t.Fatalf("idle with pending shell = %q, want %q", got, want)
	}
	state.RunningJobs = 2
	want = "⏳ Chord: Ready for input; background jobs still running: 2; local shell commands still running: 1 (1m · 7 tools)"
	if got := r.formatIdleNotification(state); got != want {
		t.Fatalf("idle with pending jobs and shell = %q, want %q", got, want)
	}
}

func TestFormatTurnSummary(t *testing.T) {
	tests := []struct {
		duration       time.Duration
		tools, retries int
		want           string
	}{
		{0, 0, 0, ""},
		{500 * time.Millisecond, 0, 0, ""},
		{42 * time.Second, 1, 1, "42s · 1 tool · 1 retry"},
		{3 * time.Second, 12, 2, "3s · 12 tools · 2 retries"},
	}
	for _, tt := range tests {
		if got := formatTurnSummary(tt.duration, tt.tools, tt.retries); got != tt.want {
			t.Fatalf("formatTurnSummary(%v, %d, %d) = %q, want %q", tt.duration, tt.tools, tt.retries, got, tt.want)
		}
	}
}

func TestFormatIdleNotificationIncludesTurnSummary(t *testing.T) {
	r := &NotificationRouter{}
	state := ControlState{
		LastTurnDuration:  1*time.Minute + 42*time.Second,
		LastTurnToolCalls: 12,
		LastTurnRetries:   2,
	}
	want := "✅ Chord: Ready for input (1m42s · 12 tools · 2 retries)"
	if got := r.formatIdleNotification(state); got != want {
		t.Fatalf("formatIdleNotification() = %q, want %q", got, want)
	}

	// Running background jobs keep their own message and still carry the
	// completed turn's summary.
	state.RunningJobs = 1
	want = "⏳ Chord: Ready for input; background jobs still running: 1 (1m42s · 12 tools · 2 retries)"
	if got := r.formatIdleNotification(state); got != want {
		t.Fatalf("formatIdleNotification() with jobs = %q, want %q", got, want)
	}

	// A turn with nothing measurable keeps the plain confirmation.
	if got := r.formatIdleNotification(ControlState{RunningJobs: 2}); got != "⏳ Chord: Ready for input; background jobs still running: 2" {
		t.Fatalf("plain idle with jobs = %q", got)
	}
	if got := r.formatIdleNotification(ControlState{}); got != "✅ Chord: Ready for input" {
		t.Fatalf("plain idle = %q", got)
	}
}

func TestFormatTurnDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{42 * time.Second, "42s"},
		{time.Minute, "1m"},
		{time.Minute + 42*time.Second, "1m42s"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1h02m"},
	}
	for _, tt := range tests {
		if got := formatTurnDuration(tt.d); got != tt.want {
			t.Fatalf("formatTurnDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
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
