package main

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
)

func TestTurnWindowClosesAfterLocalShell(t *testing.T) {
	p := &ChordProcess{stdin: &captureWriteCloser{}}
	if err := p.SendCommand(map[string]any{"type": "local_shell", "command": "pwd"}); err != nil {
		t.Fatal(err)
	}
	p.state.LocalShellStartedAt = time.Now().Add(-time.Hour)
	p.processEnvelope(&HeadlessEnvelope{Type: "local_shell_result", Payload: json.RawMessage(`{"command":"pwd","output":"/tmp/project"}`)})
	if state := p.State(); state.Busy || !state.TurnStartedAt.IsZero() || state.PendingLocalShells != 0 || !state.LocalShellStartedAt.IsZero() {
		t.Fatalf("shell result retained active work: %#v", state)
	}
	p.pendingInputs = map[string]struct{}{"input-1": {}}
	before := time.Now()
	p.processEnvelope(&HeadlessEnvelope{Type: "input_result", Seq: 1, Payload: json.RawMessage(`{"request_id":"input-1","status":"started"}`)})
	if state := p.State(); state.TurnStartedAt.Before(before) {
		t.Fatalf("new input inherited shell start: %v", state.TurnStartedAt)
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "idle", Seq: 2})
	if state := p.State(); state.LastTurnDuration >= time.Minute {
		t.Fatalf("completion includes time before input: %v", state.LastTurnDuration)
	}
}

func TestTurnWindowSurvivesStatusOrdering(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle snapshot", true: "busy snapshot"}[busy], func(t *testing.T) {
			p := &ChordProcess{state: ControlState{SessionID: "session-1"}}
			payload, err := json.Marshal(StatusResponse{SessionID: "session-1", Busy: busy})
			if err != nil {
				t.Fatal(err)
			}
			p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 1, Payload: payload})
			p.processEnvelope(&HeadlessEnvelope{Type: "activity", Seq: 1, Payload: json.RawMessage(`{"type":"executing"}`)})
			if p.State().TurnStartedAt.IsZero() {
				t.Fatal("activity after snapshot did not open a window")
			}
			started := time.Now().Add(-time.Minute)
			p.state.TurnStartedAt = started
			p.state.TurnToolCalls = 3
			p.state.TurnRetries = 2
			p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 2, Payload: payload})
			p.processEnvelope(&HeadlessEnvelope{Type: "activity", Seq: 2, Payload: json.RawMessage(`{"type":"streaming"}`)})
			if state := p.State(); state.TurnStartedAt != started || state.TurnToolCalls != 3 || state.TurnRetries != 2 {
				t.Fatalf("snapshot/activity reset active progress: %#v", state)
			}
			p.processEnvelope(&HeadlessEnvelope{Type: "idle", Seq: 3})
			if state := p.State(); state.LastTurnDuration < time.Minute || state.LastTurnToolCalls != 3 || state.LastTurnRetries != 2 {
				t.Fatalf("completion lost active progress: %#v", state)
			}
		})
	}
}

func TestControlCommandsPreserveTurnProgress(t *testing.T) {
	for _, command := range []string{"confirm", "handoff", "cancel", "local_shell"} {
		t.Run(command, func(t *testing.T) {
			started := time.Now().Add(-20 * time.Minute)
			p := &ChordProcess{stdin: &captureWriteCloser{}, state: ControlState{
				Busy: true, TurnStartedAt: started, TurnToolCalls: 12, TurnRetries: 2,
			}}
			if err := p.SendCommand(map[string]any{"type": command}); err != nil {
				t.Fatal(err)
			}
			if state := p.State(); state.TurnStartedAt != started || state.TurnToolCalls != 12 || state.TurnRetries != 2 {
				t.Fatalf("control reset progress: %#v", state)
			}
			p.processEnvelope(&HeadlessEnvelope{Type: "idle"})
			if state := p.State(); state.LastTurnDuration < 20*time.Minute || state.LastTurnToolCalls != 12 || state.LastTurnRetries != 2 {
				t.Fatalf("completion lost progress: %#v", state)
			}
		})
	}
}

func TestIdleControlCommandsDoNotOpenTurnWindow(t *testing.T) {
	for _, command := range []string{"confirm", "handoff", "cancel"} {
		t.Run(command, func(t *testing.T) {
			writer := &captureWriteCloser{}
			p := &ChordProcess{stdin: writer, pendingInputs: map[string]struct{}{"input-1": {}}}
			if err := p.SendCommand(map[string]any{"type": command}); err != nil {
				t.Fatal(err)
			}
			if state := p.State(); !state.TurnStartedAt.IsZero() || state.Busy != (command != "cancel") {
				t.Fatalf("unexpected idle control state: %#v", state)
			}
			var sent map[string]any
			if err := json.Unmarshal(writer.buf.Bytes(), &sent); err != nil || sent["type"] != command {
				t.Fatalf("control was not forwarded: %v, %#v", err, sent)
			}
			before := time.Now()
			p.processEnvelope(&HeadlessEnvelope{Type: "input_result", Seq: 1, Payload: json.RawMessage(`{"request_id":"input-1","status":"started"}`)})
			if state := p.State(); !state.Busy || state.TurnStartedAt.Before(before) {
				t.Fatalf("actual input did not start fresh work: %#v", state)
			}
		})
	}
}

