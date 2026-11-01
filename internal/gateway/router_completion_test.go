package gateway

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAgentDoneEnvelopeRoutesFullReport(t *testing.T) {
	const report = "Inspection complete.\nChecks: unit tests passed.\n- file_attribution_incomplete: true\n- remaining_limitations: integration tests not run"
	const message = "<system-reminder>\nSubAgent mailbox update:\n- agent_id: agent-1\n- task_id: task-1\n- owner_agent_id: main\n- kind: completed\n- message_type: result\n- correlation_id: completion-1\n- priority: normal\n- summary: " + report + "\n</system-reminder>"
	payload, err := json.Marshal(map[string]string{
		"agent_id":   "agent-1",
		"task_id":    "task-1",
		"agent_type": "reviewer",
		"summary":    "Done",
		"message":    message,
	})
	if err != nil {
		t.Fatal(err)
	}
	sender := &stubIMAdapter{typ: "wechat"}
	router := &NotificationRouter{adapter: sender}
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	var callbackState ControlState
	var callbacks int
	process := &ChordProcess{
		key: key,
		onEvent: func(gotKey, eventType string, state ControlState) {
			callbacks++
			if gotKey != key || eventType != "agent_done" {
				t.Fatalf("callback = %q/%q, want %q/agent_done", gotKey, eventType, key)
			}
			callbackState = state
			router.HandleChordEvent(gotKey, eventType, state)
		},
	}

	process.processEnvelope(&HeadlessEnvelope{Type: "agent_done", Payload: payload})
	if callbacks != 1 || callbackState.LastAgentDone == nil {
		t.Fatalf("callbacks = %d, LastAgentDone = %#v", callbacks, callbackState.LastAgentDone)
	}
	for name, state := range map[string]ControlState{"callback": callbackState, "cache": process.State()} {
		if state.LastAgentDone == nil || state.LastAgentDone.Message != message || state.LastAgentDone.Summary != "Done" {
			t.Fatalf("%s completion = %#v, want unmodified message and summary", name, state.LastAgentDone)
		}
	}
	messages := sender.sentMessages()
	if len(messages) != 1 {
		t.Fatalf("messages = %#v, want one completion notification", messages)
	}
	want := "✅ reviewer · task-1 completed\n" + report
	if got := messages[0]; got.chatID != "chat-1" || got.text != want {
		t.Fatalf("notification = %#v, want chat-1/%q", got, want)
	}
}

func TestFormatAgentDoneNotificationReport(t *testing.T) {
	const mailboxHeader = "SubAgent mailbox update:\n- agent_id: agent-1\n- task_id: task-1\n- kind: completed\n- priority: normal\n- summary: "
	const report = "Inspection complete.\nChecks: unit tests passed.\n\n```text\n- agent_id: sample-agent\n- summary: sample text\n```\n- actual_files_changed: main.go\n- reported_files_changed: main.go, helper.go\n- file_attribution_incomplete: true\n- remaining_limitations: integration tests not run\n- known_risks: output depends on platform\n- follow_up_recommended: run integration tests\n- artifact_refs:\n  - artifacts/report.txt\n- worktree: task-checkout (branch task-branch)\n- worktree_base: main"
	tests := []struct {
		name    string
		message string
		summary string
		want    string
	}{
		{name: "plain report", message: report, summary: "Done", want: report},
		{name: "trim report", message: " \n" + report + "\n ", summary: "Done", want: report},
		{name: "mailbox report", message: mailboxHeader + report, summary: "Done", want: report},
		{name: "wrapped mailbox report", message: "<system-reminder>\n" + mailboxHeader + report + "\n</system-reminder>", summary: "Done", want: report},
		{name: "wrapped plain report", message: "<system-reminder>\n" + report + "\n</system-reminder>", summary: "Done", want: report},
		{name: "summary only", summary: " Done ", want: "Done"},
		{name: "whitespace report", message: " \n\t", summary: "Done", want: "Done"},
		{name: "empty wrapper", message: "<system-reminder>\n</system-reminder>", summary: "Done", want: "Done"},
		{name: "empty mailbox summary", message: mailboxHeader, summary: "Done", want: "Done"},
		{name: "no content"},
		{name: "mailbox without summary", message: "SubAgent mailbox update:\n- agent_id: sample-agent", want: "SubAgent mailbox update:\n- agent_id: sample-agent"},
		{name: "mailbox inside report", message: "Example:\n" + mailboxHeader + "sample text", want: "Example:\n" + mailboxHeader + "sample text"},
		{name: "unpaired wrapper", message: "<system-reminder>\n" + report, want: "<system-reminder>\n" + report},
		{name: "wrapper inside report", message: "Example: <system-reminder>sample</system-reminder>", want: "Example: <system-reminder>sample</system-reminder>"},
	}
	router := &NotificationRouter{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := ControlState{LastAgentDone: &AgentDonePayload{
				AgentID: "agent-1", TaskID: "task-1", AgentType: "reviewer",
				Message: tt.message, Summary: tt.summary,
			}}
			want := "✅ reviewer · task-1 completed"
			if tt.want != "" {
				want += "\n" + tt.want
			}
			if got := router.formatNotification("agent_done", state); got != want {
				t.Fatalf("notification = %q, want %q", got, want)
			}
		})
	}
	if got := router.formatNotification("agent_done", ControlState{}); got != "" {
		t.Fatalf("nil completion notification = %q, want empty", got)
	}
}

func TestFormatAgentDoneNotificationTruncatesReport(t *testing.T) {
	const label = "✅ reviewer · task-1 completed\n"
	report := strings.Repeat("🙂", maxNotificationRunes)
	router := &NotificationRouter{}
	state := ControlState{LastAgentDone: &AgentDonePayload{
		AgentID: "agent-1", TaskID: "task-1", AgentType: "reviewer",
		Message: report, Summary: "Done",
	}}
	got := router.formatNotification("agent_done", state)
	want := label + strings.Repeat("🙂", maxNotificationRunes-utf8.RuneCountInString(label)-3) + "..."
	if got != want || !utf8.ValidString(got) || utf8.RuneCountInString(got) != maxNotificationRunes {
		t.Fatalf("notification = %q, want valid UTF-8 report truncated to %d runes", got, maxNotificationRunes)
	}
}
