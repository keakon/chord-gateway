package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
)

func TestParseTakeoverCommand(t *testing.T) {
	tests := []struct {
		text, action, session string
		invalid               bool
		timeout               *time.Duration
	}{
		{text: "/takeover session-1", action: "start", session: "session-1"},
		{text: "/takeover session-1 --timeout 0", action: "start", session: "session-1", timeout: new(time.Duration(0))},
		{text: "/takeover session-1 --timeout 2m", action: "start", session: "session-1", timeout: new(2 * time.Minute)},
		{text: "/takeover status", action: "status"},
		{text: "/takeover", invalid: true},
		{text: "/takeover session-1 --timeout -1s", invalid: true},
		{text: "/takeover session-1 --timeout invalid", invalid: true},
		{text: "/takeover session-1 --wait 1s", invalid: true},
		{text: "/takeover status --timeout 0", invalid: true},
		{text: "/takeover session-1 extra", invalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			got := parseIMCommand(tt.text)
			if got.Type != "takeover" || got.Invalid != tt.invalid {
				t.Fatalf("parse=%#v", got)
			}
			if tt.invalid {
				return
			}
			if got.Action != tt.action || got.SessionID != tt.session || (got.Timeout == nil) != (tt.timeout == nil) {
				t.Fatalf("parse=%#v", got)
			}
			if got.Timeout != nil && *got.Timeout != *tt.timeout {
				t.Fatalf("timeout=%v", *got.Timeout)
			}
		})
	}
	if got := parseIMCommand("/taskover session-1"); got.Type == "takeover" {
		t.Fatal("unapproved taskover alias recognized")
	}
}

func TestTransferGuardsRouterMessagesRemindersAndOldCardsAfterRestart(t *testing.T) {
	cfg := &config.Config{ChordPath: "/missing-test-binary", Workspaces: []config.Workspace{{ID: "test", Path: t.TempDir()}}}
	m := newTestChordManager(cfg)
	dir := t.TempDir()
	m.transfers = newSessionTransferStore(dir)
	key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}).String()
	record := takeoverTestRecord("released")
	if err := m.transfers.update(key, &record, ""); err != nil {
		t.Fatal(err)
	}
	m.transfers = newSessionTransferStore(dir)
	if err := m.transfers.Load(); err != nil {
		t.Fatal(err)
	}
	router := NewNotificationRouter(m)
	adapter := &stubIMAdapter{typ: "wechat"}
	router.SetAdapter(adapter)
	for _, message := range []IncomingMessage{
		{IMType: "wechat", ChatID: "chat", Text: "new request"},
		{IMType: "wechat", ChatID: "chat", Text: "!printf hello"},
		{IMType: "wechat", ChatID: "chat", Text: "/answer @old-question late answer"},
		{IMType: "wechat", ChatID: "chat", InternalAction: &InternalAction{Type: "confirm", Action: "allow", RequestID: "old-confirm", BindingInstanceID: "binding-old"}},
	} {
		router.HandleIncomingMessage(message)
		if m.GetProcessForKey(key) != nil {
			t.Fatal("blocked input or stale callback spawned replacement")
		}
	}
	if len(adapter.sentMessages()) != 4 {
		t.Fatal("blocked inputs did not receive guidance")
	}
	if _, err := m.GetOrSpawnForKey(key); !errors.Is(err, ErrSessionTransferred) {
		t.Fatalf("manager error=%v", err)
	}
	// A reminder already scheduled before the guard must be removed, including
	// the callback that has begun firing by the time preparation closes input.
	router.mu.Lock()
	router.resetReminderTimer(key, time.Hour)
	router.mu.Unlock()
	router.scheduleReminder(key, time.Now())
	router.fireReminder(key)
	router.mu.Lock()
	remaining := len(router.reminders)
	router.mu.Unlock()
	if remaining != 0 || len(adapter.sentMessages()) != 4 {
		t.Fatal("transferred binding emitted or retained reminder")
	}
}

