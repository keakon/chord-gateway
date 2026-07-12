package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
)

func TestTailBufferKeepsOnlyTail(t *testing.T) {
	buf := newTailBuffer(5)
	if n, err := buf.Write([]byte("abc")); err != nil || n != 3 {
		t.Fatalf("first write n=%d err=%v", n, err)
	}
	if n, err := buf.Write([]byte("defgh")); err != nil || n != 5 {
		t.Fatalf("second write n=%d err=%v", n, err)
	}
	if got := buf.String(); got != "defgh" {
		t.Fatalf("buffer tail = %q, want defgh", got)
	}

	if n, err := buf.Write([]byte("1234567")); err != nil || n != 7 {
		t.Fatalf("oversize write n=%d err=%v", n, err)
	}
	if got := buf.String(); got != "34567" {
		t.Fatalf("oversize tail = %q, want 34567", got)
	}
}

func TestTruncateStderrTail(t *testing.T) {
	if got := truncateStderrTail("abcdef", 3); got != "def" {
		t.Fatalf("truncateStderrTail = %q, want def", got)
	}
	if got := truncateStderrTail("short", 10); got != "short" {
		t.Fatalf("truncateStderrTail short = %q", got)
	}
	if got := truncateStderrTail(strings.Repeat("x", 2100), 0); len(got) != 2000 {
		t.Fatalf("default truncate len = %d, want 2000", len(got))
	}
}

func TestProcessEnvelopeReadyPersistsPinOutsideProcessLock(t *testing.T) {
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	pins := &sessionPinStore{
		pins: make(map[string]string),
		writer: func(string, []byte, os.FileMode) error {
			close(writeStarted)
			<-releaseWrite
			return errors.New("write failed")
		},
	}
	mgr := &ChordManager{pins: pins}
	p := &ChordProcess{key: "ws|wechat|chat", mgr: mgr}
	payload, err := json.Marshal(map[string]string{"session_id": "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		p.processEnvelope(&HeadlessEnvelope{Type: "ready", Payload: payload})
		close(done)
	}()

	<-writeStarted
	stateRead := make(chan struct{})
	go func() {
		_ = p.State()
		close(stateRead)
	}()
	select {
	case <-stateRead:
	case <-time.After(time.Second):
		t.Fatal("State blocked while session pin was being persisted")
	}
	close(releaseWrite)
	<-done
}

func BenchmarkProcessEnvelope(b *testing.B) {
	cases := []struct {
		name string
		env  HeadlessEnvelope
	}{
		{name: "subscribe-response", env: HeadlessEnvelope{Type: "subscribe_response"}},
		{name: "activity", env: HeadlessEnvelope{Type: "activity", Payload: json.RawMessage(`{"type":"tool","detail":"running"}`)}},
		{name: "assistant-message", env: HeadlessEnvelope{Type: "assistant_message", Payload: json.RawMessage(`{"text":"completed","agent_id":"agent","tool_calls":2}`)}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			p := &ChordProcess{key: "ws|wechat|chat"}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p.processEnvelope(&tc.env)
			}
		})
	}
}

func TestProcessEnvelopeUsesConsistentEventTimestamp(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat"}
	p.processEnvelope(&HeadlessEnvelope{
		Type:    "assistant_message",
		Payload: json.RawMessage(`{"text":"completed","agent_id":"agent","tool_calls":2}`),
	})
	state := p.State()
	if !state.LastPushAt.Equal(p.lastActivity) {
		t.Fatalf("LastPushAt = %v, lastActivity = %v", state.LastPushAt, p.lastActivity)
	}
	updatedAt, err := time.Parse(time.RFC3339, state.UpdatedAt)
	if err != nil {
		t.Fatalf("parse UpdatedAt: %v", err)
	}
	if updatedAt.Unix() != p.lastActivity.Unix() {
		t.Fatalf("UpdatedAt = %v, lastActivity = %v", updatedAt, p.lastActivity)
	}
}

func TestChordManagerProcessLookupAndStop(t *testing.T) {
	cfg := &config.Config{Workspaces: []config.Workspace{{ID: "ws1", Path: t.TempDir()}, {ID: "ws2", Path: t.TempDir()}}}
	mgr := &ChordManager{procs: make(map[string]*ChordProcess)}
	mgr.cfg.Store(cfg)

	key1 := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	key2 := (processKey{workspaceID: "ws2", imType: "feishu", chatID: "chat-2"}).String()
	p1 := &ChordProcess{key: key1, workspaceID: "ws1", stdin: &captureWriteCloser{}}
	p2 := &ChordProcess{key: key2, workspaceID: "ws2", stdin: &captureWriteCloser{}}
	mgr.procs[key1] = p1
	mgr.procs[key2] = p2

	if got := mgr.GetProcessForKey(key1); got != p1 {
		t.Fatalf("GetProcessForKey key1 = %#v, want p1", got)
	}
	if got := mgr.GetProcessForKey("missing"); got != nil {
		t.Fatalf("GetProcessForKey missing = %#v, want nil", got)
	}

	mgr.StopProcessKey("missing")
	if got := len(mgr.procs); got != 2 {
		t.Fatalf("StopProcessKey missing changed map size to %d", got)
	}

	mgr.StopProcessKey(key1)
	if _, ok := mgr.procs[key1]; ok {
		t.Fatal("StopProcessKey did not remove key1")
	}
	if !p1.stoppedByGateway {
		t.Fatal("StopProcessKey should mark process stopped by gateway")
	}

	mgr.StopProcessKey(key2)
	if _, ok := mgr.procs[key2]; ok {
		t.Fatal("StopProcessKey did not remove key2")
	}
	if !p2.stoppedByGateway {
		t.Fatal("StopProcessKey should mark process stopped by gateway")
	}
}

