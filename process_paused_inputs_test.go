package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/keakon/chord-gateway/config"
)

func testPausedInputs() *PausedInputs {
	return &PausedInputs{Paused: true, PendingInputCount: 2, Inputs: []PausedInputSummary{
		{InputID: "input-1", OwnerTaskID: "main", Preview: "Review the document", AttachmentCount: 1},
		{InputID: "input-2", OwnerTaskID: "task-2", Preview: "Check the examples"},
	}}
}

func TestPausedInputProjectionRestoresAndReconcilesStatus(t *testing.T) {
	p := &ChordProcess{bindingInstanceID: "binding-1", restoringTransferID: "transfer-1", expectedSessionID: "session-1", readySignal: make(chan struct{}), eventLogf: func(string, ...any) {}}
	payload := map[string]any{"session_id": "session-1", "runtime_id": "runtime-1", "session_epoch": 1, "transfer_id": "transfer-1", "binding_instance_id": "binding-1", "paused_inputs": testPausedInputs()}
	data, _ := json.Marshal(payload)
	p.processEnvelope(&HeadlessEnvelope{Type: "ready_paused", Seq: 3, Payload: data})
	select {
	case <-p.readySignal:
	default:
		t.Fatal("valid paused target did not become ready")
	}
	ready := p.State()
	if ready.PausedInputs == nil || !ready.PausedInputs.Paused || ready.PausedInputs.PendingInputCount != 2 {
		t.Fatal("restore lost the pending input projection")
	}
	text := formatBindingStatus(nil, "", "", ready)
	for _, want := range []string{"Paused", "Pending inputs: 2", "main · input-1", "Review the document", "1 attachments", "task-2 · input-2"} {
		requireContains(t, text, want)
	}
	// A fresh status records explicit continuation and leaves only the parked
	// owner's input. A stale status cannot resurrect the old paused queue.
	remaining := &PausedInputs{PendingInputCount: 1, Inputs: []PausedInputSummary{testPausedInputs().Inputs[1]}}
	data, _ = json.Marshal(StatusResponse{SessionID: "session-1", PausedInputs: remaining})
	p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 4, Payload: data})
	old, _ := json.Marshal(StatusResponse{SessionID: "session-1", PausedInputs: testPausedInputs()})
	p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 3, Payload: old})
	if got := p.State().PausedInputs; got == nil || got.Paused || got.PendingInputCount != 1 || got.Inputs[0].OwnerTaskID != "task-2" {
		t.Fatalf("status did not reconcile remaining queue: %#v", got)
	}
	if !ready.PausedInputs.Paused || ready.PausedInputs.PendingInputCount != 2 {
		t.Fatal("a later status mutated the ready projection")
	}
	p.restoringTransferID = ""
	p.processEnvelope(&HeadlessEnvelope{Type: "session_switched", Seq: 5, Payload: []byte(`{"session_id":"session-2","runtime_id":"runtime-1","session_epoch":2}`)})
	if p.State().PausedInputs != nil {
		t.Fatal("session switch retained the previous session's queue")
	}
	if strings.Contains(formatBindingStatus(nil, "", "", p.State()), "Pending inputs:") {
		t.Fatal("missing projection was reported as a known empty queue")
	}
}

