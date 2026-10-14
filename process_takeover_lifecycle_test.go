package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
)

func newTakeoverLifecycleManager(t *testing.T, ready bool) (*ChordManager, string, sessionTransfer, <-chan string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake headless uses a POSIX shell")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-headless")
	payload := map[string]any{"workspace": dir, "session_id": "session-2", "runtime_id": "target-runtime", "session_epoch": 1, "binding_instance_id": "target-1", "transfer_id": "transfer-1", "paused_inputs": map[string]any{"paused": true, "pending_input_count": 0, "inputs": []any{}}}
	event := "ready_paused"
	if !ready {
		event = "error"
		payload = map[string]any{"message": "target booting"}
	}
	envelope, err := json.Marshal(map[string]any{"type": event, "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' " + shellQuote(string(envelope)) + "\nwhile IFS= read -r line; do :; done\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ChordPath: binary, Workspaces: []config.Workspace{{ID: "test", Path: dir}}}
	m := newTestChordManager(cfg)
	m.shutdownCh = make(chan struct{})
	m.takeoverCLI = func(context.Context, string, takeoverRequest) (TransferStatus, error) { return TransferStatus{}, nil }
	m.transfers = newSessionTransferStore(t.TempDir())
	m.pins = newSessionPinStore(t.TempDir())
	key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}).String()
	record := takeoverTestRecord("released")
	record.Status.SessionID = "session-2"
	record.Status.Workspace = dir
	if err := m.transfers.update(key, &record, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.pins.Set(key, "session-1"); err != nil {
		t.Fatal(err)
	}
	events := make(chan string, 8)
	m.onEvent = func(_ string, event string, _ ControlState) { events <- event }
	t.Cleanup(func() { m.StopAll(time.Second) })
	return m, key, record, events
}

func waitTakeoverLifecycleEvent(t *testing.T, events <-chan string, expected string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event == expected {
				return
			}
		case <-timer.C:
			t.Fatalf("target did not emit %s", expected)
		}
	}
}

func TestExplicitReplacementPinFailureAllowsRetryWithoutRemovingGuard(t *testing.T) {
	for _, session := range []string{"", "session-2"} {
		t.Run("session="+session, func(t *testing.T) {
			m, key, _, events := newTakeoverLifecycleManager(t, true)
			writer := m.pins.writer
			m.pins.writer = func(string, []byte, os.FileMode) error { return errors.New("pin storage unavailable") }
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := m.replaceBlockedBinding(ctx, key, session); err == nil {
				t.Fatal("replacement ignored pin write failure")
			}
			waitTakeoverLifecycleEvent(t, events, "exit")
			if !m.transferBlocked(key) || m.GetProcessForKey(key) != nil || m.pins.Get(key) != "session-1" {
				t.Fatal("failed replacement changed pin, cleared guard, or retained its target")
			}
			m.pins.writer = writer
			if err := m.replaceBlockedBinding(ctx, key, session); err != nil {
				t.Fatalf("explicit retry failed: %v", err)
			}
			if m.transferBlocked(key) || m.pins.Get(key) != "session-2" {
				t.Fatal("successful replacement did not publish target")
			}
			if err := m.GetProcessForKey(key).SendUserMessage("Continue the task"); err != nil {
				t.Fatalf("explicit replacement retained its stdin guard: %v", err)
			}
		})
	}
}

