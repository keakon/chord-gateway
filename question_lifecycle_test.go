package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

func TestQuestionCollectionsSurviveIdleAndTargetedClose(t *testing.T) {
	p := &ChordProcess{}
	for _, id := range []string{"q-1", "q-2"} {
		payload, _ := json.Marshal(QuestionPayload{RequestID: id, Version: 1, Question: "Choose"})
		p.processEnvelope(&HeadlessEnvelope{Type: "question_request", Seq: 1, Payload: payload})
	}
	if p.State().uniquePendingQuestion() != nil || p.State().pendingQuestionCount() != 2 {
		t.Fatal("ambiguous target guessed")
	}
	p.transitionToIdle("", true)
	if p.State().pendingQuestionCount() != 2 {
		t.Fatal("idle removed asynchronous questions")
	}
	payload, _ := json.Marshal(QuestionPayload{RequestID: "q-1", Version: 2, Outcome: QuestionResolvedDefaulted, Selected: []string{"First"}})
	p.processEnvelope(&HeadlessEnvelope{Type: "question_resolved", Seq: 2, Payload: payload})
	p.processEnvelope(&HeadlessEnvelope{Type: "question_resolved", Seq: 2, Payload: payload})
	if p.State().uniquePendingQuestion().RequestID != "q-2" || p.State().Questions["q-1"].Outcome != QuestionResolvedDefaulted {
		t.Fatal("targeted close lost other question or original outcome")
	}
	r := &NotificationRouter{}
	if text := r.formatQuestionResolvedNotification(p.State()); !strings.Contains(text, "system decision") {
		t.Fatal("default looks like user consent")
	}
}
func TestQuestionStateRejectsStaleUpdates(t *testing.T) {
	s := ControlState{}
	s.applyQuestionState(&QuestionPayload{RequestID: "q-1", Version: 2, Outcome: QuestionResolvedAnswered})
	old := s
	if s.applyQuestionState(&QuestionPayload{RequestID: "q-1", Version: 1}) {
		t.Fatal("stale update reopened question")
	}
	s.applyQuestionState(&QuestionPayload{RequestID: "q-2", Version: 1})
	if len(old.Questions) != 1 {
		t.Fatal("outbound snapshot changed after update")
	}
}

func TestFeishuQuestionCallbackKeepsCardUntilResolved(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", Owner: &config.OwnerIdentity{AccountID: "cli_test", UserID: "ou_owner"}}
	a := testFeishuAdapter(t, fc)

	for _, requestID := range []string{"q-1", "q-2"} {
		event := makeFeishuCardActionEvent("ou_owner", "oc_chat1", requestID, "answer")
		event.Event.Action.Value["type"] = "question"
		event.Event.Action.Value["value"] = "first"
		response, err := handleCardActionEvent(context.Background(), a, event)
		if err != nil || response == nil || response.Toast == nil || response.Card != nil || response.Toast.Type != "info" {
			t.Fatalf("response=%+v err=%v", response, err)
		}
	}
	if got := len(feishuTestQueue(a, im.IncomingMessage{})); got != 2 {
		t.Fatalf("accepted callbacks=%d", got)
	}

}

func TestQuestionSnapshotDoesNotConsumeCloseOrRevision(t *testing.T) {
	var events []string
	p := &ChordProcess{onEvent: func(_ string, kind string, state ControlState) {
		events = append(events, kind)
		if kind == "question_updated" && state.LastQuestion.RevisionResult == nil {
			t.Fatal("revision notification lost its result")
		}
	}}
	p.processEnvelope(&HeadlessEnvelope{Type: "question_request", Seq: 1, Payload: json.RawMessage(`{"request_id":"q-1","version":1}`)})
	p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 2, Payload: json.RawMessage(`{"questions":[{"request_id":"q-1","version":2,"reason":"answered"}]}`)})
	p.processEnvelope(&HeadlessEnvelope{Type: "question_resolved", Seq: 2, Payload: json.RawMessage(`{"request_id":"q-1","version":2,"reason":"answered"}`)})
	p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 3, Payload: json.RawMessage(`{"questions":[{"request_id":"q-1","version":3,"reason":"answered"}]}`)})
	p.processEnvelope(&HeadlessEnvelope{Type: "question_updated", Seq: 3, Payload: json.RawMessage(`{"request_id":"q-1","version":3,"reason":"answered","revision_result":{"result_id":"revision-1","selected":["Revised"]}}`)})
	if got := strings.Join(events, ","); got != "question_request,question_resolved,question_updated" {
		t.Fatalf("events = %s", got)
	}
}