func TestTakeoverObserverUnknownOutcomeRetainsOriginalTransfer(t *testing.T) {
	m := newTestChordManager(testConfig())
	m.transfers = newSessionTransferStore(t.TempDir())
	key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}).String()
	record := takeoverTestRecord("waiting_source")
	record.Direction = "incoming"
	record.Status.TargetKind = "gateway"
	if err := m.transfers.update(key, &record, ""); err != nil {
		t.Fatal(err)
	}
	calls := 0
	m.takeoverCLI = func(_ context.Context, _ string, req takeoverRequest) (TransferStatus, error) {
		calls++
		if req.Action != "start" || req.TransferID != record.Status.TransferID || !m.transferBlocked(key) {
			t.Fatalf("start without original durable guard: %#v", req)
		}
		return TransferStatus{}, errors.New("coordinator disconnected")
	}
	router := NewNotificationRouter(m)
	adapter := &stubIMAdapter{typ: "wechat"}
	router.SetAdapter(adapter)
	router.observeTakeover(&m.Config().Workspaces[0], "chat", key, true, nil)
	got, ok, err := m.transfers.Get(key)
	if err != nil || !ok || calls != 1 || got.Status.TransferID != record.Status.TransferID || !m.transferBlocked(key) {
		t.Fatalf("unknown start lost original guard %#v, %v", got, err)
	}
	if !strings.Contains(adapter.lastMessage().text, "do not resend start") {
		t.Fatal("unknown outcome lacked status-only retry guidance")
	}
}

func TestTakeoverZeroBudgetDoesNotCancelOrClearSource(t *testing.T) {
	m := newTestChordManager(testConfig())
	m.transfers = newSessionTransferStore(t.TempDir())
	key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}).String()
	record := takeoverTestRecord("waiting_source")
	record.Direction = "incoming"
	record.Status.TargetKind = "gateway"
	if err := m.transfers.update(key, &record, ""); err != nil {
		t.Fatal(err)
	}
	calls := 0
	m.takeoverCLI = func(_ context.Context, _ string, req takeoverRequest) (TransferStatus, error) {
		calls++
		if req.Action != "start" {
			t.Fatalf("zero wait executed %q", req.Action)
		}
		status := record.Status
		status.State = "draining"
		status.StartedAt = "2026-01-01T00:00:00Z"
		return status, nil
	}
	router := NewNotificationRouter(m)
	adapter := &stubIMAdapter{typ: "wechat"}
	router.SetAdapter(adapter)
	router.observeTakeover(&m.Config().Workspaces[0], "chat", key, true, new(time.Duration(0)))
	got, ok, err := m.transfers.Get(key)
	if err != nil || !ok || calls != 1 || got.Status.State != "draining" || got.Status.StartedAt == "" || got.Status.WaitExpired {
		t.Fatalf("wait budget changed source %#v %v", got, err)
	}
	if !strings.Contains(adapter.lastMessage().text, "Source work is not cancelled") {
		t.Fatal("zero budget lacked independent-observation guidance")
	}
}

func TestStaleCardCannotReachReplacementBinding(t *testing.T) {
	m, p, w := newTakeoverTestProcess(t)
	router := NewNotificationRouter(m)
	adapter := &stubIMAdapter{typ: "wechat"}
	router.SetAdapter(adapter)
	router.HandleIncomingMessage(IncomingMessage{IMType: "wechat", ChatID: "chat", InternalAction: &InternalAction{Type: "confirm", Action: "allow", RequestID: "confirm-1", BindingInstanceID: "binding-old"}})
	if w.String() != "" || m.GetProcessForKey(p.key) != p || !strings.Contains(adapter.lastMessage().text, "previous process") {
		t.Fatal("stale approval reached replacement runtime")
	}
}

