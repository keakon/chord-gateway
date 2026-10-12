package main

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestLocalShellCompletionPreservesAgentProgress(t *testing.T) {
	for _, agentStartsFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "shell first", true: "agent first"}[agentStartsFirst], func(t *testing.T) {
			p := &ChordProcess{stdin: &captureWriteCloser{}}
			startAgent := func() {
				p.processEnvelope(&HeadlessEnvelope{Type: "activity", Seq: 1, Payload: json.RawMessage(`{"type":"executing"}`)})
				p.state.TurnStartedAt = time.Now().Add(-time.Minute)
				p.state.TurnToolCalls = 7
				p.state.TurnRetries = 2
			}
			if agentStartsFirst {
				startAgent()
			}
			if err := p.SendCommand(map[string]any{"type": "local_shell", "command": "pwd"}); err != nil {
				t.Fatal(err)
			}
			if !agentStartsFirst {
				startAgent()
			}
			started := p.State().TurnStartedAt
			p.processEnvelope(&HeadlessEnvelope{Type: "local_shell_result", Payload: json.RawMessage(`{"command":"pwd"}`)})
			if state := p.State(); !state.Busy || state.TurnStartedAt != started || state.TurnToolCalls != 7 || state.TurnRetries != 2 {
				t.Fatalf("shell result changed agent progress: %#v", state)
			}
			p.processEnvelope(&HeadlessEnvelope{Type: "idle", Seq: 2})
			if state := p.State(); state.Busy || state.LastTurnDuration < time.Minute || state.LastTurnToolCalls != 7 || state.LastTurnRetries != 2 {
				t.Fatalf("agent completion lost its summary: %#v", state)
			}
		})
	}
}

func TestAgentIdlePreservesPendingShells(t *testing.T) {
	p := &ChordProcess{stdin: &captureWriteCloser{}}
	for range 2 {
		if err := p.SendCommand(map[string]any{"type": "local_shell", "command": "pwd"}); err != nil {
			t.Fatal(err)
		}
	}
	started := p.State().LocalShellStartedAt
	p.processEnvelope(&HeadlessEnvelope{Type: "activity", Seq: 1, Payload: json.RawMessage(`{"type":"streaming"}`)})
	p.processEnvelope(&HeadlessEnvelope{Type: "idle", Seq: 2})
	for remaining := 2; remaining >= 0; remaining-- {
		state := p.State()
		if state.PendingLocalShells != remaining || state.Busy != (remaining > 0) || !state.TurnStartedAt.IsZero() {
			t.Fatalf("unexpected state after shell completion: %#v", state)
		}
		if remaining > 0 {
			if state.LocalShellStartedAt != started {
				t.Fatal("shell interval reset while commands were pending")
			}
			p.processEnvelope(&HeadlessEnvelope{Type: "local_shell_result", Payload: json.RawMessage(`{"command":"pwd"}`)})
		} else if !state.LocalShellStartedAt.IsZero() {
			t.Fatal("completed shells retained their interval")
		}
	}
}

func TestStatusDoesNotMixShellAndAgentWindows(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{false: "agent idle", true: "agent busy"}[busy], func(t *testing.T) {
			p := &ChordProcess{stdin: &captureWriteCloser{}, state: ControlState{SessionID: "session-1"}}
			if err := p.SendCommand(map[string]any{"type": "local_shell", "command": "pwd"}); err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(StatusResponse{SessionID: "session-1", Busy: busy})
			if err != nil {
				t.Fatal(err)
			}
			p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 1, Payload: payload})
			if state := p.State(); !state.Busy || state.TurnStartedAt.IsZero() == busy {
				t.Fatalf("snapshot confused shell and agent work: %#v", state)
			}
			p.processEnvelope(&HeadlessEnvelope{Type: "local_shell_result", Payload: json.RawMessage(`{"command":"pwd"}`)})
			if p.State().Busy != busy {
				t.Fatal("shell result did not retain the Core busy state")
			}
		})
	}
}

func TestFailedShellWriteDoesNotRegisterWork(t *testing.T) {
	p := &ChordProcess{stdin: &errorWriteCloser{err: errors.New("write failed")}}
	if err := p.SendCommand(map[string]any{"type": "local_shell", "command": "pwd"}); err == nil {
		t.Fatal("failed write succeeded")
	}
	if state := p.State(); state.Busy || state.PendingLocalShells != 0 || !state.LocalShellStartedAt.IsZero() {
		t.Fatalf("failed write registered shell work: %#v", state)
	}
}

func TestPendingShellPreventsIdleCleanup(t *testing.T) {
	r := newTestRouter()
	key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat-1"}).String()
	p := &ChordProcess{stdin: &captureWriteCloser{}, key: key, lastActivity: time.Now().Add(-time.Hour)}
	r.mgr.procs[key] = p
	if err := p.SendCommand(map[string]any{"type": "local_shell", "command": "pwd"}); err != nil {
		t.Fatal(err)
	}
	if idle := r.mgr.collectIdleProcesses(time.Minute); len(idle) != 0 {
		t.Fatal("idle cleanup collected a pending shell")
	}
	p.handleExit()
	if state := p.State(); state.Busy || state.PendingLocalShells != 0 || !state.LocalShellStartedAt.IsZero() {
		t.Fatalf("exit retained shell work: %#v", state)
	}
}

func TestSuppressedAgentIdleKeepsShellReminder(t *testing.T) {
	r := newTestRouter()
	key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat-1"}).String()
	p := &ChordProcess{stdin: &captureWriteCloser{}, key: key, onEvent: r.HandleChordEvent}
	r.mgr.procs[key] = p
	t.Cleanup(func() { r.stopReminder(key) })
	if err := p.SendCommand(map[string]any{"type": "local_shell", "command": "pwd"}); err != nil {
		t.Fatal(err)
	}
	r.scheduleCommandReminder(key)
	p.processEnvelope(&HeadlessEnvelope{Type: "idle", Seq: 1, Payload: json.RawMessage(`{"suppress_user_notification":true}`)})
	if len(r.reminders) != 1 {
		t.Fatal("suppressed agent idle stopped the pending shell reminder")
	}
}
