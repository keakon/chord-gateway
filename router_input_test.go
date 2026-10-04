package main

import (
	"encoding/json"
	"testing"
)

func TestInputReceiptsAndActivityDriveReminders(t *testing.T) {
	r := newTestRouter()
	r.adapter = &stubIMAdapter{typ: "wechat"}
	key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat-1"}).String()
	p := &ChordProcess{key: key, stdin: &captureWriteCloser{}, onEvent: r.HandleChordEvent, eventLogf: func(string, ...any) {}}
	r.mgr.procs[key] = p
	t.Cleanup(func() { r.stopReminder(key) })
	sendAndReply := func(status string, seq uint64) {
		t.Helper()
		if err := p.SendUserMessage("request"); err != nil {
			t.Fatal(err)
		}
		var id string
		for k := range p.pendingInputs {
			id = k
		}
		payload, _ := json.Marshal(InputResultPayload{RequestID: id, Status: status})
		p.processEnvelope(&HeadlessEnvelope{Type: "input_result", Seq: seq, Payload: payload})
	}
	sendAndReply(inputHandled, 1)
	if len(r.reminders) != 0 {
		t.Fatal("local command armed reminder")
	}
	sendAndReply(inputStarted, 2)
	if len(r.reminders) != 1 || !p.State().Busy {
		t.Fatal("started input did not arm reminder")
	}
	sendAndReply(inputQueued, 3)
	sendAndReply(inputHandled, 4)
	if len(r.reminders) != 1 || !p.State().Busy {
		t.Fatal("non-started receipt cleared active reminder")
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "idle", Seq: 5, Payload: json.RawMessage(`{"suppress_user_notification":true}`)})
	if len(r.reminders) != 0 {
		t.Fatal("idle retained reminder")
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "activity", Seq: 6, Payload: json.RawMessage(`{"type":"connecting"}`)})
	if len(r.reminders) != 1 || !p.State().Busy {
		t.Fatal("activity did not arm reminder for queued work")
	}
	if r.adapter.(*stubIMAdapter).lastMessage().text != "" {
		t.Fatal("state-only events pushed chat text")
	}
}
