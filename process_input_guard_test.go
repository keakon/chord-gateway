package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
)

func TestIncomingTakeoverPreservesDisconnectedInputEvidence(t *testing.T) {
	for _, phase := range []string{"admission", "preflight", "restore", "explicit replacement"} {
		for _, kind := range []string{"unknown", "receipt", "queue", "storage", "paused", "restored"} {
			t.Run(phase+"/"+kind, func(t *testing.T) {
				r, code := pairedTestRouter(t)
				_, key := bindTestOwner(t, r, code, t.TempDir())
				wsID, _, _ := parseProcessKey(key)
				ws := r.getConfig().WorkspaceByID(wsID)
				p := &ChordProcess{key: key, exited: true, state: ControlState{SessionID: "original"}}
				switch kind {
				case "unknown":
					p.state.UnconfirmedInputs = 1
				case "receipt":
					p.pendingInputs = map[string]struct{}{"request": {}}
				case "queue":
					p.state.InputQueue = &InputQueue{Inputs: []InputQueueItem{{InputID: "input"}}}
				case "storage":
					p.state.InputQueue = &InputQueue{Blocked: "storage unavailable"}
				case "paused":
					p.state.PausedInputs = testPausedInputs()
				case "restored":
					p.state.InputQueue = &InputQueue{RestoredNextInputsBlocked: true}
				}
				if phase != "preflight" {
					r.mgr.procs[key] = p
				}
				calls := 0
				r.mgr.takeoverCLI = func(_ context.Context, _ string, req takeoverRequest) (TransferStatus, error) {
					calls++
					if phase != "preflight" || req.Action != transferStatePreflight {
						t.Error("guarded target contacted the source")
						return TransferStatus{}, errors.New("unexpected source call")
					}
					// A process can disconnect with unresolved input while preflight runs.
					r.mgr.mu.Lock()
					r.mgr.procs[key] = p
					r.mgr.mu.Unlock()
					return TransferStatus{State: transferStatePreflight, TransferID: req.TransferID, SessionID: req.SessionID, SourceRuntimeID: "source", Workspace: ws.Path, TargetKind: "gateway", TargetInstanceID: req.TargetInstanceID}, nil
				}
				if err := r.mgr.pins.Set(pinKeyForBinding(key), "original"); err != nil {
					t.Fatal(err)
				}
				cmd := parseTakeoverCommand("replacement --timeout 0")
				switch phase {
				case "admission":
					r.handleTakeover(ws, "chat", key, cmd)
				case "preflight":
					r.prepareIncomingTakeover(ws, "chat", key, cmd, testChatOperation(t, r.mgr, key))
				case "restore", "explicit replacement":
					record := takeoverTestRecord("released")
					record.Direction = "incoming"
					if err := r.mgr.transfers.update(key, &record, ""); err != nil {
						t.Fatal(err)
					}
					var err error
					if phase == "restore" {
						_, err = r.mgr.restoreTransferredSession(t.Context(), key, record)
					} else {
						err = r.mgr.replaceBlockedBinding(t.Context(), key, "replacement")
					}
					if !errors.Is(err, errUnresolvedInputs) {
						t.Fatalf("replacement: %v", err)
					}
				}
				if calls > 1 || phase != "preflight" && calls != 0 {
					t.Fatalf("source calls: %d", calls)
				}
				if r.mgr.GetProcessForKey(key) != p || p.stoppedByGateway || r.mgr.pins.Get(pinKeyForBinding(key)) != "original" {
					t.Fatal("lost original process or pin")
				}
				if phase != "restore" && phase != "explicit replacement" && r.mgr.transferBlocked(key) {
					t.Fatal("rejected takeover installed a guard")
				}
			})
		}
	}
}