func TestChordManagerLifecycleLocksAreScopedAndReleased(t *testing.T) {
	mgr := &ChordManager{}
	key1 := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	key2 := (processKey{workspaceID: "ws2", imType: "wechat", chatID: "chat-2"}).String()

	releaseKey1 := mgr.acquireKeyLock(key1)
	if got := lifecycleKeyLockCount(mgr); got != 1 {
		t.Fatalf("lifecycle lock count = %d, want 1", got)
	}
	releaseKey2 := mgr.acquireKeyLock(key2)
	if got := lifecycleKeyLockCount(mgr); got != 2 {
		t.Fatalf("lifecycle lock count = %d, want 2", got)
	}
	releaseKey2()
	releaseKey1()
	if got := lifecycleKeyLockCount(mgr); got != 0 {
		t.Fatalf("released lifecycle locks retained: %d", got)
	}
}

func TestChordManagerStopWaitsForSameKeyLifecycleOperation(t *testing.T) {
	mgr := &ChordManager{procs: make(map[string]*ChordProcess)}
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	p := &ChordProcess{key: key, stdin: &captureWriteCloser{}}
	mgr.procs[key] = p

	releaseKeyLock := mgr.acquireKeyLock(key)
	stopped := make(chan struct{})
	go func() {
		mgr.StopProcessKey(key)
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("StopProcessKey completed during another lifecycle operation for the same key")
	case <-time.After(20 * time.Millisecond):
	}
	if got := mgr.GetProcessForKey(key); got != p {
		t.Fatalf("process changed while lifecycle lock was held: got %#v, want %#v", got, p)
	}

	releaseKeyLock()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("StopProcessKey did not complete after lifecycle operation released the key")
	}
	if got := mgr.GetProcessForKey(key); got != nil {
		t.Fatalf("process remains after StopProcessKey: %#v", got)
	}
	if got := lifecycleKeyLockCount(mgr); got != 0 {
		t.Fatalf("released lifecycle lock retained: %d", got)
	}
}

func TestChordManagerDifferentKeyLifecycleOperationsRemainParallel(t *testing.T) {
	mgr := &ChordManager{procs: make(map[string]*ChordProcess)}
	key1 := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	key2 := (processKey{workspaceID: "ws2", imType: "wechat", chatID: "chat-2"}).String()
	mgr.procs[key2] = &ChordProcess{key: key2, stdin: &captureWriteCloser{}}

	releaseKey1 := mgr.acquireKeyLock(key1)
	defer releaseKey1()

	stopped := make(chan struct{})
	go func() {
		mgr.StopProcessKey(key2)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("operation for a different process key was unnecessarily serialized")
	}
}

func lifecycleKeyLockCount(mgr *ChordManager) int {
	mgr.keyLocksMu.Lock()
	defer mgr.keyLocksMu.Unlock()
	return len(mgr.keyLocks)
}

func TestChordManagerGetOrSpawnForKeyMissingWorkspace(t *testing.T) {
	mgr := &ChordManager{procs: make(map[string]*ChordProcess)}
	mgr.cfg.Store(&config.Config{})
	key := (processKey{workspaceID: "missing", imType: "wechat", chatID: "chat"}).String()
	p, err := mgr.GetOrSpawnForKey(key)
	if err != nil {
		t.Fatalf("GetOrSpawnForKey error = %v", err)
	}
	if p != nil {
		t.Fatalf("GetOrSpawnForKey missing = %#v, want nil", p)
	}
}

func TestChordManagerGetOrSpawnForKeyReplacesExitedProcess(t *testing.T) {
	chordBinary := makeFakeChordBinary(t, "")
	workspaceDir := t.TempDir()
	cfg := &config.Config{
		ChordPath:  chordBinary,
		Workspaces: []config.Workspace{{ID: "ws1", Path: workspaceDir}},
	}
	mgr := newTestChordManager(cfg)
	key := (processKey{workspaceID: "ws1", imType: "feishu", chatID: "chat-1"}).String()
	dead := &ChordProcess{key: key, workspaceID: "ws1"}
	mgr.procs[key] = dead

	p, err := mgr.GetOrSpawnForKey(key)
	if err != nil {
		t.Fatalf("GetOrSpawnForKey error = %v", err)
	}
	defer mgr.StopAll(time.Millisecond)
	if p == nil || p == dead {
		t.Fatalf("GetOrSpawnForKey returned %#v, want fresh process", p)
	}
	if got := mgr.GetProcessForKey(key); got != p {
		t.Fatalf("process map = %#v, want fresh process %#v", got, p)
	}
}

