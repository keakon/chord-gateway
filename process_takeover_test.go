package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
)

func newTakeoverTestProcess(t *testing.T) (*ChordManager, *ChordProcess, *captureWriteCloser) {
	t.Helper()
	m := newTestChordManager(testConfig())
	m.transfers = newSessionTransferStore(t.TempDir())
	m.pins = newSessionPinStore(t.TempDir())
	key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}).String()
	w := &captureWriteCloser{}
	p := &ChordProcess{mgr: m, key: key, stdin: w, runtimeID: "runtime-1", sessionEpoch: 1, bindingInstanceID: "binding-1", workspacePath: "/workspace", autoRestart: true, eventLogf: func(string, ...any) {}}
	p.state.SessionID = "session-1"
	p.state.BindingInstanceID = p.bindingInstanceID
	m.procs[key] = p
	return m, p, w
}

func takeoverEnvelope(t *testing.T, event string, status TransferStatus) *HeadlessEnvelope {
	t.Helper()
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	return &HeadlessEnvelope{Type: event, Payload: data}
}

func takeoverCommands(t *testing.T, w *captureWriteCloser) []map[string]any {
	t.Helper()
	var result []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(w.String()), "\n") {
		if line == "" {
			continue
		}
		var command map[string]any
		if err := json.Unmarshal([]byte(line), &command); err != nil {
			t.Fatal(err)
		}
		result = append(result, command)
	}
	return result
}

func TestTransferPreparationOrdersInputsAndRetainsUnconfirmedReceipts(t *testing.T) {
	m, p, w := newTakeoverTestProcess(t)
	if err := m.pins.Set(p.key, p.state.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := p.SendUserMessage("queued instruction"); err != nil {
		t.Fatal(err)
	}
	commands := takeoverCommands(t, w)
	id := commands[0]["request_id"].(string)
	status := takeoverTestRecord("preparing").Status
	p.processEnvelope(takeoverEnvelope(t, "session_transfer_preparing", status))
	commands = takeoverCommands(t, w)
	if len(commands) != 2 || commands[0]["type"] != "send" || commands[1]["type"] != "session_transfer_prepared" || commands[1]["accepted"] != true {
		t.Fatalf("command order=%#v", commands)
	}
	if !m.transferBlocked(p.key) {
		t.Fatal("ACK preceded persistent guard")
	}
	if err := p.SendUserMessage("too late"); !errors.Is(err, ErrSessionTransferred) {
		t.Fatalf("late input error=%v", err)
	}
	if err := p.SendCommand(map[string]any{"type": "image_begin"}); !errors.Is(err, ErrSessionTransferred) {
		t.Fatalf("late image error=%v", err)
	}
	p.processEnvelope(takeoverEnvelope(t, "session_transfer_saved", status))
	guarded, _, _ := m.transfers.Get(p.key)
	if guarded.Status.State != "preparing" || m.pins.Get(p.key) != "session-1" || len(p.pendingInputs) != 1 {
		t.Fatal("saved accepted before pending receipt resolved")
	}
	receipt, _ := json.Marshal(InputResultPayload{RequestID: id, Status: inputQueued})
	p.processEnvelope(&HeadlessEnvelope{Type: "input_result", Payload: receipt})
	p.processEnvelope(takeoverEnvelope(t, "session_transfer_saved", status))
	guarded, _, _ = m.transfers.Get(p.key)
	if guarded.Status.State != "saved" || m.pins.Get(p.key) != "" || len(p.pendingInputs) != 0 {
		t.Fatalf("confirmed save=%#v pin=%q pending=%d", guarded, m.pins.Get(p.key), len(p.pendingInputs))
	}
	// A delayed release from this source must not clear the replacement pin.
	if err := m.pins.Set(p.key, "session-new"); err != nil {
		t.Fatal(err)
	}
	p.processEnvelope(takeoverEnvelope(t, "session_transfer_released", status))
	if m.pins.Get(p.key) != "session-new" {
		t.Fatal("old release cleared replacement pin")
	}
}

func TestTransferPreparationWriterFailureRejectsACKAndBlocksSend(t *testing.T) {
	m, p, w := newTakeoverTestProcess(t)
	m.transfers.writer = func(string, []byte) error { return errors.New("disk unavailable") }
	p.processEnvelope(takeoverEnvelope(t, "session_transfer_preparing", takeoverTestRecord("preparing").Status))
	commands := takeoverCommands(t, w)
	if len(commands) != 1 || commands[0]["accepted"] != false || commands[0]["error_code"] != "persistence_failed" {
		t.Fatalf("failure ACK=%#v", commands)
	}
	if err := p.SendUserMessage("late instruction"); !errors.Is(err, ErrSessionTransferred) {
		t.Fatalf("input reopened on uncertain Preparation: %v", err)
	}
	if _, err := m.GetOrSpawnForKey(p.key); !errors.Is(err, ErrSessionTransferred) {
		t.Fatalf("manager ignored failed preparation guard: %v", err)
	}
	if p.State().LastTransportError == nil {
		t.Fatal("persistence failure was hidden")
	}
}

func TestTransferPreparationRejectsStaleSourceIdentity(t *testing.T) {
	m, p, w := newTakeoverTestProcess(t)
	status := takeoverTestRecord("preparing").Status
	status.SourceRuntimeID = "runtime-old"
	p.processEnvelope(takeoverEnvelope(t, "session_transfer_preparing", status))
	if m.transferBlocked(p.key) || p.transferActive() || w.String() != "" {
		t.Fatal("stale source froze current binding or received ACK")
	}
	if err := p.SendUserMessage("current instruction"); err != nil {
		t.Fatal(err)
	}
}

func TestTransferExitCodeWithoutEventsPreventsRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake source uses a POSIX shell")
	}
	m, p, _ := newTakeoverTestProcess(t)
	command := exec.Command("/bin/sh", "-c", "exit 20")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	p.cmd = command
	exited := make(chan struct{})
	p.onEvent = func(_ string, event string, _ ControlState) {
		if event == "exit" {
			close(exited)
		}
	}
	p.handleExit()
	select {
	case <-exited:
	default:
		t.Fatal("exit callback missing")
	}
	if p.exitCode != sessionTransferExitCode || !m.transferBlocked(p.key) {
		t.Fatal("dedicated exit did not preserve transfer guard")
	}
	if _, err := m.GetOrSpawnForKey(p.key); !errors.Is(err, ErrSessionTransferred) {
		t.Fatalf("exit 20 allowed restart: %v", err)
	}
	guard, _, _ := m.transfers.Get(p.key)
	if guard.Status.State != "status_unknown" {
		t.Fatal("event-free exit invented successful checkpoint")
	}
}