func TestQuestionStatusRetriesPresentation(t *testing.T) {
	var events []questionEvent
	p := &ChordProcess{onEvent: func(_ string, kind string, state ControlState) {
		events = append(events, questionEvent{kind: kind, state: state})
	}}
	p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 2, Payload: json.RawMessage(`{"questions":[{"request_id":"q-1","version":2,"timer":"cancelled"},{"request_id":"historical","version":3,"reason":"answered"}]}`)})
	if len(events) != 1 || events[0].state.LastQuestion.RequestID != "q-1" {
		t.Fatalf("snapshot did not present the open question: %+v", events)
	}
}

func TestQuestionSnapshotClosureDoesNotConsumeSameVersionRevision(t *testing.T) {
	var events []string
	p := &ChordProcess{onEvent: func(_ string, kind string, _ ControlState) { events = append(events, kind) }}
	p.processEnvelope(&HeadlessEnvelope{Type: "question_request", Seq: 1, Payload: json.RawMessage(`{"request_id":"q-1","version":1}`)})
	p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 4, Payload: json.RawMessage(`{"questions":[{"request_id":"q-1","version":4,"reason":"answered"}]}`)})
	for _, revision := range []struct {
		version uint64
		id      string
	}{{3, "revision-1"}, {4, "revision-2"}} {
		payload, _ := json.Marshal(QuestionPayload{RequestID: "q-1", Version: revision.version, Outcome: QuestionResolvedAnswered, RevisionResult: &QuestionRevision{ResultID: revision.id}})
		p.processEnvelope(&HeadlessEnvelope{Type: "question_updated", Seq: revision.version, Payload: payload})
		p.processEnvelope(&HeadlessEnvelope{Type: "question_updated", Seq: revision.version, Payload: payload})
	}
	if got := strings.Join(events, ","); got != "question_request,question_resolved,question_updated,question_updated" {
		t.Fatalf("snapshot consumed revisions or duplicate was delivered: %s", got)
	}
}

func TestQuestionPresentationFailuresCanBeRetriedFromStatus(t *testing.T) {
	for _, failure := range []string{"write", "rejected receipt"} {
		t.Run(failure, func(t *testing.T) {
			key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat1"}).String()
			mgr := newTestChordManager(&config.Config{})
			p := &ChordProcess{key: key, stdin: &captureWriteCloser{}}
			if failure == "write" {
				p.stdin = &errorWriteCloser{err: errors.New("closed pipe")}
			}
			mgr.procs[key] = p
			sender := &stubIMAdapter{typ: "wechat"}
			r := &NotificationRouter{mgr: mgr, adapter: sender}
			p.onEvent = r.HandleChordEvent
			p.processEnvelope(&HeadlessEnvelope{Type: "question_request", Seq: 1, Payload: json.RawMessage(`{"request_id":"q-1","version":1,"timer":"armed","question":"Choose"}`)})
			if failure != "write" {
				p.processEnvelope(&HeadlessEnvelope{Type: "question_receipt", Payload: json.RawMessage(`{"operation_id":"gateway:presented:q-1:1","request_id":"q-1","accepted":false,"error":"retry needed"}`)})
			}
			if !strings.Contains(sender.lastMessage().text, "/status") {
				t.Fatal("presentation failure was not visible")
			}
			stdin := &captureWriteCloser{}
			p.stdin = stdin
			p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 2, Payload: json.RawMessage(`{"questions":[{"request_id":"q-1","version":1,"timer":"armed","question":"Choose"}]}`)})
			if !strings.Contains(stdin.String(), `"action":"presented"`) {
				t.Fatal("status did not retry presentation")
			}
			p.processEnvelope(&HeadlessEnvelope{Type: "question_updated", Seq: 3, Payload: json.RawMessage(`{"request_id":"q-1","version":2,"timer":"cancelled","question":"Choose"}`)})
			if len(sender.sentMessages()) != 2 || !strings.Contains(sender.lastMessage().text, "Choose") {
				t.Fatal("committed cancellation did not expose the question")
			}
		})
	}
}

func TestQuestionRetryReusesOriginalOperationAfterStatus(t *testing.T) {
	r := &NotificationRouter{}
	stdin := &captureWriteCloser{}
	p := &ChordProcess{stdin: stdin}
	command := map[string]any{"type": "question", "request_id": "q-1", "operation_id": "original", "action": "answer", "answers": []string{"first"}, "custom": false}
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if _, err := r.waitQuestionOperation(ctx, "binding", "sender", p, command); err == nil {
		t.Fatal("missing receipt should leave an uncertain operation")
	}
	attachQuestionTestReceipts(p)
	command["operation_id"] = "new-click"
	command["action"] = "revise"
	if _, err := r.waitQuestionOperation(t.Context(), "binding", "sender", p, command); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdin.String()), "\n")
	if len(lines) != 2 || lines[0] != lines[1] || len(r.questionOperations) != 0 {
		t.Fatalf("retry changed the operation: %s", stdin.String())
	}
}

