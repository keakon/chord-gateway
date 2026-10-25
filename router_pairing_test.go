package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

type pairingTestAdapter struct {
	mu       sync.Mutex
	messages []string
}

func (a *pairingTestAdapter) Connect() error              { return nil }
func (a *pairingTestAdapter) Disconnect()                 {}
func (a *pairingTestAdapter) Type() string                { return "wechat" }
func (a *pairingTestAdapter) SupportsLoginRenewal() bool  { return false }
func (a *pairingTestAdapter) StartLogin() (string, error) { return "", im.ErrLoginNotSupported }
func (a *pairingTestAdapter) SendText(chat, text string) error {
	return a.SendTextContext(context.Background(), chat, text)
}
func (a *pairingTestAdapter) SendTextContext(_ context.Context, _, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = append(a.messages, text)
	return nil
}

func pairedTestRouter(t *testing.T) (*NotificationRouter, string) {
	t.Helper()
	return pairedTestRouterWithConfig(t, "ims: {wechat: {}}\n")
}

func pairedTestRouterWithConfig(t *testing.T, data string) (*NotificationRouter, string) {
	t.Helper()
	store := testConfigStore(t, data)
	paths := &config.Paths{StateDir: t.TempDir()}
	mgr := NewChordManager(store.Snapshot(), paths)
	mgr.store = store
	r := NewNotificationRouter(mgr)
	var terminal bytes.Buffer
	p := NewPairingService(store, true, &terminal)
	r.admission = NewAdmission(p)
	mgr.admission = r.admission
	r.SetAdapter(&pairingTestAdapter{})
	t.Cleanup(func() { r.admission.Close(); r.Close() })
	if err := p.PlatformReady("wechat", "bot"); err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(terminal.String())
	for i, field := range fields {
		if field == "\"<directory>\"" {
			return r, fields[i+1]
		}
	}
	t.Fatal("pairing instructions missing")
	return nil, ""
}
func dispatchPairingTest(r *NotificationRouter, msg im.IncomingMessage) bool {
	ticket, ok := r.admission.Authorize(msg)
	if !ok {
		return false
	}
	msg.SetAdmissionTicket(&ticket)
	r.HandleIncomingMessage(msg)
	return true
}

func TestPairingBindAndReset(t *testing.T) {
	r, code := pairedTestRouter(t)
	dir := t.TempDir()
	msg := im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "owner", MessageID: "first", Text: fmt.Sprintf("/bind %q %s", dir, code)}
	if !dispatchPairingTest(r, msg) {
		t.Fatal("pairing rejected")
	}
	cfg := r.getConfig()
	owner := cfg.IMConfigByType("wechat").OwnerIdentity()
	if owner == nil || owner.UserID != "owner" {
		t.Fatal("owner not published")
	}
	ws, err := cfg.ResolveWorkspace("wechat", "chat")
	canonical, _, _ := canonicalBindingDirectory(dir)
	if err != nil || ws.Path != canonical {
		t.Fatal("wrong binding")
	}
	if len(r.mgr.procs) != 0 {
		t.Fatal("bind spawned process")
	}
	before, err := os.ReadFile(r.mgr.store.path)
	if err != nil {
		t.Fatal(err)
	}
	msg.MessageID = "repeat"
	if !dispatchPairingTest(r, msg) {
		t.Fatal("owner repeat rejected")
	}
	after, _ := os.ReadFile(r.mgr.store.path)
	if !bytes.Equal(before, after) {
		t.Fatal("idempotent bind rewrote configuration")
	}
	msg.SenderID = "stranger"
	msg.MessageID = "unauthorized"
	msg.Text = "!touch /tmp/should-not-run"
	if dispatchPairingTest(r, msg) {
		t.Fatal("stranger admitted")
	}
	msg.SenderID = "owner"
	msg.ChatID = "new-chat"
	msg.MessageID = "unbound"
	msg.Text = "task"
	if !dispatchPairingTest(r, msg) || len(r.mgr.procs) != 0 {
		t.Fatal("unbound input executed")
	}
	msg.MessageID = "new-bind"
	msg.Text = fmt.Sprintf("/bind %q", dir)
	dispatchPairingTest(r, msg)
	if len(r.getConfig().Workspaces) != 1 {
		t.Fatal("directory not reused")
	}
	key := (processKey{workspaceID: ws.ID, imType: "wechat", chatID: "chat"}).String()
	if scoped := pinKeyForBinding(key); scoped != key {
		t.Fatalf("pin key = %q, want the bare binding key %q", scoped, key)
	}
	if err := r.mgr.pins.Set(key, "session"); err != nil {
		t.Fatal(err)
	}
	paths := &config.Paths{StateDir: filepath.Dir(r.mgr.pins.path)}
	if err := resetPairing(context.Background(), r.mgr.store, paths, "wechat"); err != nil {
		t.Fatal(err)
	}
	if r.getConfig().IMConfigByType("wechat").OwnerIdentity() != nil {
		t.Fatal("reset retained owner")
	}
}