func TestProcessKeyRoundTripOpaqueParts(t *testing.T) {
	key := processKey{workspaceID: "ws|1", imType: "wechat", chatID: "chat|room|42"}.String()
	workspaceID, imType, chatID := parseProcessKey(key)
	if workspaceID != "ws|1" || imType != "wechat" || chatID != "chat|room|42" {
		t.Fatalf("parseProcessKey(%q) = (%q, %q, %q)", key, workspaceID, imType, chatID)
	}
}

func TestParseProcessKeyLegacyFormat(t *testing.T) {
	workspaceID, imType, chatID := parseProcessKey("ws1|feishu|chat-1")
	if workspaceID != "ws1" || imType != "feishu" || chatID != "chat-1" {
		t.Fatalf("legacy parse = (%q, %q, %q)", workspaceID, imType, chatID)
	}
}

func TestChordManagerSpawnArgsForPinnedSession(t *testing.T) {
	tmp := t.TempDir()
	pins := newSessionPinStore(tmp)
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	if err := pins.Set(key, " sess-123 "); err != nil {
		t.Fatalf("pin session: %v", err)
	}
	mgr := &ChordManager{pins: pins}
	got := mgr.spawnArgsForKey(key)
	if len(got) != 2 || got[0] != "--resume" || got[1] != "sess-123" {
		t.Fatalf("spawnArgsForKey = %v", got)
	}
}

func TestChordManagerSpawnArgsForLegacyPinnedSession(t *testing.T) {
	tmp := t.TempDir()
	pins := newSessionPinStore(tmp)
	legacyKey := legacyProcessKeyString("ws1", "wechat", "chat-1")
	if err := pins.Set(legacyKey, "sess-legacy"); err != nil {
		t.Fatalf("pin legacy session: %v", err)
	}
	mgr := &ChordManager{pins: pins}
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	got := mgr.spawnArgsForKey(key)
	if len(got) != 2 || got[0] != "--resume" || got[1] != "sess-legacy" {
		t.Fatalf("spawnArgsForKey legacy = %v", got)
	}
}

func TestNewChordManagerUsesSessionPinOverride(t *testing.T) {
	tmp := t.TempDir()
	pinsPath := filepath.Join(tmp, "custom-pins.json")
	paths := &config.Paths{StateDir: filepath.Join(tmp, "state")}
	mgr := NewChordManager(&config.Config{SessionPinsFile: pinsPath}, paths)
	if mgr.pins == nil || mgr.pins.path != pinsPath {
		t.Fatalf("pins path = %#v, want %q", mgr.pins, pinsPath)
	}
}

func TestChordProcessSendCommandClosedPipe(t *testing.T) {
	p := &ChordProcess{}
	if err := p.SendCommand(map[string]any{"type": "status"}); err != io.ErrClosedPipe {
		t.Fatalf("SendCommand closed pipe err = %v, want %v", err, io.ErrClosedPipe)
	}
}

func TestChordProcessLocalShellResultEndsTurn(t *testing.T) {
	payload, err := json.Marshal(LocalShellPayload{Command: "pwd", Output: "/tmp", Failed: false})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	p := &ChordProcess{
		state: ControlState{
			Busy:           true,
			Phase:          "local_shell",
			PhaseDetail:    "pwd",
			PendingConfirm: &ConfirmPayload{RequestID: "confirm-1"},
			LastError:      "previous error",
		},
	}

	p.processEnvelope(&HeadlessEnvelope{Type: "local_shell_result", Payload: payload})
	state := p.State()

	if state.Busy {
		t.Fatal("local_shell_result should mark process idle")
	}
	if state.LastLocalShell == nil || state.LastLocalShell.Command != "pwd" || state.LastLocalShell.Output != "/tmp" {
		t.Fatalf("LastLocalShell = %#v", state.LastLocalShell)
	}
	if state.PendingConfirm == nil || state.PendingConfirm.RequestID != "confirm-1" {
		t.Fatalf("PendingConfirm = %#v, want preserved", state.PendingConfirm)
	}
	if state.LastError != "previous error" {
		t.Fatalf("LastError = %q, want preserved", state.LastError)
	}
}

func TestChordProcessTerminateGroupNilAndNoProcess(t *testing.T) {
	var nilProcess *ChordProcess
	nilProcess.TerminateGroup(time.Millisecond)

	p := &ChordProcess{stdin: &captureWriteCloser{}}
	p.TerminateGroup(time.Millisecond)
	if !p.stoppedByGateway {
		t.Fatal("TerminateGroup should mark no-process instance stopped by gateway")
	}
}