func TestExplicitReplacementEOFDoesNotPublishSessionSwitchAsReadiness(t *testing.T) {
	m, key, _, events := newTakeoverLifecycleManager(t, true)
	// A cached session identity is insufficient: only a live ready target may
	// clear the guard. EOF closes the same wait signal as a ready envelope.
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"session_switched\",\"payload\":{\"session_id\":\"session-2\",\"runtime_id\":\"target-runtime\",\"session_epoch\":1}}'\nexit 1\n"
	if err := os.WriteFile(m.Config().ChordBinary(), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.replaceBlockedBinding(ctx, key, "session-2"); err == nil {
		t.Fatal("EOF published cached session identity as a ready target")
	}
	waitTakeoverLifecycleEvent(t, events, "exit")
	if !m.transferBlocked(key) || m.GetProcessForKey(key) != nil || m.pins.Get(key) != "session-1" {
		t.Fatal("failed EOF target reopened binding or changed pin")
	}
}

func TestGuardedBindingCannotPublishReadinessAfterDisconnect(t *testing.T) {
	for _, failure := range []string{"stdout closed", "process exited", "gateway stopped"} {
		t.Run(failure, func(t *testing.T) {
			m, p, _ := newTakeoverTestProcess(t)
			record := takeoverTestRecord("restoring")
			record.Direction = "incoming"
			record.Status.TargetKind = "gateway"
			p.bindingGuard = true
			p.readyPaused = true
			p.restoringTransferID = record.Status.TransferID
			p.bindingInstanceID = record.Status.TargetInstanceID
			p.autoRestart = false
			if err := m.transfers.update(p.key, &record, ""); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "stdout closed":
				p.stdin = nil
			case "process exited":
				p.exited = true
			case "gateway stopped":
				p.stoppedByGateway = true
			}
			if err := m.publishGuardedBinding(p, record, record.Status.SessionID, true); err == nil {
				t.Fatal("published stale ready_paused after target disconnection")
			}
			if !m.transferBlocked(p.key) || m.pins.Get(p.key) != "" || !p.bindingGuard || p.autoRestart {
				t.Fatal("disconnected target reopened binding")
			}
		})
	}
}

func TestExplicitReplacementTimeoutAllowsExplicitRetry(t *testing.T) {
	m, key, _, events := newTakeoverLifecycleManager(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- m.replaceBlockedBinding(ctx, key, "session-2") }()
	// The deadline may expire before stdout delivers the booting event. Assert
	// the timeout and cleanup, not scheduler-dependent progress before timeout.
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expired readiness wait returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out replacement did not finish")
	}
	waitTakeoverLifecycleEvent(t, events, "exit")
	if !m.transferBlocked(key) || m.GetProcessForKey(key) != nil || m.pins.Get(key) != "session-1" {
		t.Fatal("timed out replacement cleared guard or retained its own target")
	}
	// A later explicit attempt starts a new target; ordinary reconnect remains
	// blocked while the earlier readiness result is unknown.
	if _, err := m.GetOrSpawnForKey(key); !errors.Is(err, ErrSessionTransferred) {
		t.Fatalf("ordinary reconnect bypassed guard: %v", err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"ready\",\"payload\":{\"session_id\":\"session-2\"}}'\nwhile IFS= read -r line; do :; done\n"
	if err := os.WriteFile(m.Config().ChordBinary(), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	retryCtx, retryCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer retryCancel()
	if err := m.replaceBlockedBinding(retryCtx, key, "session-2"); err != nil {
		t.Fatalf("explicit retry after expired wait failed: %v", err)
	}
	if m.transferBlocked(key) || m.pins.Get(key) != "session-2" {
		t.Fatal("explicit retry failed to publish replacement")
	}
}

func TestShutdownInterruptsGuardedTargetReadinessWait(t *testing.T) {
	for _, restoring := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit replacement", true: "transfer restore"}[restoring], func(t *testing.T) {
			m, key, record, events := newTakeoverLifecycleManager(t, false)
			if restoring {
				record.Direction = "incoming"
				record.Status.TargetKind = "gateway"
				if err := m.transfers.update(key, &record, record.Status.TransferID); err != nil {
					t.Fatal(err)
				}
			}
			result := make(chan error, 1)
			go func() {
				if restoring {
					_, err := m.restoreTransferredSession(context.Background(), key, record)
					result <- err
				} else {
					result <- m.replaceBlockedBinding(context.Background(), key, "session-2")
				}
			}()
			waitTakeoverLifecycleEvent(t, events, "error")
			stopped := make(chan struct{})
			go func() { m.StopAll(time.Second); close(stopped) }()
			select {
			case err := <-result:
				if !errors.Is(err, ErrManagerShuttingDown) {
					t.Fatalf("readiness wait returned %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown did not cancel target readiness wait")
			}
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("target readiness wait held gateway shutdown")
			}
			waitTakeoverLifecycleEvent(t, events, "exit")
			if !m.transferBlocked(key) || m.pins.Get(key) != "session-1" || m.GetProcessForKey(key) != nil {
				t.Fatal("shutdown published an unconfirmed target")
			}
			if restoring {
				guard, ok, err := m.transfers.Get(key)
				if err != nil || !ok || !guard.Interrupted || guard.Status.State != "restoring" {
					t.Fatalf("shutdown lost independent target interruption: %#v, %v", guard, err)
				}
			}
		})
	}
}