func takeoverRestoreBinary(t *testing.T, eventType, session, transfer, binding, workspace string) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake target uses a POSIX shell")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-target")
	args := filepath.Join(dir, "args")
	payload, _ := json.Marshal(map[string]any{"type": eventType, "payload": map[string]any{"session_id": session, "transfer_id": transfer, "binding_instance_id": binding, "workspace": workspace, "runtime_id": "runtime-target", "session_epoch": 2, "paused_inputs": PausedInputs{Paused: true}}})
	script := "#!/bin/sh\nif [ \"$2\" = '--help' ]; then printf '%s\\n' '--gateway-managed --gateway-instance --gateway-transfer-id'; exit 0; fi\nprintf '%s\\n' \"$@\" > " + shellQuote(args) + "\nprintf '%s\\n' " + shellQuote(string(payload)) + "\n"
	if eventType == "ready_paused" && session == "session-1" && transfer == "transfer-1" && binding == "target-1" {
		script += "while IFS= read -r line; do :; done\n"
	} else {
		script += "exit 1\n"
	}
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary, args
}

func TestTransferredRestoreRequiresMatchingReadyPausedAndNeverFresh(t *testing.T) {
	cases := []struct {
		name, event, session, transfer, binding string
		good                                    bool
	}{
		{"matching", "ready_paused", "session-1", "transfer-1", "target-1", true},
		{"ordinary ready", "ready", "session-1", "transfer-1", "target-1", false},
		{"wrong session", "ready_paused", "other", "transfer-1", "target-1", false},
		{"wrong transfer", "ready_paused", "session-1", "other", "target-1", false},
		{"wrong instance", "ready_paused", "session-1", "transfer-1", "other", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			binary, argsFile := takeoverRestoreBinary(t, tt.event, tt.session, tt.transfer, tt.binding, workspace)
			cfg := &config.Config{ChordPath: binary, Workspaces: []config.Workspace{{ID: "test", Path: workspace}}}
			m := newTestChordManager(cfg)
			m.takeoverCLI = func(context.Context, string, takeoverRequest) (TransferStatus, error) {
				return TransferStatus{}, errors.New("coordinator not used by restore")
			}
			m.transfers = newSessionTransferStore(t.TempDir())
			m.pins = newSessionPinStore(t.TempDir())
			key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}).String()
			record := takeoverTestRecord("released")
			record.Direction = "incoming"
			record.Status.TargetKind = "gateway"
			record.Status.Workspace = cfg.Workspaces[0].Path
			if err := m.transfers.update(key, &record, ""); err != nil {
				t.Fatal(err)
			}
			// Wait for child stdout EOF explicitly so tests leave no read-loop callback behind.
			exited := make(chan struct{})
			m.onEvent = func(_ string, event string, _ ControlState) {
				if event == "exit" {
					close(exited)
				}
			}
			t.Cleanup(func() {
				m.StopAll(time.Second)
				select {
				case <-exited:
				case <-time.After(5 * time.Second):
					t.Error("target read loop did not exit")
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			p, err := m.restoreTransferredSession(ctx, key, record)
			args, readErr := os.ReadFile(argsFile)
			if readErr != nil {
				t.Fatal(readErr)
			}
			requireContains(t, string(args), "--resume\nsession-1\n")
			requireContains(t, string(args), "--gateway-transfer-id\ntransfer-1\n")
			if tt.good {
				if err != nil || p == nil || !p.readyPaused || m.transferBlocked(key) || m.pins.Get(key) != "session-1" {
					t.Fatalf("restore p=%v err=%v blocked=%v pin=%q", p, err, m.transferBlocked(key), m.pins.Get(key))
				}
			} else {
				if err == nil || p != nil || !m.transferBlocked(key) || m.pins.Get(key) != "" {
					t.Fatalf("invalid readiness bound target p=%v error=%v", p, err)
				}
				if _, spawnErr := m.GetOrSpawnForKey(key); !errors.Is(spawnErr, ErrSessionTransferred) {
					t.Fatalf("failed restore triggered fresh: %v", spawnErr)
				}
			}
		})
	}
}