func TestHistoricalQuestionAnswerDoesNotInventBusy(t *testing.T) {
	r := newTestRouter()
	sender := &stubIMAdapter{typ: "wechat"}
	r.adapter = sender
	key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat-1"}).String()
	p := &ChordProcess{key: key, stdin: &captureWriteCloser{}}
	attachQuestionTestReceipts(p)
	r.mgr.procs[key] = p
	q := &QuestionPayload{RequestID: "old", Version: 2, Outcome: QuestionResolvedAnswered}
	if !r.submitQuestionAnswer("chat-1", im.IncomingMessage{IMType: "wechat", MessageID: "answer-1", Text: "/answer @old Revised"}, key, p, q, []string{"Revised"}) {
		t.Fatal("revision was not accepted")
	}
	if p.State().Busy || len(r.reminders) != 0 {
		t.Fatal("durable acceptance invented an active turn")
	}
}

func TestQuestionIMEntryWaitsForCommittedTimerCancellation(t *testing.T) {
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat1"}).String()
	mgr := newTestChordManager(&config.Config{})
	stdin := &captureWriteCloser{}
	mgr.procs[key] = &ChordProcess{key: key, stdin: stdin}
	sender := &stubIMAdapter{typ: "wechat"}
	r := &NotificationRouter{mgr: mgr, adapter: sender}
	q := &QuestionPayload{RequestID: "q-1", Version: 1, BindingID: "binding-1", Question: "Choose", Timer: "armed"}
	state := ControlState{LastQuestion: q, Questions: map[string]*QuestionPayload{q.RequestID: q}}
	r.HandleChordEvent(key, "question_request", state)
	if sender.lastMessage().text != "" || !strings.Contains(stdin.String(), `"supports_interaction":false`) || !strings.Contains(stdin.String(), `"binding_id":"binding-1"`) {
		t.Fatal("IM entry opened before Core cancellation")
	}
	next := *q
	next.Version++
	next.Timer = "cancelled"
	state.LastQuestion = &next
	state.Questions = map[string]*QuestionPayload{q.RequestID: &next}
	r.HandleChordEvent(key, "question_updated", state)
	r.HandleChordEvent(key, "question_updated", state)
	if sender.lastMessage().text == "" || len(sender.sent) != 1 {
		t.Fatalf("entry missing or repeated: %+v", sender.sent)
	}
}

func TestQuestionHeadlessWireTerminalAndRevision(t *testing.T) {
	p := &ChordProcess{}
	// Exercise the documented headless frames directly, independently of the
	// gateway payload's JSON tags so serialization cannot mask contract drift.
	frames := []string{
		`{"type":"question_request","seq":1,"payload":{"request_id":"q-1","binding_id":"binding-1","task_id":"main","request_scope_id":"scope-1","version":1,"question":"Choose","timer":"awaiting_presentation"}}`,
		`{"type":"question_resolved","seq":2,"payload":{"request_id":"q-1","binding_id":"binding-1","task_id":"main","request_scope_id":"scope-1","version":2,"question":"Choose","timer":"disabled","reason":"defaulted","result_id":"result-1","selected":["Brief"],"selected_ids":["brief"]}}`,
		`{"type":"question_updated","seq":3,"payload":{"request_id":"q-1","binding_id":"binding-1","task_id":"main","request_scope_id":"scope-1","version":3,"question":"Choose","timer":"disabled","reason":"defaulted","result_id":"result-1","revision_result":{"question_id":"q-1","result_id":"revision-1","outcome":"answered","selected":["Detailed"]}}}`,
	}
	for _, frame := range frames {
		var env HeadlessEnvelope
		if err := json.Unmarshal([]byte(frame), &env); err != nil {
			t.Fatal(err)
		}
		p.processEnvelope(&env)
	}
	state := p.State()
	q := state.Questions["q-1"]
	if state.pendingQuestionCount() != 0 || q.Outcome != QuestionResolvedDefaulted || q.ResultID != "result-1" || q.ScopeID != "scope-1" || q.RevisionResult == nil || q.RevisionResult.ResultID != "revision-1" || q.RevisionResult.Outcome != QuestionResolvedAnswered {
		t.Fatalf("headless terminal or revision contract lost: %#v", q)
	}
}