// Respond through the real envelope merger so stale/missing projections and
// receipt uncertainty exercise the same state transitions as a live child.
func pendingRefreshProcess(t *testing.T, inputs *PausedInputs, stale bool) (*ChordProcess, <-chan string) {
	t.Helper()
	reader, writer := io.Pipe()
	p := &ChordProcess{stdin: writer, state: ControlState{SessionID: "session", PausedInputs: testPausedInputs()}, lastActivity: time.Now().Add(-time.Hour)}
	if stale {
		p.state.LastEnvelopeSeq = 20
	}
	commands := make(chan string, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		decoder := json.NewDecoder(reader)
		for {
			var cmd map[string]any
			if decoder.Decode(&cmd) != nil {
				return
			}
			kind, _ := cmd["type"].(string)
			commands <- kind
			if kind == "status" {
				raw, _ := json.Marshal(StatusResponse{SessionID: "session", PausedInputs: inputs})
				p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 10, Payload: raw})
			}
		}
	}()
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close(); <-done })
	return p, commands
}

func TestNewRefreshesAllOwnerInputsWithoutDroppingOtherEvidence(t *testing.T) {
	for _, kind := range []string{"empty", "worker", "missing", "stale", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			inputs := &PausedInputs{}
			if kind == "worker" {
				inputs = testPausedInputs()
			}
			if kind == "missing" {
				inputs = nil
			}
			p, commands := pendingRefreshProcess(t, inputs, kind == "stale")
			if kind == "unknown" {
				p.state.UnconfirmedInputs = 1
			}
			err := p.SendCommand(map[string]any{"type": "send", "content": "/new"})
			if got := <-commands; got != "status" {
				t.Fatalf("first command: %s", got)
			}
			if kind == "empty" {
				if err != nil {
					t.Fatal(err)
				}
				if got := <-commands; got != "send" {
					t.Fatalf("next command: %s", got)
				}
			} else if !errors.Is(err, errUnresolvedInputs) {
				t.Fatalf("guard: %v", err)
			}
		})
	}
}

func TestSessionReplacementRefreshesBeforeChangingPin(t *testing.T) {
	for _, remaining := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "worker"}[remaining], func(t *testing.T) {
			inputs := &PausedInputs{}
			if remaining {
				inputs = testPausedInputs()
			}
			p, commands := pendingRefreshProcess(t, inputs, false)
			dir := t.TempDir()
			m := newTestChordManager(&config.Config{ChordPath: filepath.Join(dir, "missing-binary"), Workspaces: []config.Workspace{{ID: "test", Path: dir}}})
			m.pins = newSessionPinStore(t.TempDir())
			key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}).String()
			p.key = key
			m.procs[key] = p
			if err := m.pins.Set(key, "session"); err != nil {
				t.Fatal(err)
			}
			_, err := m.replaceOrdinarySession(key, "replacement")
			if got := <-commands; got != "status" {
				t.Fatalf("first command: %s", got)
			}
			if remaining {
				if !errors.Is(err, errUnresolvedInputs) || m.pins.Get(key) != "session" || m.GetProcessForKey(key) != p {
					t.Fatal("pending worker input was discarded")
				}
			} else if err == nil || errors.Is(err, errUnresolvedInputs) || m.pins.Get(key) != "replacement" {
				t.Fatalf("empty summary did not reach attempted replacement: %v", err)
			}
		})
	}
}

func TestIdleRetirementRefreshesRestoredInputSummary(t *testing.T) {
	for _, remaining := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "worker"}[remaining], func(t *testing.T) {
			inputs := &PausedInputs{}
			if remaining {
				inputs = testPausedInputs()
			}
			p, commands := pendingRefreshProcess(t, inputs, false)
			m := newTestChordManager(testConfig())
			p.key = "sample"
			m.procs[p.key] = p
			retired := m.collectIdleProcesses(0)
			if got := <-commands; got != "status" {
				t.Fatalf("first command: %s", got)
			}
			if (len(retired) == 0) != remaining {
				t.Fatalf("retired %d, remaining=%v", len(retired), remaining)
			}
		})
	}
}

func TestInputRefreshFailureKeepsOriginalSession(t *testing.T) {
	p := &ChordProcess{state: ControlState{SessionID: "session", PausedInputs: testPausedInputs()}}
	if err := p.SendCommand(map[string]any{"type": "send", "content": "/new"}); !errors.Is(err, errInputStatusUnknown) {
		t.Fatalf("closed transport: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := p.refreshPendingInputStatus(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled refresh: %v", err)
	}
	if p.state.SessionID != "session" || !p.hasGuardedInputsLocked() {
		t.Fatal("refresh failure cleared evidence")
	}
}