// The barrier holds an already admitted stdin write so preparation must acquire
// the same command-order boundary before it can publish its ACK.
type takeoverBarrierWriter struct {
	captureWriteCloser
	entered chan struct{}
	release chan struct{}
}

func (w *takeoverBarrierWriter) Write(data []byte) (int, error) {
	if strings.Contains(string(data), `"type":"send"`) {
		close(w.entered)
		<-w.release
	}
	return w.captureWriteCloser.Write(data)
}

func TestTransferPreparationWaitsForExistingStdinWrite(t *testing.T) {
	m, p, _ := newTakeoverTestProcess(t)
	writer := &takeoverBarrierWriter{entered: make(chan struct{}), release: make(chan struct{})}
	p.stdin = writer
	sendDone := make(chan error, 1)
	go func() { sendDone <- p.SendUserMessage("instruction in pipe") }()
	<-writer.entered
	prepDone := make(chan struct{})
	go func() {
		p.processEnvelope(takeoverEnvelope(t, "session_transfer_preparing", takeoverTestRecord("preparing").Status))
		close(prepDone)
	}()
	close(writer.release)
	if err := <-sendDone; err != nil {
		t.Fatal(err)
	}
	<-prepDone
	commands := takeoverCommands(t, &writer.captureWriteCloser)
	if len(commands) != 2 || commands[0]["type"] != "send" || commands[1]["type"] != "session_transfer_prepared" || commands[1]["accepted"] != true {
		t.Fatalf("stdin cutoff order=%#v", commands)
	}
	if !m.transferBlocked(p.key) || len(p.pendingInputs) != 1 {
		t.Fatal("preparing discarded in-pipe input or omitted guard")
	}
}

