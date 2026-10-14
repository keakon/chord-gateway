package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A busy process is mid-turn: /new and /resume must refuse before the
// route-change barrier and before any replacement, the same fast path the
// legacy console dispatch takes.
func TestIngressSessionChangeRejectsBusyProcessBeforeReplacing(t *testing.T) {
	cfg := testConfig()
	cfg.Workspaces[0].Path = t.TempDir()
	binary := filepath.Join(t.TempDir(), "chord")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"ready\",\"payload\":{\"session_id\":\"session\",\"runtime_id\":\"runtime\",\"session_epoch\":1}}'\nwhile IFS= read -r line; do :; done\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.ChordPath = binary
	mgr := newTestChordManager(cfg)
	mgr.pins = newSessionPinStore(t.TempDir())
	defer mgr.StopAll(time.Second)
	r := &NotificationRouter{mgr: mgr}
	key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}).String()
	p, _, result := r.startIngressCommandProcess(t.Context(), key, &IngressControlTarget{})
	if result.State != "" || p == nil {
		t.Fatal(p, result)
	}
	if got := mgr.pins.Get(pinKeyForBinding(key)); got != "session" {
		t.Fatalf("ready pin = %q, want the spawned session", got)
	}
	p.mu.Lock()
	p.state.Busy = true
	p.mu.Unlock()
	msg := IncomingMessage{IMType: "wechat", ChatID: "chat", ticket: &AdmissionTicket{Identity: ChatIdentity{"wechat", "bot", "chat"}}}
	got := r.dispatchIngressSession(t.Context(), msg, key, IMCommand{Type: "new"}, nil)
	if got.State != "rejected" || got.Reason != "session_change_busy" {
		t.Fatalf("result = %+v, want a busy refusal", got)
	}
	if mgr.GetProcessForKey(key) != p || mgr.pins.Get(pinKeyForBinding(key)) != "session" {
		t.Fatal("busy refusal replaced the process or dropped the pin")
	}
}

func TestSessionChangeRejectReasonDistinguishesRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"busy", errProcessBusy, "session_change_busy"},
		{"wrapped busy", fmt.Errorf("replace: %w", errProcessBusy), "session_change_busy"},
		{"transfer in progress", ErrSessionTransferred, "session_transferred"},
		{"unresolved inputs", errUnresolvedInputs, "undelivered_inputs"},
		{"input status unknown", errInputStatusUnknown, "undelivered_inputs"},
		{"shutting down", ErrManagerShuttingDown, "session_change_failed"},
		{"spawn failure", errors.New("exit status 42"), "session_change_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionChangeRejectReason(tc.err); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// The shared diagnostic renders its own text for a busy refusal, because the
// actionable step is cancelling the turn, not checking status.
func TestIngressDispatchDiagnosticExplainsBusySessionChange(t *testing.T) {
	r, code := pairedTestRouter(t)
	msg := IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "owner", MessageID: "bind", Text: fmt.Sprintf("/bind %q %s", t.TempDir(), code)}
	if !dispatchPairingTest(r, msg) {
		t.Fatal("bind rejected")
	}
	adapter := r.currentAdapter().(*pairingTestAdapter)
	msg.MessageID, msg.Text = "new", "/new"
	ticket, ok := r.admission.Authorize(msg)
	if !ok {
		t.Fatal("authorization")
	}
	msg.ticket = &ticket
	r.enqueueIngressDispatchDiagnostic(msg, DispatchResult{"rejected", "session_change_busy"})
	waitIngressCondition(t, func() bool {
		adapter.mu.Lock()
		defer adapter.mu.Unlock()
		for _, text := range adapter.messages {
			if strings.Contains(text, "busy") {
				return true
			}
		}
		return false
	})
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	last := adapter.messages[len(adapter.messages)-1]
	if !strings.Contains(last, "busy") || !strings.Contains(last, "/cancel") {
		t.Fatalf("busy diagnostic = %q", last)
	}
}
