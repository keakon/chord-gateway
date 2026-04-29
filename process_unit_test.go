package main

import (
	"io"
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

func TestTruncateStderr(t *testing.T) {
	if got := truncateStderr("abcdef", 3); got != "def" {
		t.Fatalf("truncateStderr = %q, want def", got)
	}
	if got := truncateStderr("short", 10); got != "short" {
		t.Fatalf("truncateStderr short = %q", got)
	}
	if got := truncateStderr(strings.Repeat("x", 2100), 0); len(got) != 2000 {
		t.Fatalf("default truncate len = %d, want 2000", len(got))
	}
}

func TestChordManagerProcessLookupAndStop(t *testing.T) {
	cfg := &config.Config{Workspaces: []config.Workspace{{ID: "ws1", Path: t.TempDir()}, {ID: "ws2", Path: t.TempDir()}}}
	mgr := &ChordManager{cfg: cfg, procs: make(map[string]*ChordProcess)}

	key1 := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	key2 := (processKey{workspaceID: "ws2", imType: "feishu", chatID: "chat-2"}).String()
	p1 := &ChordProcess{key: key1, workspaceID: "ws1", stdin: &captureWriteCloser{}}
	p2 := &ChordProcess{key: key2, workspaceID: "ws2", stdin: &captureWriteCloser{}}
	mgr.procs[key1] = p1
	mgr.procs[key2] = p2

	if got := mgr.GetProcess("ws1"); got != p1 {
		t.Fatalf("GetProcess ws1 = %#v, want p1", got)
	}
	if got := mgr.GetProcess("missing"); got != nil {
		t.Fatalf("GetProcess missing = %#v, want nil", got)
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

	mgr.StopProcess("ws2")
	if _, ok := mgr.procs[key2]; ok {
		t.Fatal("StopProcess did not remove workspace process")
	}
	if !p2.stoppedByGateway {
		t.Fatal("StopProcess should mark process stopped by gateway")
	}
}

func TestChordManagerLegacyGetOrSpawnMissingWorkspace(t *testing.T) {
	mgr := &ChordManager{cfg: &config.Config{}, procs: make(map[string]*ChordProcess)}
	p, err := mgr.GetOrSpawn("missing")
	if err != nil {
		t.Fatalf("GetOrSpawn error = %v", err)
	}
	if p != nil {
		t.Fatalf("GetOrSpawn missing = %#v, want nil", p)
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

func TestChordProcessTerminateGroupNilAndNoProcess(t *testing.T) {
	var nilProcess *ChordProcess
	nilProcess.TerminateGroup(time.Millisecond)

	p := &ChordProcess{stdin: &captureWriteCloser{}}
	p.TerminateGroup(time.Millisecond)
	if !p.stoppedByGateway {
		t.Fatal("TerminateGroup should mark no-process instance stopped by gateway")
	}
}