func TestTakeoverTargetPreflightRejectsActiveOwnedOrFailedGuardWriter(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "guard writer failure", true: "active owned target"}[active], func(t *testing.T) {
			cfg := &config.Config{Workspaces: []config.Workspace{{ID: "test", Path: t.TempDir()}}}
			m := newTestChordManager(cfg)
			m.transfers = newSessionTransferStore(t.TempDir())
			key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}).String()
			calls := 0
			m.takeoverCLI = func(_ context.Context, _ string, req takeoverRequest) (TransferStatus, error) {
				calls++
				if req.Action != transferStatePreflight {
					t.Errorf("guard writer failure contacted source with %q", req.Action)
				}
				status := takeoverTestRecord(transferStatePreflight).Status
				status.TransferID, status.SessionID, status.Workspace = req.TransferID, req.SessionID, cfg.Workspaces[0].Path
				status.TargetKind, status.TargetInstanceID = "gateway", req.TargetInstanceID
				return status, nil
			}
			if active {
				m.procs[key] = &ChordProcess{stdin: &captureWriteCloser{}}
			} else {
				m.transfers.writer = func(string, []byte) error { return errors.New("disk unavailable") }
			}
			router := NewNotificationRouter(m)
			adapter := &stubIMAdapter{typ: "wechat"}
			router.SetAdapter(adapter)
			cmd := parseTakeoverCommand("session-1 --timeout 0")
			if active {
				router.handleTakeover(&cfg.Workspaces[0], "chat", key, cmd)
				if calls != 0 {
					t.Fatal("active binding contacted source")
				}
				requireContains(t, adapter.lastMessage().text, "target_not_ready")
			} else {
				router.prepareIncomingTakeover(&cfg.Workspaces[0], "chat", key, cmd)
				if calls != 1 || m.transferBlocked(key) {
					t.Fatalf("preflight calls=%d blocked=%v", calls, m.transferBlocked(key))
				}
				requireContains(t, adapter.lastMessage().text, "no transfer was started")
			}
		})
	}
}

func TestFailedExplicitSessionChangeKeepsTransferGuard(t *testing.T) {
	for _, command := range []string{"/new", "/resume session-2"} {
		t.Run(command, func(t *testing.T) {
			cfg := &config.Config{ChordPath: "/missing-test-binary", Workspaces: []config.Workspace{{ID: "test", Path: t.TempDir()}}}
			m := newTestChordManager(cfg)
			m.transfers = newSessionTransferStore(t.TempDir())
			m.pins = newSessionPinStore(t.TempDir())
			m.takeoverCLI = func(context.Context, string, takeoverRequest) (TransferStatus, error) {
				return TransferStatus{}, errors.New("coordinator not used by explicit session change")
			}
			key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}).String()
			record := takeoverTestRecord("released")
			if err := m.transfers.update(key, &record, ""); err != nil {
				t.Fatal(err)
			}
			if err := m.pins.Set(key, "session-1"); err != nil {
				t.Fatal(err)
			}
			router := NewNotificationRouter(m)
			adapter := &stubIMAdapter{typ: "wechat"}
			router.SetAdapter(adapter)
			router.HandleIncomingMessage(IncomingMessage{IMType: "wechat", ChatID: "chat", Text: command})
			if !m.transferBlocked(key) || m.pins.Get(key) != "session-1" || m.GetProcessForKey(key) != nil {
				t.Fatal("failed explicit change cleared guard or published target pin")
			}
			requireContains(t, adapter.lastMessage().text, "guard remains")
		})
	}
}

func TestIncomingRejectedTransferClearsOnlyConfirmedIdentity(t *testing.T) {
	for _, state := range []string{transferStatePreflight, transferStatePrepareFailed, "status_unknown"} {
		for _, changed := range []bool{false, true} {
			t.Run(state+map[bool]string{false: " matching", true: " changed"}[changed], func(t *testing.T) {
				m, p, _ := newTakeoverTestProcess(t)
				m.removeProcessIfCurrent(p.key, p)
				record := takeoverTestRecord("waiting_source")
				record.Direction, record.Status.TargetKind = "incoming", "gateway"
				if err := m.transfers.update(p.key, &record, ""); err != nil {
					t.Fatal(err)
				}
				m.takeoverCLI = func(context.Context, string, takeoverRequest) (TransferStatus, error) {
					status := record.Status
					status.State, status.ErrorCode = state, "target_not_ready"
					if changed {
						status.SourceRuntimeID = "other"
					}
					return status, errors.New("not accepted")
				}
				router := NewNotificationRouter(m)
				adapter := &stubIMAdapter{typ: "wechat"}
				router.SetAdapter(adapter)
				router.observeTakeover(&m.Config().Workspaces[0], "chat", p.key, true, nil)
				wantBlocked := changed || state == "status_unknown"
				if got := m.transferBlocked(p.key); got != wantBlocked {
					t.Fatalf("blocked=%v want %v", got, wantBlocked)
				}
			})
		}
	}
}

