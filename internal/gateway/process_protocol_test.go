package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestSessionChangeClearsQuestionHistoryCursor(t *testing.T) {
	for _, env := range []*HeadlessEnvelope{
		{Type: "session_switched", Payload: json.RawMessage(`{"session_id":"new-session"}`)},
		{Type: "status_response", Payload: json.RawMessage(`{"session_id":"new-session"}`)},
	} {
		t.Run(env.Type, func(t *testing.T) {
			writer := &captureWriteCloser{}
			p := &ChordProcess{
				stdin: writer,
				state: ControlState{
					SessionID:            "old-session",
					QuestionBindingID:    "old-binding",
					QuestionsNextAfterID: "old-cursor",
				},
			}
			p.processEnvelope(env)
			if state := p.State(); state.QuestionBindingID != "" || state.QuestionsNextAfterID != "" {
				t.Fatalf("session change retained old question cursor: binding=%q after=%q", state.QuestionBindingID, state.QuestionsNextAfterID)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := p.WaitStatus(ctx)
				done <- err
			}()
			deadline := time.Now().Add(time.Second)
			for writer.String() == "" {
				if time.Now().After(deadline) {
					t.Fatal("WaitStatus did not send a request")
				}
				time.Sleep(time.Millisecond)
			}
			var command map[string]json.RawMessage
			if err := json.Unmarshal([]byte(writer.String()), &command); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"binding_id", "questions_after_id"} {
				if _, ok := command[key]; ok {
					t.Errorf("WaitStatus sent stale %s: %s", key, writer.String())
				}
			}
			cancel()
			<-done
		})
	}
}

func TestReadLoopAcceptsLargeEncodedHeadlessFrame(t *testing.T) {
	// Chord captures up to 512 KiB of local-shell output. JSON escaping can
	// expand that to more than the old 1 MiB scanner limit.
	output := strings.Repeat("\x00", 512*1024)
	payload, err := json.Marshal(map[string]string{"output": output})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(HeadlessEnvelope{Type: "local_shell_result", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	p := &ChordProcess{}
	p.readLoop(t.Context(), bytes.NewReader(append(line, '\n')))
	if p.state.LastLocalShell == nil || p.state.LastLocalShell.Output != output {
		t.Fatal("JSON-escaped shell output was not delivered intact")
	}
}

func TestReadLoopAcceptsHeadlessFrameAtLimit(t *testing.T) {
	const prefix = `{"type":"assistant_message","payload":{"text":"`
	const suffix = `"}}`
	text := strings.Repeat("x", headlessMaxEnvelopeBytes-len(prefix)-len(suffix))
	for _, ending := range []string{"\n", "\r\n", ""} {
		t.Run("ending="+strings.ReplaceAll(strings.ReplaceAll(ending, "\r", "CR"), "\n", "LF"), func(t *testing.T) {
			p := &ChordProcess{}
			p.readLoop(t.Context(), strings.NewReader(prefix+text+suffix+ending))
			if p.state.LastAssistantText != text || p.state.LastTransportError != nil {
				t.Fatalf("frame at limit rejected: error=%q text_len=%d", p.state.LastError, len(p.state.LastAssistantText))
			}
		})
	}
}

func TestReadLoopReportsMalformedFrameAndContinues(t *testing.T) {
	var events []string
	p := &ChordProcess{onEvent: func(_ string, eventType string, _ ControlState) {
		events = append(events, eventType)
	}}
	p.readLoop(t.Context(), strings.NewReader("invalid\n{\"type\":\"assistant_message\",\"payload\":{\"text\":\"sample\"}}\n"))
	if strings.Join(events, ",") != "transport_error,assistant_message,exit" || p.state.LastAssistantText != "sample" {
		t.Fatalf("malformed frame prevented delivery: callbacks=%v state=%+v", events, p.state)
	}
	if failure := p.state.LastTransportError; failure == nil || failure.Fatal || !strings.Contains(failure.Message, "parse headless") {
		t.Fatalf("malformed frame error = %+v", failure)
	}
}

type protocolErrorReader struct{}

func (protocolErrorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestReadLoopReportsStreamFailuresBeforeExit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader io.Reader
	}{
		{name: "one byte over limit", reader: strings.NewReader(strings.Repeat("x", headlessMaxEnvelopeBytes+1) + "\n")},
		{name: "scanner overflow", reader: strings.NewReader(strings.Repeat("x", headlessMaxEnvelopeBytes+64))},
		{name: "read error", reader: protocolErrorReader{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			p := &ChordProcess{pendingInputs: map[string]struct{}{"input-1": {}}, onEvent: func(_ string, eventType string, _ ControlState) {
				events = append(events, eventType)
			}}
			p.readLoop(t.Context(), tc.reader)
			if strings.Join(events, ",") != "transport_error,exit" || !strings.Contains(p.state.LastError, "read headless") {
				t.Fatalf("stream failure was hidden: callbacks=%v error=%q", events, p.state.LastError)
			}
			if !p.stoppedByGateway || p.state.LastTransportError == nil || !p.state.LastTransportError.Fatal {
				t.Fatal("failed transport did not stop the process and record a fatal error")
			}
			if p.state.UnconfirmedInputs != 1 {
				t.Fatalf("unconfirmed inputs = %d, want 1", p.state.UnconfirmedInputs)
			}
		})
	}
}

func TestReadLoopDoesNotReportExpectedShutdownAsTransportFailure(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "gateway stop", true: "cancelled context"}[cancelled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			p := &ChordProcess{stoppedByGateway: !cancelled}
			if cancelled {
				cancel()
			}
			p.readLoop(ctx, protocolErrorReader{})
			if p.state.LastTransportError != nil {
				t.Fatal("expected shutdown produced a transport failure")
			}
		})
	}
}

// The gateway subscribes to final assistant messages, not rollback previews.
// The rollback case also covers the optional decoder path, not subscription.
func TestContinuationFinalMessageReplacesPendingText(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		name := "default_subscription"
		if rollback {
			name = "optional_rollback_event"
		}
		t.Run(name, func(t *testing.T) {
			p := &ChordProcess{state: ControlState{LastAssistantText: "pending"}}
			if rollback {
				p.processEnvelope(&HeadlessEnvelope{Type: "assistant_rollback", Payload: json.RawMessage(`{}`)})
				if p.State().LastAssistantText != "" {
					t.Fatal("rollback retained pending text")
				}
			}
			p.processEnvelope(&HeadlessEnvelope{Type: "assistant_message", Payload: json.RawMessage(`{"text":"final result"}`)})
			if got := p.State().LastAssistantText; got != "final result" {
				t.Fatalf("final text=%q", got)
			}
		})
	}
}