func TestPairingConcurrentClaimHasSingleOwner(t *testing.T) {
	r, code := pairedTestRouter(t)
	dir := t.TempDir()
	var wg sync.WaitGroup
	for _, sender := range []string{"first", "second"} {
		wg.Go(func() {
			dispatchPairingTest(r, im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: sender, SenderID: sender, MessageID: sender, Text: fmt.Sprintf("/bind %q %s", dir, code)})
		})
	}
	wg.Wait()
	imCfg := r.getConfig().IMConfigByType("wechat")
	if imCfg.OwnerIdentity() == nil || len(imCfg.Bindings()) != 1 {
		t.Fatal("claim did not select exactly one owner")
	}
}

func TestPairedRebindInvalidatesOldTicket(t *testing.T) {
	r, code := pairedTestRouter(t)
	first, second := t.TempDir(), t.TempDir()
	msg := im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "owner", MessageID: "first", Text: fmt.Sprintf("/bind %q %s", first, code)}
	dispatchPairingTest(r, msg)
	msg.MessageID = "old-task"
	msg.Text = "task"
	old, ok := r.admission.Authorize(msg)
	if !ok {
		t.Fatal("owner task rejected")
	}
	oldLease, _, _ := old.Gate.Snapshot()
	msg.MessageID = "switch"
	msg.Text = fmt.Sprintf("/bind %q", second)
	dispatchPairingTest(r, msg)
	if r.admission.Valid(old) || oldLease.Context().Err() == nil {
		t.Fatal("old binding remained authorized")
	}
	msg.MessageID = "return"
	msg.Text = fmt.Sprintf("/bind %q", first)
	dispatchPairingTest(r, msg)
	if r.admission.Valid(old) {
		t.Fatal("returning to directory reauthorized old input")
	}
}

// A pin belongs to the workspace and route, not the platform account: the same
// chat under a re-paired account must continue its conversation instead of
// silently starting over.
func TestPairedPinSurvivesAccountChange(t *testing.T) {
	r, code := pairedTestRouter(t)
	dir := t.TempDir()
	msg := im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "owner", MessageID: "first", Text: fmt.Sprintf("/bind %q %s", dir, code)}
	dispatchPairingTest(r, msg)
	ws, err := r.getConfig().ResolveWorkspace("wechat", "chat")
	if err != nil {
		t.Fatal(err)
	}
	key := (processKey{workspaceID: ws.ID, imType: "wechat", chatID: "chat"}).String()
	if err = r.mgr.pins.Set(key, "old-session"); err != nil {
		t.Fatal(err)
	}
	if got := r.mgr.spawnArgsForKey(key); len(got) != 2 || got[0] != "--resume" || got[1] != "old-session" {
		t.Fatalf("spawn args = %v, want the pinned session to be resumed", got)
	}
}

func TestPairedInputDuringBindingBarrierIsNotQueued(t *testing.T) {
	r, code := pairedTestRouter(t)
	dir := t.TempDir()
	msg := im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "owner", MessageID: "first", Text: fmt.Sprintf("/bind %q %s", dir, code)}
	dispatchPairingTest(r, msg)
	msg.MessageID = "task"
	msg.Text = "task"
	ticket, ok := r.admission.Authorize(msg)
	if !ok {
		t.Fatal("owner rejected")
	}
	ticket.Gate.Switching()
	if _, ok = r.admission.Authorize(msg); ok {
		t.Fatal("binding barrier admitted a task")
	}
}

func TestCancelledBindingCannotWriteHeadlessInput(t *testing.T) {
	lease := im.NewBindingLease(im.ChatIdentity{Platform: "wechat", AccountID: "bot", ChatID: "chat"}, "workspace", 1, 1)
	writer := &captureWriteCloser{}
	p := &ChordProcess{lease: lease, stdin: writer}
	lease.Close()
	if err := p.SendCommand(map[string]any{"type": "send", "content": "task"}); err == nil {
		t.Fatal("cancelled binding wrote a command")
	}
	if writer.String() != "" {
		t.Fatal("stale input reached headless")
	}
}

func TestIMIngressRequiresAdmission(t *testing.T) {
	r := NewNotificationRouter(newTestChordManager(testConfig()))
	defer r.Close()
	adapter := &pairingTestAdapter{}
	r.SetAdapter(adapter)
	r.HandleIncomingMessage(im.IncomingMessage{IMType: "wechat", ChatID: "chat", SenderID: "owner", Text: "/status"})
	if len(adapter.messages) != 0 || len(r.mgr.procs) != 0 {
		t.Fatal("IM input bypassed admission")
	}
}