func TestIncomingPreflightRechecksBindingBeforeStartingSource(t *testing.T) {
	m := newTestChordManager(testConfig())
	m.transfers = newSessionTransferStore(t.TempDir())
	key := processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}.String()
	calls := 0
	m.takeoverCLI = func(_ context.Context, _ string, req takeoverRequest) (TransferStatus, error) {
		calls++
		if req.Action != transferStatePreflight {
			t.Fatalf("started source after binding changed: %#v", req)
		}
		// An ordinary input won the binding while the read-only CLI was running.
		m.procs[key] = &ChordProcess{stdin: &captureWriteCloser{}}
		status := takeoverTestRecord(transferStatePreflight).Status
		status.TransferID, status.SessionID, status.TargetKind, status.TargetInstanceID = req.TransferID, req.SessionID, "gateway", req.TargetInstanceID
		return status, nil
	}
	router := NewNotificationRouter(m)
	adapter := &stubIMAdapter{typ: "wechat"}
	router.SetAdapter(adapter)
	router.prepareIncomingTakeover(&m.Config().Workspaces[0], "chat", key, parseTakeoverCommand("session-1 --timeout 0"))
	if calls != 1 || m.transferBlocked(key) {
		t.Fatalf("calls=%d blocked=%v", calls, m.transferBlocked(key))
	}
	requireContains(t, adapter.lastMessage().text, "target_not_ready")
}

type blockedTakeoverAdapter struct {
	*stubIMAdapter
	entered chan struct{}
	release chan struct{}
	sent    chan struct{}
	once    sync.Once
}

func (a *blockedTakeoverAdapter) SendText(string, string) error {
	a.once.Do(func() { close(a.entered) })
	<-a.release
	a.sent <- struct{}{}
	return nil
}
func TestIncomingTakeoverDoesNotHoldLifecycleLocksDuringIMSend(t *testing.T) {
	m := newTestChordManager(testConfig())
	m.transfers = newSessionTransferStore(t.TempDir())
	key := processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}.String()
	started := make(chan struct{})
	m.takeoverCLI = func(_ context.Context, _ string, req takeoverRequest) (TransferStatus, error) {
		status := takeoverTestRecord(req.Action).Status
		status.TransferID, status.SessionID, status.TargetKind, status.TargetInstanceID = req.TransferID, req.SessionID, "gateway", req.TargetInstanceID
		if req.Action == "start" {
			close(started)
			status.State = "draining"
		}
		return status, nil
	}
	router := NewNotificationRouter(m)
	adapter := &blockedTakeoverAdapter{stubIMAdapter: &stubIMAdapter{typ: "wechat"}, entered: make(chan struct{}), release: make(chan struct{}), sent: make(chan struct{}, 2)}
	router.SetAdapter(adapter)
	finished := make(chan struct{})
	go func() {
		router.prepareIncomingTakeover(&m.Config().Workspaces[0], "chat", key, parseTakeoverCommand("session-1 --timeout 0"))
		close(finished)
	}()
	defer func() {
		close(adapter.release)
		for range 2 {
			select {
			case <-adapter.sent:
			case <-time.After(5 * time.Second):
				t.Error("takeover notification did not settle after IM release")
			}
		}
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("preparation did not finish after IM release")
		}
	}()
	select {
	case <-adapter.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("IM send did not begin")
	}
	if !m.transferBlocked(key) {
		t.Fatal("IM notification preceded durable guard")
	}
	available := make(chan struct{})
	go func() {
		m.lifecycleGate.Lock()
		release := m.acquireKeyLock(key)
		release()
		m.lifecycleGate.Unlock()
		close(available)
	}()
	select {
	case <-available:
	case <-time.After(5 * time.Second):
		t.Fatal("IM send held binding or lifecycle lock")
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("IM send delayed source start")
	}
	select {
	case <-finished:
		t.Fatal("fixture IM send unexpectedly completed")
	default:
	}
}