func TestInvalidPausedInputProjectionCannotPublishReady(t *testing.T) {
	tests := []struct {
		name  string
		value *PausedInputs
	}{
		{"missing", nil},
		{"not paused", &PausedInputs{}},
		{"negative total", &PausedInputs{Paused: true, PendingInputCount: -1}},
		{"missing page", &PausedInputs{Paused: true, PendingInputCount: 1}},
		{"invalid owner", &PausedInputs{Paused: true, PendingInputCount: 1, Inputs: []PausedInputSummary{{InputID: "input-1"}}}},
		{"control in owner", &PausedInputs{Paused: true, PendingInputCount: 1, Inputs: []PausedInputSummary{{InputID: "input-1", OwnerTaskID: "task\x00"}}}},
		{"duplicate input", &PausedInputs{Paused: true, PendingInputCount: 2, Inputs: []PausedInputSummary{{InputID: "input-1", OwnerTaskID: "main"}, {InputID: "input-1", OwnerTaskID: "task-2"}}}},
		{"negative attachments", &PausedInputs{Paused: true, PendingInputCount: 1, Inputs: []PausedInputSummary{{InputID: "input-1", OwnerTaskID: "main", AttachmentCount: -1}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &ChordProcess{bindingInstanceID: "binding-1", restoringTransferID: "transfer-1", expectedSessionID: "session-1", readySignal: make(chan struct{})}
			payload, _ := json.Marshal(map[string]any{"session_id": "session-1", "runtime_id": "runtime-1", "session_epoch": 1, "transfer_id": "transfer-1", "binding_instance_id": "binding-1", "paused_inputs": tt.value})
			p.processEnvelope(&HeadlessEnvelope{Type: "ready_paused", Payload: payload})
			select {
			case <-p.readySignal:
				t.Fatal("invalid or unavailable input projection confirmed ready")
			default:
			}
			if p.readyPaused || p.State().PausedInputs != nil || p.State().SessionID != "" {
				t.Fatal("invalid projection partially changed target state")
			}
		})
	}
}

func TestPausedInputPreviewBoundsPreserveEveryDisplayedOwner(t *testing.T) {
	value := &PausedInputs{Paused: true, PendingInputCount: 11}
	for i := range maxPausedInputSummaries {
		value.Inputs = append(value.Inputs, PausedInputSummary{InputID: fmt.Sprintf("input-%d", i), OwnerTaskID: fmt.Sprintf("task-%d", i), Preview: strings.Repeat("文", 180) + "\n\tcontinued", AttachmentCount: 1})
	}
	projection, err := normalizePausedInputs(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range projection.Inputs {
		if utf8.RuneCountInString(input.Preview) > maxPausedInputPreview || !utf8.ValidString(input.Preview) || strings.ContainsAny(input.Preview, "\n\t") {
			t.Fatal("preview escaped its Unicode or single-line bound")
		}
	}
	value.Inputs[0].Preview = "changed"
	if projection.Inputs[0].Preview == "changed" {
		t.Fatal("producer mutated the published input summary")
	}
	text := formatBindingStatus(&config.Workspace{ID: strings.Repeat("w", 1200)}, "", "", ControlState{SessionID: "session-1", PausedInputs: projection})
	if utf8.RuneCountInString(text) > maxNotificationRunes {
		t.Fatal("queue display exceeded the IM message budget")
	}
	for i := range maxPausedInputSummaries {
		requireContains(t, text, fmt.Sprintf("task-%d · input-%d", i, i))
	}
	requireContains(t, text, "3 more queued inputs")
	requireContains(t, text, "Pending inputs: 11")
}

func TestPausedInputsVisibleThroughTargetRestoreAndStatusCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake headless uses a POSIX shell")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "headless-test")
	workspace := t.TempDir()
	ready, _ := json.Marshal(map[string]any{"type": "ready_paused", "payload": map[string]any{"workspace": workspace, "session_id": "session-1", "runtime_id": "runtime-1", "session_epoch": 1, "binding_instance_id": "target-1", "transfer_id": "transfer-1", "paused_inputs": testPausedInputs()}})
	snapshot, _ := json.Marshal(HeadlessEnvelope{Type: "status_response", Seq: 1, Payload: mustPausedStatusJSON(t)})
	script := "#!/bin/sh\nprintf '%s\\n' " + shellQuote(string(ready)) + "\nwhile IFS= read -r line; do\ncase \"$line\" in *'\"type\":\"status\"'*) printf '%s\\n' " + shellQuote(string(snapshot)) + ";; esac\ndone\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ChordPath: binary, Workspaces: []config.Workspace{{ID: "test", Path: workspace}}}
	m := newTestChordManager(cfg)
	m.transfers = newSessionTransferStore(t.TempDir())
	m.pins = newSessionPinStore(t.TempDir())
	key := processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}.String()
	record := takeoverTestRecord("released")
	record.Direction, record.Status.TargetKind, record.Status.Workspace = "incoming", "gateway", cfg.Workspaces[0].Path
	// A source count can be stale; ready notification must use the target's page.
	record.Status.PendingInputs = 99
	if err := m.transfers.update(key, &record, ""); err != nil {
		t.Fatal(err)
	}
	m.takeoverCLI = func(context.Context, string, takeoverRequest) (TransferStatus, error) { return record.Status, nil }
	exited := make(chan struct{})
	router := NewNotificationRouter(m)
	m.SetOnEvent(func(eventKey string, event string, state ControlState) {
		if event == "exit" {
			close(exited)
		}
		router.HandleChordEvent(eventKey, event, state)
	})
	adapter := &stubIMAdapter{typ: "wechat"}
	router.SetAdapter(adapter)
	t.Cleanup(func() {
		m.StopAll(time.Second)
		router.Close()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Error("target read loop did not exit")
		}
	})
	router.observeTakeover(&cfg.Workspaces[0], "chat", key, false, nil)
	text := adapter.lastMessage().text
	for _, want := range []string{"ready_paused", "Pending inputs: 2", "main · input-1", "Review the document", "task-2 · input-2"} {
		requireContains(t, text, want)
	}
	if m.transferBlocked(key) || m.pins.Get(key) != "session-1" || strings.Contains(text, "Pending inputs: 99") {
		t.Fatal("target was not published from its own paused input snapshot")
	}
	router.HandleIncomingMessage(IncomingMessage{IMType: "wechat", ChatID: "chat", Text: "/status"})
	text = adapter.lastMessage().text
	for _, want := range []string{"Paused", "Pending inputs: 2", "Review the document", "task-2 · input-2"} {
		requireContains(t, text, want)
	}
	guardData, err := os.ReadFile(m.transfers.path)
	if err != nil || strings.Contains(string(guardData), "Review the document") {
		t.Fatal("input preview was persisted in transfer guards")
	}
}

func mustPausedStatusJSON(t *testing.T) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(StatusResponse{SessionID: "session-1", PausedInputs: testPausedInputs()})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
