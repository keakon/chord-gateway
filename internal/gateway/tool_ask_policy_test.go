package gateway

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestToolAskPolicyDeniedPreservesInteractions(t *testing.T) {
	pending := &ConfirmPayload{RequestID: "req-1"}
	expired := &ConfirmPayload{RequestID: "req-0"}
	var events []ControlState
	p := &ChordProcess{
		key: "sample",
		state: ControlState{
			PendingConfirm: pending,
			ExpiredConfirm: expired,
			Busy:           true,
		},
		onEvent: func(_ string, kind string, state ControlState) {
			if kind == toolAskPolicyDeniedEvent {
				events = append(events, state)
			}
		},
	}
	p.processEnvelope(&HeadlessEnvelope{Type: toolAskPolicyDeniedEvent, Seq: 1, Payload: json.RawMessage(`{"call_id":"call-1","tool_name":"shell","agent_id":"worker-1","policy":"deny","reason":"This is not user feedback."}`)})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	state := events[0]
	if state.PendingConfirm != pending || state.ExpiredConfirm != expired || !state.Busy || state.LastConfirmResolved != nil {
		t.Fatalf("policy event changed interaction state: %+v", state)
	}
	if got := state.LastToolAskPolicyDenied; got == nil || got.CallID != "call-1" || got.AgentID != "worker-1" || got.Reason != "This is not user feedback." {
		t.Fatalf("policy event = %+v", got)
	}
	r := &NotificationRouter{}
	text := r.formatNotification(toolAskPolicyDeniedEvent, state)
	for _, want := range []string{"shell", "worker-1", "confirm_ask_policy=deny", "No approval is pending for this call"} {
		if !strings.Contains(text, want) {
			t.Fatalf("notification %q missing %q", text, want)
		}
	}
	for _, unwanted := range []string{"/allow", "/deny", "req-1", "expired"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("notification %q contains interaction action %q", text, unwanted)
		}
	}
	if !slices.Contains(configuredHeadlessSubscribeEvents(nil), toolAskPolicyDeniedEvent) {
		t.Fatal("policy denial must be subscribed by default")
	}
	for _, raw := range []string{
		`{"call_id":"call-2","tool_name":"shell","agent_id":"main","policy":"wait"}`,
		`{"tool_name":"shell","agent_id":"main","policy":"deny"}`,
		`{"call_id":3,"policy":"deny"}`,
	} {
		p.processEnvelope(&HeadlessEnvelope{Type: toolAskPolicyDeniedEvent, Seq: 2, Payload: json.RawMessage(raw)})
	}
	if len(events) != 1 {
		t.Fatalf("invalid event replayed previous denial: events=%d", len(events))
	}
}