func TestLateTransferExitCannotBlockReplacementBinding(t *testing.T) {
	m, old, _ := newTakeoverTestProcess(t)
	replacement := &ChordProcess{mgr: m, key: old.key, stdin: &captureWriteCloser{}, bindingInstanceID: "binding-new"}
	replacement.state.SessionID = "session-new"
	m.procs[old.key] = replacement
	old.exitCode = sessionTransferExitCode
	old.handleExit()
	if m.transferBlocked(old.key) || m.GetProcessForKey(old.key) != replacement {
		t.Fatal("old transfer exit poisoned current binding")
	}
	got, err := m.GetOrSpawnForKey(old.key)
	if err != nil || got != replacement {
		t.Fatalf("replacement unavailable after old exit: %v", err)
	}
}

func TestTransferExitGuardWriterFailureFailsClosed(t *testing.T) {
	m, p, _ := newTakeoverTestProcess(t)
	m.transfers.writer = func(string, []byte) error { return errors.New("disk unavailable") }
	p.exitCode = sessionTransferExitCode
	p.handleExit()
	if !m.transferBlocked(p.key) || p.State().LastTransportError == nil {
		t.Fatal("event-free exit with write failure allowed automatic recovery")
	}
	if _, err := m.GetOrSpawnForKey(p.key); !errors.Is(err, ErrSessionTransferred) {
		t.Fatalf("write failure reopened binding: %v", err)
	}
}

func TestTransferredReadyTargetPinFailureCanRetrySameProcess(t *testing.T) {
	workspace := t.TempDir()
	binary, _ := takeoverRestoreBinary(t, "ready_paused", "session-1", "transfer-1", "target-1", workspace)
	cfg := &config.Config{ChordPath: binary, Workspaces: []config.Workspace{{ID: "test", Path: workspace}}}
	m := newTestChordManager(cfg)
	m.takeoverCLI = func(context.Context, string, takeoverRequest) (TransferStatus, error) {
		return TransferStatus{}, errors.New("coordinator not used by restore")
	}
	m.transfers = newSessionTransferStore(t.TempDir())
	m.pins = newSessionPinStore(t.TempDir())
	key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}).String()
	record := takeoverTestRecord("released")
	record.Direction = "incoming"
	record.Status.TargetKind = "gateway"
	record.Status.Workspace = cfg.Workspaces[0].Path
	if err := m.transfers.update(key, &record, ""); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	m.onEvent = func(_ string, event string, _ ControlState) {
		if event == "exit" {
			close(exited)
		}
	}
	t.Cleanup(func() {
		m.StopAll(time.Second)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Error("read loop did not exit")
		}
	})
	originalWriter := m.pins.writer
	m.pins.writer = func(string, []byte, os.FileMode) error { return errors.New("pin disk unavailable") }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if p, err := m.restoreTransferredSession(ctx, key, record); err == nil || p != nil {
		t.Fatalf("pin failure accepted p=%v error=%v", p, err)
	}
	existing := m.GetProcessForKey(key)
	if existing == nil || !existing.Alive() || !m.transferBlocked(key) {
		t.Fatal("pin failure lost paused target or reopened guard")
	}
	if err := existing.SendUserMessage("late instruction"); !errors.Is(err, ErrSessionTransferred) {
		t.Fatalf("pin failure admitted input: %v", err)
	}
	m.pins.writer = originalWriter
	p, err := m.restoreTransferredSession(ctx, key, record)
	if err != nil || p != existing || m.transferBlocked(key) || m.pins.Get(key) != "session-1" {
		t.Fatalf("retry spawned new target or failed to publish existing target: %v", err)
	}
}

func TestRestoredReadyDistinguishesZeroEpochFromMissingEpoch(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "zero"}[present], func(t *testing.T) {
			_, p, _ := newTakeoverTestProcess(t)
			p.restoringTransferID = "transfer-1"
			p.readySignal = make(chan struct{})
			payload := map[string]any{"session_id": "session-1", "transfer_id": "transfer-1", "binding_instance_id": p.bindingInstanceID, "runtime_id": "runtime-target", "workspace": "/workspace", "paused_inputs": PausedInputs{Paused: true}}
			if present {
				payload["session_epoch"] = uint64(0)
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			p.processEnvelope(&HeadlessEnvelope{Type: "ready_paused", Payload: raw})
			select {
			case <-p.readySignal:
				if !present {
					t.Fatal("missing epoch accepted")
				}
			default:
				if present {
					t.Fatal("explicit zero epoch rejected")
				}
			}
		})
	}
}
