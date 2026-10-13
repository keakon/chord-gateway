package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTransferProgressPersistsAndNotifiesWithoutClearingSource(t *testing.T) {
	m, p, w := newTakeoverTestProcess(t)
	router := NewNotificationRouter(m)
	t.Cleanup(router.Close)
	adapter := &stubIMAdapter{typ: "wechat"}
	router.SetAdapter(adapter)
	p.onEvent = router.HandleChordEvent
	if err := m.pins.Set(p.key, p.state.SessionID); err != nil {
		t.Fatal(err)
	}
	// An ordered source status may precede the receipt of an earlier send.
	if err := p.SendUserMessage("queued instruction"); err != nil {
		t.Fatal(err)
	}
	status := takeoverTestRecord("preparing").Status
	p.processEnvelope(takeoverEnvelope(t, "session_transfer_preparing", status))
	for _, state := range []string{"draining", "saving", "save_failed", "saving"} {
		status.State = state
		status.Blockers = []TransferBlocker{{OwnerTaskID: "task-1", Type: "tool", Description: "Waiting for active tool"}}
		if state == "save_failed" {
			status.ErrorCode = "persistence_failed"
			status.RecoverableAction = "retry-save"
		} else {
			status.ErrorCode = ""
			status.RecoverableAction = ""
		}
		p.processEnvelope(takeoverEnvelope(t, "session_transfer_status", status))
		record, ok, err := m.transfers.Get(p.key)
		if err != nil || !ok || record.Status.State != state || len(record.Status.Blockers) != 1 {
			t.Fatalf("progress not persisted: %#v, %v", record, err)
		}
		deadline := time.Now().Add(time.Second)
		for !strings.Contains(adapter.lastMessage().text, "State: "+state) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		text := adapter.lastMessage().text
		if !strings.Contains(text, "State: "+state) || !strings.Contains(text, "Waiting for active tool") {
			t.Fatalf("progress not delivered: %s", text)
		}
		if state == "save_failed" && !strings.Contains(text, "retry-save") {
			t.Fatalf("save recovery missing: %s", text)
		}
		if m.pins.Get(p.key) != "session-1" || !m.transferBlocked(p.key) || len(p.pendingInputs) != 1 || p.autoRestart {
			t.Fatal("progress changed source ownership or input receipts")
		}
	}
	if len(takeoverCommands(t, w)) != 2 {
		t.Fatal("progress sent a preparation ACK")
	}
	if err := p.SendUserMessage("later instruction"); !errors.Is(err, ErrSessionTransferred) {
		t.Fatalf("source admission reopened: %v", err)
	}
	if err := m.transfers.Load(); err != nil {
		t.Fatal(err)
	}
	record, _, _ := m.transfers.Get(p.key)
	if record.Status.State != "saving" {
		t.Fatal("progress did not survive reload")
	}
}

func TestTransferProgressRejectsInvalidStateAndIdentity(t *testing.T) {
	for _, change := range []string{"state", "runtime", "epoch", "transfer", "binding", "workspace", "target"} {
		t.Run(change, func(t *testing.T) {
			m, p, _ := newTakeoverTestProcess(t)
			status := takeoverTestRecord("preparing").Status
			p.processEnvelope(takeoverEnvelope(t, "session_transfer_preparing", status))
			status.State = "draining"
			switch change {
			case "state":
				status.State = "released"
			case "runtime":
				status.SourceRuntimeID = "other"
			case "epoch":
				status.SourceSessionEpoch++
			case "transfer":
				status.TransferID = "other"
			case "binding":
				status.BindingInstanceID = "other"
			case "workspace":
				status.Workspace = "/other"
			case "target":
				status.TargetInstanceID = "other"
			}
			p.processEnvelope(takeoverEnvelope(t, "session_transfer_status", status))
			record, _, _ := m.transfers.Get(p.key)
			if record.Status.State != "preparing" || p.transfer.State != "preparing" {
				t.Fatal("invalid progress changed source state")
			}
		})
	}
}

func TestTransferProgressCannotRegressSavedOrReleased(t *testing.T) {
	for _, state := range []string{"saved", "released"} {
		t.Run(state, func(t *testing.T) {
			m, p, _ := newTakeoverTestProcess(t)
			status := takeoverTestRecord("preparing").Status
			p.processEnvelope(takeoverEnvelope(t, "session_transfer_preparing", status))
			p.processEnvelope(takeoverEnvelope(t, "session_transfer_"+state, status))
			status.State = "draining"
			p.processEnvelope(takeoverEnvelope(t, "session_transfer_status", status))
			record, _, _ := m.transfers.Get(p.key)
			if record.Status.State != state || p.transfer.State != state {
				t.Fatal("late progress regressed terminal state")
			}
		})
	}
}