func TestIdleCancelDoesNotScheduleReminder(t *testing.T) {
	r := newTestRouter()
	key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat-1"}).String()
	p := &ChordProcess{stdin: &captureWriteCloser{}, key: key}
	r.mgr.procs[key] = p
	t.Cleanup(func() { r.stopReminder(key) })
	r.handleCancelCommand(&config.Workspace{ID: "test"}, "chat-1", key, p)
	if p.State().Busy || len(r.reminders) != 0 {
		t.Fatal("idle cancellation started a reminder")
	}
}

func TestControlCommandStartsOnlyAfterSuccessfulWrite(t *testing.T) {
	p := &ChordProcess{stdin: &errorWriteCloser{err: errors.New("write failed")}}
	if err := p.SendCommand(map[string]any{"type": "cancel"}); err == nil {
		t.Fatal("failed write succeeded")
	}
	if state := p.State(); state.Busy || !state.TurnStartedAt.IsZero() {
		t.Fatalf("failed command opened a window: %#v", state)
	}
	p.stdin = &captureWriteCloser{}
	if err := p.SendCommand(map[string]any{"type": "status"}); err != nil {
		t.Fatal(err)
	}
	if state := p.State(); state.Busy || !state.TurnStartedAt.IsZero() {
		t.Fatalf("status command opened a window: %#v", state)
	}
	if err := p.SendCommand(map[string]any{"type": "local_shell", "command": "pwd"}); err != nil {
		t.Fatal(err)
	}
	if state := p.State(); !state.Busy || !state.TurnStartedAt.IsZero() || state.LocalShellStartedAt.IsZero() || state.PendingLocalShells != 1 {
		t.Fatalf("successful command did not open a window: %#v", state)
	}
}

func TestSessionChangesDropTurnProgress(t *testing.T) {
	for _, event := range []string{"session_switched", "status_response"} {
		t.Run(event, func(t *testing.T) {
			p := &ChordProcess{state: ControlState{
				SessionID: "session-1", Busy: true,
				TurnStartedAt: time.Now().Add(-time.Minute), TurnToolCalls: 3, TurnRetries: 2,
				LastTurnDuration: time.Minute, LastTurnToolCalls: 5, LastTurnRetries: 1,
			}}
			p.processEnvelope(&HeadlessEnvelope{Type: event, Seq: 1, Payload: json.RawMessage(`{"session_id":"session-2"}`)})
			if state := p.State(); !state.TurnStartedAt.IsZero() || state.TurnToolCalls != 0 || state.TurnRetries != 0 || state.LastTurnDuration != 0 || state.LastTurnToolCalls != 0 || state.LastTurnRetries != 0 {
				t.Fatalf("session switch retained progress: %#v", state)
			}
		})
	}
}

type turnTerminalWriter struct {
	proc *ChordProcess
	done chan struct{}
}

func (w *turnTerminalWriter) Write(data []byte) (int, error) {
	started := make(chan struct{})
	go func() {
		close(started)
		w.proc.processEnvelope(&HeadlessEnvelope{Type: "local_shell_result", Payload: json.RawMessage(`{"command":"pwd"}`)})
		close(w.done)
	}()
	<-started
	return len(data), nil
}

func (*turnTerminalWriter) Close() error { return nil }

func TestFastCommandResultDoesNotReopenTurn(t *testing.T) {
	r := newTestRouter()
	key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat-1"}).String()
	p := &ChordProcess{key: key, eventLogf: func(string, ...any) {}}
	writer := &turnTerminalWriter{proc: p, done: make(chan struct{})}
	p.stdin = writer
	r.mgr.procs[key] = p
	t.Cleanup(func() { r.stopReminder(key) })
	if err := p.SendCommand(map[string]any{"type": "local_shell", "command": "pwd"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-writer.done:
	case <-time.After(time.Second):
		t.Fatal("terminal event did not complete")
	}
	r.scheduleCommandReminder(key)
	if state := p.State(); state.Busy || !state.TurnStartedAt.IsZero() || len(r.reminders) != 0 {
		t.Fatalf("command scheduling reopened completed work: %#v", state)
	}
}
