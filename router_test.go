package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/keakon/chord-gateway/config"
)

// ---------------------------------------------------------------------------
// test helpers
// ---------------------------------------------------------------------------

func testConfig() *config.Config {
	return &config.Config{
		IMs: []config.IMAdapterConfig{{Wechat: &config.WechatConfig{}}},
		Workspaces: []config.Workspace{
			{ID: "test", Path: "/tmp/test"},
		},
	}
}

func newTestRouter() *NotificationRouter {
	return &NotificationRouter{
		mgr:           newTestChordManager(testConfig()),
		lastKeyChatID: make(map[string]string),
	}
}

func TestNewNotificationRouterAndSetAdapter(t *testing.T) {
	cfg := testConfig()
	mgr := newTestChordManager(cfg)
	r := NewNotificationRouter(mgr)
	if r.mgr != mgr || r.getConfig() != cfg {
		t.Fatalf("router links not initialized")
	}
	if r.lastKeyChatID == nil {
		t.Fatalf("router maps should be initialized")
	}
	if mgr.onEvent == nil {
		t.Fatalf("manager onEvent should be registered")
	}

	adapter := &stubIMAdapter{typ: "wechat"}
	r.SetAdapter(adapter)
	if r.adapter != adapter {
		t.Fatalf("adapter was not set")
	}
}

func TestHandleIncomingMessageNoConfigLoaded(t *testing.T) {
	sender := &stubIMAdapter{typ: "console"}
	r := &NotificationRouter{adapter: sender, lastKeyChatID: make(map[string]string)}

	r.HandleIncomingMessage(IncomingMessage{IMType: "console", ChatID: "chat", SenderID: "user", Text: "/status"})

	if got := sender.lastMessage().text; !strings.Contains(got, "Configuration not loaded") {
		t.Fatalf("message = %q", got)
	}
}

func TestRespawnSessionWithoutManager(t *testing.T) {
	sender := &stubIMAdapter{typ: "wechat"}
	r := &NotificationRouter{adapter: sender}

	r.respawnSession(&config.Workspace{ID: "ws1", Path: "/tmp/ws1"}, "chat-1", "wechat", "")

	if got := sender.lastMessage().text; !strings.Contains(got, "Process manager not available") {
		t.Fatalf("message = %q", got)
	}
}

func TestHandleIncomingMessageRoutesMissingWorkspaceError(t *testing.T) {
	sender := &stubIMAdapter{typ: "wechat"}
	cfg := &config.Config{IMs: []config.IMAdapterConfig{{Wechat: &config.WechatConfig{WorkspaceID: "missing"}}}, Workspaces: []config.Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}
	r := &NotificationRouter{
		mgr:           newTestChordManager(cfg),
		adapter:       sender,
		lastKeyChatID: make(map[string]string),
	}

	r.HandleIncomingMessage(IncomingMessage{IMType: "wechat", ChatID: "chat-1", SenderID: "chat-1", Text: "hello"})
	if got := sender.lastMessage().text; !strings.Contains(got, "workspace_id") {
		t.Fatalf("message = %q", got)
	}
}

func TestHandleIncomingMessageNoWorkspace(t *testing.T) {
	sender := &stubIMAdapter{typ: "console"}
	cfg := &config.Config{Workspaces: nil}
	r := &NotificationRouter{
		mgr:           newTestChordManager(cfg),
		adapter:       sender,
		lastKeyChatID: make(map[string]string),
	}

	r.HandleIncomingMessage(IncomingMessage{IMType: "console", ChatID: "chat-1", Text: "hello"})
	if got := sender.lastMessage().text; !strings.Contains(got, "no workspace configured") {
		t.Fatalf("message = %q", got)
	}
}

func TestHandleIncomingMessageUsesInternalAction(t *testing.T) {
	cfg := &config.Config{
		IMs:        []config.IMAdapterConfig{{Feishu: &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", ChatBindings: map[string]string{"oc_chat": "ws1"}}}},
		Workspaces: []config.Workspace{{ID: "ws1", Path: "/tmp/ws1"}},
	}
	mgr := newTestChordManager(cfg)
	sender := &stubIMAdapter{typ: "feishu"}
	stdin := &captureWriteCloser{}
	key := (processKey{workspaceID: "ws1", imType: "feishu", chatID: "oc_chat"}).String()
	mgr.procs[key] = &ChordProcess{
		key:         key,
		workspaceID: "ws1",
		stdin:       stdin,
		state: ControlState{
			PendingConfirm:  &ConfirmPayload{RequestID: "req-confirm"},
			PendingQuestion: &QuestionPayload{RequestID: "req-question", Options: []string{"yes", "no"}},
		},
	}
	r := &NotificationRouter{mgr: mgr, adapter: sender, lastKeyChatID: make(map[string]string), expiredPending: make(map[string]expiredPendingState)}

	r.HandleIncomingMessage(IncomingMessage{
		IMType: "feishu",
		ChatID: "oc_chat",
		Text:   "",
		InternalAction: &InternalAction{
			Type:      "confirm",
			Action:    "allow",
			RequestID: "req-confirm",
		},
	})
	out := stdin.String()
	for _, want := range []string{`"type":"confirm"`, `"request_id":"req-confirm"`, `"action":"allow"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("confirm internal action missing %s in %q", want, out)
		}
	}
	if strings.Contains(out, `"type":"send"`) {
		t.Fatalf("confirm internal action was treated as send: %q", out)
	}

	stdin = &captureWriteCloser{}
	mgr.procs[key].stdin = stdin
	r.HandleIncomingMessage(IncomingMessage{
		IMType: "feishu",
		ChatID: "oc_chat",
		Text:   "",
		InternalAction: &InternalAction{
			Type:      "question",
			RequestID: "req-question",
			Value:     "yes",
		},
	})
	out = stdin.String()
	for _, want := range []string{`"type":"question"`, `"request_id":"req-question"`, `"answers":["yes"]`} {
		if !strings.Contains(out, want) {
			t.Fatalf("question internal action missing %s in %q", want, out)
		}
	}
}

func TestHandleBindCreatesWorkspaceAndBinding(t *testing.T) {
	dir := t.TempDir()
	defaultDir := filepath.Join(dir, "default")
	projectDir := filepath.Join(dir, "project-a")
	for _, p := range []string{defaultDir, projectDir} {
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(dir, "config.yaml")
	content := "ims:\n  feishu:\n    app_id: cli_xxx\n    app_secret: secret\nworkspaces:\n  default:\n    path: " + defaultDir + "\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	mgr := NewChordManager(cfg, &config.Paths{StateDir: t.TempDir()})
	sender := &stubIMAdapter{typ: "feishu"}
	r := NewNotificationRouter(mgr)
	r.SetConfigFile(configPath)
	r.SetAdapter(sender)

	r.HandleIncomingMessage(IncomingMessage{IMType: "feishu", ChatID: "oc_new", SenderID: "ou_user", Text: "/bind project-a \"" + projectDir + "\""})

	msg := sender.lastMessage().text
	if !strings.Contains(msg, "Bound") || !strings.Contains(msg, "project-a") {
		t.Fatalf("message = %q", msg)
	}
	if got := currentFeishuBinding(r.getConfig(), "oc_new"); got != "project-a" {
		t.Fatalf("binding = %q, want project-a", got)
	}
	ws := r.getConfig().WorkspaceByID("project-a")
	if ws == nil || ws.Path != projectDir {
		t.Fatalf("workspace project-a = %#v", ws)
	}
	r.mu.Lock()
	got := r.lastKeyChatID[(processKey{workspaceID: "project-a", imType: "feishu", chatID: "oc_new"}).String()]
	r.mu.Unlock()
	if got != "oc_new" {
		t.Fatalf("lastKeyChatID new binding = %q", got)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "oc_new: project-a") {
		t.Fatalf("updated config missing chat binding:\n%s", string(data))
	}
}

func TestHandleBindRejectsInvalidQuotedPath(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	content := "ims:\n  feishu:\n    app_id: cli_xxx\n    app_secret: secret\nworkspaces:\n  default:\n    path: ~/default\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	mgr := NewChordManager(cfg, &config.Paths{StateDir: t.TempDir()})
	sender := &stubIMAdapter{typ: "feishu"}
	r := NewNotificationRouter(mgr)
	r.SetConfigFile(configPath)
	r.SetAdapter(sender)

	r.HandleIncomingMessage(IncomingMessage{IMType: "feishu", ChatID: "oc_new", SenderID: "ou_user", Text: "/bind project-a \"~/work/project a"})

	if got := sender.lastMessage().text; got != "⚠️ Usage: /bind <workspace_id> <path>" {
		t.Fatalf("message = %q", got)
	}
	if got := currentFeishuBinding(r.getConfig(), "oc_new"); got != "" {
		t.Fatalf("binding = %q, want empty", got)
	}
	if ws := r.getConfig().WorkspaceByID("project-a"); ws != nil {
		t.Fatalf("workspace should not be created, got %#v", ws)
	}
}

func TestHandleBindFromDefaultWorkspaceStopsOldProcess(t *testing.T) {
	fakeChord := makeFakeChordBinary(t, "")
	stateDir := t.TempDir()
	dir := t.TempDir()
	defaultDir := filepath.Join(dir, "default")
	projectDir := filepath.Join(dir, "project a")
	for _, p := range []string{defaultDir, projectDir} {
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(dir, "config.yaml")
	content := "ims:\n  feishu:\n    app_id: cli_xxx\n    app_secret: secret\n    owner_open_id: ou_owner\nworkspaces:\n  default:\n    path: " + defaultDir + "\nchord_path: " + fakeChord + "\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	mgr := NewChordManager(cfg, &config.Paths{StateDir: stateDir})
	sender := &stubIMAdapter{typ: "feishu"}
	r := NewNotificationRouter(mgr)
	r.SetConfigFile(configPath)
	r.SetAdapter(sender)
	oldKey := (processKey{workspaceID: "default", imType: "feishu", chatID: "oc_chat"}).String()
	r.mu.Lock()
	r.lastKeyChatID[oldKey] = "oc_chat"
	r.mu.Unlock()
	r.scheduleReminder(oldKey, time.Now())
	if err := mgr.pins.Set(oldKey, "session-default"); err != nil {
		t.Fatalf("set pin: %v", err)
	}
	proc, err := mgr.SpawnWithArgsForKey(oldKey)
	if err != nil {
		t.Fatalf("SpawnWithArgsForKey() error = %v", err)
	}
	if proc == nil || !proc.Alive() {
		t.Fatalf("expected live old process, got %#v", proc)
	}

	r.HandleIncomingMessage(IncomingMessage{IMType: "feishu", ChatID: "oc_chat", SenderID: "ou_owner", Text: "/bind project-a \"" + projectDir + "\""})
	defer mgr.StopAll(time.Second)

	if got := currentFeishuBinding(r.getConfig(), "oc_chat"); got != "project-a" {
		t.Fatalf("binding = %q, want project-a", got)
	}
	if got := mgr.pins.Get(oldKey); got != "" {
		t.Fatalf("old pin = %q, want cleared", got)
	}
	if got := mgr.procs[oldKey]; got != nil {
		t.Fatalf("old process should be removed, got %#v", got)
	}
	r.mu.Lock()
	_, ok := r.lastKeyChatID[oldKey]
	r.mu.Unlock()
	if ok {
		t.Fatalf("old key should be removed from lastKeyChatID")
	}
	if _, ok := r.reminders[oldKey]; ok {
		t.Fatalf("old reminder should be removed")
	}
	ws := r.getConfig().WorkspaceByID("project-a")
	if ws == nil || ws.Path != projectDir {
		t.Fatalf("workspace project-a = %#v", ws)
	}
}

func TestHandleBindUpdatesExistingBindingAndStopsOldProcess(t *testing.T) {
	fakeChord := makeFakeChordBinary(t, "")
	stateDir := t.TempDir()
	dir := t.TempDir()
	oldDir := filepath.Join(dir, "old")
	newDir := filepath.Join(dir, "new")
	for _, p := range []string{oldDir, newDir} {
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(dir, "config.yaml")
	content := "ims:\n  feishu:\n    app_id: cli_xxx\n    app_secret: secret\n    owner_open_id: ou_owner\n    chat_bindings:\n      oc_chat: ws-old\nworkspaces:\n  ws-old:\n    path: " + oldDir + "\n  ws-new:\n    path: " + newDir + "\nchord_path: " + fakeChord + "\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	mgr := NewChordManager(cfg, &config.Paths{StateDir: stateDir})
	sender := &stubIMAdapter{typ: "feishu"}
	r := NewNotificationRouter(mgr)
	r.SetConfigFile(configPath)
	r.SetAdapter(sender)
	oldKey := (processKey{workspaceID: "ws-old", imType: "feishu", chatID: "oc_chat"}).String()
	r.mu.Lock()
	r.lastKeyChatID[oldKey] = "oc_chat"
	r.mu.Unlock()
	if err := mgr.pins.Set(oldKey, "session-old"); err != nil {
		t.Fatalf("set pin: %v", err)
	}
	proc, err := mgr.SpawnWithArgsForKey(oldKey)
	if err != nil {
		t.Fatalf("SpawnWithArgsForKey() error = %v", err)
	}
	if proc == nil || !proc.Alive() {
		t.Fatalf("expected live old process, got %#v", proc)
	}

	// Bind to ws-new with its existing path (path must match).
	r.HandleIncomingMessage(IncomingMessage{IMType: "feishu", ChatID: "oc_chat", SenderID: "ou_owner", Text: "/bind ws-new \"" + newDir + "\""})
	defer mgr.StopAll(time.Second)

	msg := sender.lastMessage().text
	if !strings.Contains(msg, "Binding updated") || !strings.Contains(msg, "ws-new") {
		t.Fatalf("message = %q", msg)
	}
	if got := currentFeishuBinding(r.getConfig(), "oc_chat"); got != "ws-new" {
		t.Fatalf("binding = %q, want ws-new", got)
	}
	if got := mgr.pins.Get(oldKey); got != "" {
		t.Fatalf("old pin = %q, want cleared", got)
	}
	if got := mgr.procs[oldKey]; got != nil {
		t.Fatalf("old process should be removed, got %#v", got)
	}
	r.mu.Lock()
	_, ok := r.lastKeyChatID[oldKey]
	r.mu.Unlock()
	if ok {
		t.Fatalf("old key should be removed from lastKeyChatID")
	}
	newKey := (processKey{workspaceID: "ws-new", imType: "feishu", chatID: "oc_chat"}).String()
	r.mu.Lock()
	got := r.lastKeyChatID[newKey]
	r.mu.Unlock()
	if got != "oc_chat" {
		t.Fatalf("new key chat id = %q", got)
	}
}

func TestHandleBindDoesNotSpawnProcessForBusyCheck(t *testing.T) {
	fakeChord := makeFakeChordBinary(t, "")
	stateDir := t.TempDir()
	dir := t.TempDir()
	oldDir := filepath.Join(dir, "old")
	newDir := filepath.Join(dir, "new")
	for _, p := range []string{oldDir, newDir} {
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(dir, "config.yaml")
	content := "ims:\n  feishu:\n    app_id: cli_xxx\n    app_secret: secret\n    owner_open_id: ou_owner\n    chat_bindings:\n      oc_chat: ws-old\nworkspaces:\n  ws-old:\n    path: " + oldDir + "\n  ws-new:\n    path: " + newDir + "\nchord_path: " + fakeChord + "\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	mgr := NewChordManager(cfg, &config.Paths{StateDir: stateDir})
	sender := &stubIMAdapter{typ: "feishu"}
	r := NewNotificationRouter(mgr)
	r.SetConfigFile(configPath)
	r.SetAdapter(sender)
	oldKey := (processKey{workspaceID: "ws-old", imType: "feishu", chatID: "oc_chat"}).String()
	if got := mgr.GetProcessForKey(oldKey); got != nil {
		t.Fatalf("precondition failed: unexpected process %#v", got)
	}

	r.HandleIncomingMessage(IncomingMessage{IMType: "feishu", ChatID: "oc_chat", SenderID: "ou_owner", Text: "/bind ws-new \"" + newDir + "\""})
	defer mgr.StopAll(time.Second)

	if got := sender.lastMessage().text; !strings.Contains(got, "Binding updated") || !strings.Contains(got, "ws-new") {
		t.Fatalf("message = %q", got)
	}
	if got := mgr.GetProcessForKey(oldKey); got != nil {
		t.Fatalf("busy check should not spawn old process, got %#v", got)
	}
	if got := currentFeishuBinding(r.getConfig(), "oc_chat"); got != "ws-new" {
		t.Fatalf("binding = %q, want ws-new", got)
	}
}

func TestHandleBindRejectsPathMismatch(t *testing.T) {
	dir := t.TempDir()
	originalDir := filepath.Join(dir, "original")
	differentDir := filepath.Join(dir, "different")
	for _, p := range []string{originalDir, differentDir} {
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(dir, "config.yaml")
	content := "ims:\n  feishu:\n    app_id: cli_xxx\n    app_secret: secret\n    owner_open_id: ou_owner\nworkspaces:\n  ws1:\n    path: " + originalDir + "\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	mgr := NewChordManager(cfg, &config.Paths{StateDir: t.TempDir()})
	sender := &stubIMAdapter{typ: "feishu"}
	r := NewNotificationRouter(mgr)
	r.SetConfigFile(configPath)
	r.SetAdapter(sender)

	// Try to bind to ws1 with a different path — should error.
	r.HandleIncomingMessage(IncomingMessage{IMType: "feishu", ChatID: "oc_chat", SenderID: "ou_owner", Text: "/bind ws1 \"" + differentDir + "\""})
	msg := sender.lastMessage().text
	if !strings.Contains(msg, "❌") || !strings.Contains(msg, "refusing to overwrite") {
		t.Fatalf("expected path mismatch error, got: %q", msg)
	}
}

func TestHandleBindRejectsUnauthorizedSender(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	content := "ims:\n  feishu:\n    app_id: cli_xxx\n    app_secret: secret\n    owner_open_id: ou_owner\nworkspaces:\n  default:\n    path: ~/default\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	mgr := NewChordManager(cfg, &config.Paths{StateDir: t.TempDir()})
	sender := &stubIMAdapter{typ: "feishu"}
	r := NewNotificationRouter(mgr)
	r.SetConfigFile(configPath)
	r.SetAdapter(sender)

	// Sender not in allowed list: gateway must not reply.
	r.HandleIncomingMessage(IncomingMessage{IMType: "feishu", ChatID: "oc_chat", SenderID: "ou_stranger", Text: "/bind project-a ~/work/project-a"})
	if msg := sender.lastMessage(); msg.text != "" {
		t.Fatalf("expected no reply for unauthorized sender, got: %q", msg.text)
	}
}

func TestHandleSessions(t *testing.T) {
	sender := &stubIMAdapter{typ: "wechat"}
	r := &NotificationRouter{adapter: sender}

	t.Run("missing sessions directory", func(t *testing.T) {
		r.handleSessions(&config.Workspace{ID: "ws", Path: t.TempDir()}, "chat")
		if got := sender.lastMessage().text; got != "📂 No sessions found." {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("lists directories newest first and skips files", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat"}
		r := &NotificationRouter{adapter: sender}
		root := t.TempDir()
		sessionsDir := filepath.Join(root, ".chord", "sessions")
		if err := os.MkdirAll(filepath.Join(sessionsDir, "2026-01-01"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(sessionsDir, "2026-01-03"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sessionsDir, "not-a-session"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}

		r.handleSessions(&config.Workspace{ID: "ws", Path: root}, "chat")
		msg := sender.lastMessage().text
		if !strings.Contains(msg, "📂 Recent sessions:") || !strings.Contains(msg, "2026-01-03") || !strings.Contains(msg, "2026-01-01") {
			t.Fatalf("message = %q", msg)
		}
		if strings.Contains(msg, "not-a-session") {
			t.Fatalf("file should be skipped: %q", msg)
		}
		if strings.Index(msg, "2026-01-03") > strings.Index(msg, "2026-01-01") {
			t.Fatalf("sessions should be listed newest name first: %q", msg)
		}
	})
}

func TestHandleChordEventRoutesDirectAndLegacy(t *testing.T) {
	t.Run("direct assistant message", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat"}
		r := &NotificationRouter{adapter: sender}
		key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
		r.HandleChordEvent(key, "assistant_message", ControlState{LastAssistantText: "done"})
		if got := sender.lastMessage(); got.chatID != "chat-1" || got.text != "done" {
			t.Fatalf("message = %#v", got)
		}
	})

	t.Run("invalid key is ignored", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat"}
		r := &NotificationRouter{adapter: sender}
		r.HandleChordEvent((processKey{workspaceID: "ws1"}).String(), "assistant_message", ControlState{LastAssistantText: "legacy"})
		if got := sender.lastMessage(); got.text != "" {
			t.Fatalf("invalid key should not send message, got %#v", got)
		}
	})

	t.Run("todos notification forwards full list", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat"}
		r := &NotificationRouter{adapter: sender}
		key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
		state := ControlState{Todos: []TodoItem{
			{ID: "1", Content: "work", Status: "in_progress", ActiveForm: "editing"},
			{ID: "2", Content: "test", Status: "pending"},
		}}
		r.HandleChordEvent(key, "todos", state)
		got := sender.lastMessage().text
		for _, want := range []string{"📋 Todos:", "▶ work (editing)", "⬜ test"} {
			if !strings.Contains(got, want) {
				t.Fatalf("todos message missing %q in %q", want, got)
			}
		}
		count := len(sender.sentMessages())
		r.HandleChordEvent(key, "todos", state)
		if got := len(sender.sentMessages()); got != count+1 {
			t.Fatalf("todos event should forward every time: got %d want %d", got, count+1)
		}
	})
}

func TestBroadcastExceptSkipsExcludedAndUnknownAdapters(t *testing.T) {
	wechat := &stubIMAdapter{typ: "wechat"}
	feishu := &stubIMAdapter{typ: "feishu"}
	r := &NotificationRouter{
		adapter: &MultiAdapter{adapters: []IMAdapter{wechat, feishu}},
		mgr:     newTestChordManager(&config.Config{IMs: []config.IMAdapterConfig{{Feishu: &config.FeishuConfig{ChatBindings: map[string]string{"feishu-chat": "ws1"}}}}, Workspaces: []config.Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}),
		lastKeyChatID: map[string]string{
			(processKey{workspaceID: "ws1", imType: "wechat", chatID: "wechat-chat"}).String(): "wechat-chat",
		},
	}

	r.broadcastExcept("wechat", "hello")
	if got := len(wechat.sentMessages()); got != 0 {
		t.Fatalf("excluded wechat received %d messages", got)
	}
	if got := feishu.lastMessage(); got.chatID != "feishu-chat" || got.text != "hello" {
		t.Fatalf("feishu message = %#v", got)
	}

	before := len(feishu.sentMessages())
	r.broadcastExcept("unknown", "ignored")
	if got := len(feishu.sentMessages()); got != before+1 {
		t.Fatalf("unknown excluded adapter should still broadcast to known adapters, got %d want %d", got, before+1)
	}
}

func TestHandleNewStartsFreshSessionAndClearsPin(t *testing.T) {
	fakeChord := makeFakeChordBinary(t, "")
	stateDir := t.TempDir()
	workspaceDir := t.TempDir()
	ws := &config.Workspace{ID: "ws1", Path: workspaceDir}
	cfg := &config.Config{ChordPath: fakeChord, Workspaces: []config.Workspace{*ws}}
	mgr := NewChordManager(cfg, &config.Paths{StateDir: stateDir})
	sender := &stubIMAdapter{typ: "wechat"}
	r := &NotificationRouter{mgr: mgr, adapter: sender}
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	if err := mgr.pins.Set(key, "old-session"); err != nil {
		t.Fatalf("set old pin: %v", err)
	}

	// Spawn an initial process so /new has something to send to.
	stdin := &captureWriteCloser{}
	proc, err := mgr.SpawnWithArgsForKey(key)
	if err != nil {
		t.Fatalf("SpawnWithArgsForKey() error = %v", err)
	}
	proc.stdin = stdin

	r.handleNew(ws, "chat-1", "wechat")
	defer mgr.StopAll(time.Second)

	if got := mgr.pins.Get(key); got != "" {
		t.Fatalf("pin = %q, want cleared", got)
	}
	if got := sender.lastMessage().text; got != "🆕 /new sent to chord process." {
		t.Fatalf("message = %q", got)
	}
	if !strings.Contains(stdin.String(), `"/new"`) {
		t.Fatalf("stdin should contain /new command, got %q", stdin.String())
	}
}

func TestHandleResumePinsSessionAndStartsWithResumeArgs(t *testing.T) {
	fakeChord, argsFile := makeFakeChordBinaryWithArgsFile(t, "")
	stateDir := t.TempDir()
	workspaceDir := t.TempDir()
	ws := &config.Workspace{ID: "ws1", Path: workspaceDir}
	cfg := &config.Config{ChordPath: fakeChord, Workspaces: []config.Workspace{*ws}}
	mgr := NewChordManager(cfg, &config.Paths{StateDir: stateDir})
	sender := &stubIMAdapter{typ: "wechat"}
	r := &NotificationRouter{mgr: mgr, adapter: sender}
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()

	r.handleResume(ws, "chat-1", "session-123", "wechat")
	defer mgr.StopAll(time.Second)

	if got := mgr.pins.Get(key); got != "session-123" {
		t.Fatalf("pin = %q, want session-123", got)
	}
	if got := sender.lastMessage().text; got != "🔄 Resuming session session-123" {
		t.Fatalf("message = %q", got)
	}
	args := readFakeChordArgs(t, argsFile)
	requireContains(t, args, "--resume")
	requireContains(t, args, "session-123")
	if got := mgr.GetProcessForKey(key); got == nil || !got.Alive() {
		t.Fatalf("expected live process, got %#v", got)
	}
}

func TestHandleResumeDoesNotSpawnProcessForBusyCheck(t *testing.T) {
	fakeChord, argsFile := makeFakeChordBinaryWithArgsFile(t, "")
	stateDir := t.TempDir()
	workspaceDir := t.TempDir()
	ws := &config.Workspace{ID: "ws1", Path: workspaceDir}
	cfg := &config.Config{ChordPath: fakeChord, Workspaces: []config.Workspace{*ws}}
	mgr := NewChordManager(cfg, &config.Paths{StateDir: stateDir})
	sender := &stubIMAdapter{typ: "wechat"}
	r := &NotificationRouter{mgr: mgr, adapter: sender}
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	if got := mgr.GetProcessForKey(key); got != nil {
		t.Fatalf("precondition failed: unexpected process %#v", got)
	}

	r.handleResume(ws, "chat-1", "session-123", "wechat")
	defer mgr.StopAll(time.Second)

	if got := sender.lastMessage().text; got != "🔄 Resuming session session-123" {
		t.Fatalf("message = %q", got)
	}
	args := readFakeChordArgs(t, argsFile)
	if got := strings.Count(args, "--resume"); got != 1 {
		t.Fatalf("resume spawn count = %d, want 1; args = %q", got, args)
	}
	if got := mgr.GetProcessForKey(key); got == nil || !got.Alive() {
		t.Fatalf("expected live resumed process, got %#v", got)
	}
}

func TestHandleNewAndResumeErrorPaths(t *testing.T) {
	ws := &config.Workspace{ID: "ws1", Path: t.TempDir()}
	t.Run("new missing workspace", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat"}
		cfg := &config.Config{ChordPath: makeFakeChordBinary(t, ""), Workspaces: nil}
		r := &NotificationRouter{mgr: NewChordManager(cfg, &config.Paths{StateDir: t.TempDir()}), adapter: sender}

		r.handleNew(ws, "chat-1", "wechat")
		if got := sender.lastMessage().text; got != "❌ No active chord process." {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("resume spawn failure", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat"}
		cfg := &config.Config{ChordPath: filepath.Join(t.TempDir(), "missing-chord"), Workspaces: []config.Workspace{*ws}}
		r := &NotificationRouter{mgr: NewChordManager(cfg, &config.Paths{StateDir: t.TempDir()}), adapter: sender}

		r.handleResume(ws, "chat-1", "session-123", "wechat")
		if got := sender.lastMessage().text; got != "❌ Failed to resume session." {
			t.Fatalf("message = %q", got)
		}
	})
}

func TestHandleCurrentAndTodosNoActiveSession(t *testing.T) {
	ws := &config.Workspace{ID: "ws1", Path: t.TempDir()}
	cfg := &config.Config{Workspaces: nil}
	mgr := newTestChordManager(cfg)

	t.Run("current no active session", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat"}
		r := &NotificationRouter{mgr: mgr, adapter: sender}
		r.handleCurrent(ws, "chat-1", "wechat")
		msg := sender.lastMessage().text
		if !strings.Contains(msg, "Workspace: ws1") || !strings.Contains(strings.ToLower(msg), "idle") {
			t.Fatalf("current message = %q", msg)
		}
	})

	t.Run("todos no active session", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat"}
		r := &NotificationRouter{mgr: mgr, adapter: sender}
		r.handleTodos(ws, "chat-1", "wechat")
		if got := sender.lastMessage().text; got != "⏸️ No active session." {
			t.Fatalf("todos message = %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// parseIMCommand
// ---------------------------------------------------------------------------

func TestParseIMCommand(t *testing.T) {
	tests := []struct {
		name            string
		input           string
		wantType        string
		wantContent     string
		wantRequestID   string
		wantAction      string
		wantAnswers     []string
		wantSessionID   string
		wantWorkspaceID string
		wantPath        string
		wantReason      string
		wantInvalid     bool
	}{
		{name: "status", input: "/status", wantType: "status"},
		{name: "summary", input: "/summary", wantType: "send", wantContent: "/summary"},
		{name: "cancel", input: "/cancel", wantType: "cancel"},
		{name: "allow with request_id", input: "/allow req-1", wantType: "confirm", wantAction: "allow", wantRequestID: "req-1"},
		{name: "deny with reason", input: "/deny not good", wantType: "confirm", wantAction: "deny", wantReason: "not good"},
		{name: "answer", input: "/answer yes", wantType: "question", wantAnswers: []string{"yes"}},
		{name: "new", input: "/new", wantType: "new"},
		{name: "resume with session_id", input: "/resume 123", wantType: "resume", wantSessionID: "123"},
		{name: "sessions", input: "/sessions", wantType: "sessions"},
		{name: "current", input: "/current", wantType: "current"},
		{name: "todos", input: "/todos", wantType: "todos"},
		{name: "bind with workspace and path", input: "/bind project-a ~/work/project-a", wantType: "bind", wantWorkspaceID: "project-a", wantPath: "~/work/project-a"},
		{name: "bind with quoted path", input: "/bind project-a \"~/work/project a\"", wantType: "bind", wantWorkspaceID: "project-a", wantPath: "~/work/project a"},
		{name: "bind with unterminated quoted path", input: "/bind project-a \"~/work/project a", wantType: "bind", wantInvalid: true},
		{name: "bind with extra argument", input: "/bind project-a ~/work/project-a extra", wantType: "bind", wantInvalid: true},
		{name: "plain text becomes send", input: "hello world", wantType: "send", wantContent: "hello world"},
		{name: "unknown slash command becomes send", input: "/unknown", wantType: "send", wantContent: "/unknown"},
		{name: "allow without request_id", input: "/allow", wantType: "confirm", wantAction: "allow", wantRequestID: ""},
		{name: "login without target", input: "/login", wantType: "login", wantContent: ""},
		{name: "login wechat", input: "/login wechat", wantType: "login", wantContent: "wechat"},
		{name: "login feishu", input: "/login feishu", wantType: "login", wantContent: "feishu"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseIMCommand(tt.input)
			if got.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", got.Type, tt.wantType)
			}
			if got.Content != tt.wantContent {
				t.Errorf("Content = %q, want %q", got.Content, tt.wantContent)
			}
			if got.RequestID != tt.wantRequestID {
				t.Errorf("RequestID = %q, want %q", got.RequestID, tt.wantRequestID)
			}
			if got.Action != tt.wantAction {
				t.Errorf("Action = %q, want %q", got.Action, tt.wantAction)
			}
			if tt.wantAnswers != nil {
				if len(got.Answers) != len(tt.wantAnswers) {
					t.Errorf("Answers = %v, want %v", got.Answers, tt.wantAnswers)
				} else {
					for i := range tt.wantAnswers {
						if got.Answers[i] != tt.wantAnswers[i] {
							t.Errorf("Answers[%d] = %q, want %q", i, got.Answers[i], tt.wantAnswers[i])
						}
					}
				}
			}
			if got.SessionID != tt.wantSessionID {
				t.Errorf("SessionID = %q, want %q", got.SessionID, tt.wantSessionID)
			}
			if got.WorkspaceID != tt.wantWorkspaceID {
				t.Errorf("WorkspaceID = %q, want %q", got.WorkspaceID, tt.wantWorkspaceID)
			}
			if got.Path != tt.wantPath {
				t.Errorf("Path = %q, want %q", got.Path, tt.wantPath)
			}
			if got.Invalid != tt.wantInvalid {
				t.Errorf("Invalid = %v, want %v", got.Invalid, tt.wantInvalid)
			}
			if got.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tt.wantReason)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// formatStatus
// ---------------------------------------------------------------------------

func TestFormatStatus(t *testing.T) {
	t.Run("busy shows spinner", func(t *testing.T) {
		s := formatBindingStatus(nil, "", "", ControlState{Busy: true})
		if !containsEmoji(s, "🔄") {
			t.Errorf("expected 🔄 in busy status, got: %s", s)
		}
	})

	t.Run("idle shows pause", func(t *testing.T) {
		s := formatBindingStatus(nil, "", "", ControlState{Busy: false})
		if !containsEmoji(s, "⏸️") {
			t.Errorf("expected ⏸️ in idle status, got: %s", s)
		}
	})

	t.Run("pending confirm shows wrench", func(t *testing.T) {
		s := formatBindingStatus(nil, "", "", ControlState{
			Busy:           true,
			PendingConfirm: &ConfirmPayload{ToolName: "Shell", RequestID: "r1"},
		})
		if !containsEmoji(s, "🔧") {
			t.Errorf("expected 🔧 with pending confirm, got: %s", s)
		}
	})

	t.Run("pending question shows question mark", func(t *testing.T) {
		s := formatBindingStatus(nil, "", "", ControlState{
			Busy:            false,
			PendingQuestion: &QuestionPayload{Question: "Continue?", RequestID: "r2"},
		})
		if !containsEmoji(s, "❓") {
			t.Errorf("expected ❓ with pending question, got: %s", s)
		}
	})

	t.Run("last error shows cross", func(t *testing.T) {
		s := formatBindingStatus(nil, "", "", ControlState{
			Busy:      false,
			LastError: "something broke",
		})
		if !containsEmoji(s, "❌") {
			t.Errorf("expected ❌ with last error, got: %s", s)
		}
	})
}

func containsEmoji(s, emoji string) bool {
	return strings.Contains(s, emoji)
}

func TestConfiguredHeadlessSubscribeEvents(t *testing.T) {
	got := configuredHeadlessSubscribeEvents(&config.Config{})
	wantCore := []string{"assistant_message", "confirm_request", "question_request", "idle", "error", "notification", "done_completion"}
	if strings.Join(got, ",") != strings.Join(wantCore, ",") {
		t.Fatalf("default subscribe events = %v, want %v", got, wantCore)
	}

	got = configuredHeadlessSubscribeEvents(&config.Config{EventVisibility: config.EventVisibility{
		Activity:  true,
		AgentDone: true,
		Info:      true,
		Toast:     true,
		Todos:     true,
	}})
	wantAll := []string{"assistant_message", "confirm_request", "question_request", "idle", "error", "notification", "done_completion", "activity", "agent_done", "info", "toast", "todos"}
	if strings.Join(got, ",") != strings.Join(wantAll, ",") {
		t.Fatalf("configured subscribe events = %v, want %v", got, wantAll)
	}
}

// ---------------------------------------------------------------------------
// formatNotification
// ---------------------------------------------------------------------------

func TestFormatNotification_NotificationEvent(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		msg    string
		emoji  string
	}{
		{name: "idle", reason: "idle", msg: "Chord: Ready for input", emoji: "✅"},
		{name: "error", reason: "error", msg: "something failed", emoji: "⚠️"},
		{name: "cancelled", reason: "cancelled", msg: "Chord: Ready for input", emoji: "⚠️"},
		{name: "confirm", reason: "confirm_request", msg: "Chord: Permission confirmation required", emoji: "🔧"},
		{name: "question", reason: "question_request", msg: "Chord: Question requires your input", emoji: "❓"},
		{name: "plain", reason: "other", msg: "plain text", emoji: "plain text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRouter()
			msg := r.formatNotification("notification", ControlState{
				LastNotification: &NotificationPayload{Reason: tt.reason, Message: tt.msg},
			})
			if msg == "" {
				t.Fatal("notification should push")
			}
			if !strings.Contains(msg, tt.emoji) {
				t.Fatalf("expected %s in notification, got: %s", tt.emoji, msg)
			}
		})
	}
}

func TestReminderDelayUsesLastVisiblePush(t *testing.T) {
	now := time.Now()
	if got := reminderDelay(time.Time{}, now); got != reminderInterval {
		t.Fatalf("zero last push delay = %v, want %v", got, reminderInterval)
	}
	if got := reminderDelay(now.Add(-2*time.Minute), now); got < 3*time.Minute-time.Second || got > 3*time.Minute+time.Second {
		t.Fatalf("recent last push delay = %v, want about 3m", got)
	}
	if got := reminderDelay(now.Add(-6*time.Minute), now); got > time.Millisecond {
		t.Fatalf("stale last push delay = %v, want immediate", got)
	}
}

func TestFormatNotification_StateEventsDoNotDuplicate(t *testing.T) {
	r := newTestRouter()
	if msg := r.formatNotification("confirm_request", ControlState{PendingConfirm: &ConfirmPayload{ToolName: "Shell"}}); msg == "" {
		t.Fatal("confirm_request should push")
	}
	if msg := r.formatNotification("question_request", ControlState{PendingQuestion: &QuestionPayload{Question: "Continue?"}}); msg == "" {
		t.Fatal("question_request should push")
	}
	if msg := r.formatNotification("error", ControlState{LastError: "boom"}); msg != "" {
		t.Fatalf("error = %q, want empty", msg)
	}
	if msg := r.formatNotification("idle", ControlState{LastOutcome: "completed"}); msg != "✅ Chord: Ready for input" {
		t.Fatalf("idle = %q, want %q", msg, "✅ Chord: Ready for input")
	}
	if msg := r.formatNotification("agent_done", ControlState{}); msg != "" {
		t.Fatalf("agent_done = %q, want empty", msg)
	}
	if msg := r.formatNotification("todos", ControlState{}); msg != "📋 No todos." {
		t.Fatalf("todos = %q, want %q", msg, "📋 No todos.")
	}
	if msg := r.formatNotification("unknown", ControlState{}); msg != "" {
		t.Fatalf("unknown = %q, want empty", msg)
	}
}

func TestFormatBindingStatus(t *testing.T) {
	ws := &config.Workspace{ID: "project-a", Path: "/tmp/project-a"}
	msg := formatBindingStatus(ws, "feishu", "oc_chat_a", ControlState{
		Busy:            true,
		SessionID:       "sess-1",
		Phase:           "planning",
		PhaseDetail:     "writing plan",
		LastOutcome:     "completed",
		PendingConfirm:  &ConfirmPayload{ToolName: "Shell"},
		PendingQuestion: &QuestionPayload{Question: "Continue?"},
		Todos: []TodoItem{
			{ID: "1", Content: "A", Status: "completed"},
			{ID: "2", Content: "B", Status: "pending"},
		},
	})
	for _, want := range []string{"project-a", "oc_chat_a", "sess-1", "planning", "completed", "Pending confirm", "Pending question", "1/2 completed"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("expected %q in %q", want, msg)
		}
	}
}

func TestWorkspaceDisplayName(t *testing.T) {
	if got := workspaceDisplayName(nil); got != "(unknown)" {
		t.Fatalf("nil workspace = %q", got)
	}
	if got := workspaceDisplayName(&config.Workspace{Path: "/tmp/project/"}); got != "project" {
		t.Fatalf("trimmed path = %q", got)
	}
	if got := workspaceDisplayName(&config.Workspace{Path: "/"}); got != "/" {
		t.Fatalf("root path = %q", got)
	}
}

func TestFormatConfirmNotification(t *testing.T) {
	r := newTestRouter()

	t.Run("includes tool args summary for Shell", func(t *testing.T) {
		msg := r.formatConfirmNotification(ControlState{
			PendingConfirm: &ConfirmPayload{
				ToolName:  "Shell",
				ArgsJSON:  `{"command":"ls -la"}`,
				RequestID: "abc123",
			},
		})
		if !strings.Contains(msg, "$ ls -la") {
			t.Errorf("expected command summary, got: %s", msg)
		}
	})

	t.Run("includes tool args summary for Write", func(t *testing.T) {
		msg := r.formatConfirmNotification(ControlState{
			PendingConfirm: &ConfirmPayload{
				ToolName:  "Write",
				ArgsJSON:  `{"path":"src/main.go"}`,
				RequestID: "def456",
			},
		})
		if !strings.Contains(msg, "📝 src/main.go") {
			t.Errorf("expected path summary, got: %s", msg)
		}
	})

	t.Run("includes approval bullets", func(t *testing.T) {
		msg := r.formatConfirmNotification(ControlState{
			PendingConfirm: &ConfirmPayload{
				ToolName:      "Shell",
				ArgsJSON:      `{"command":"echo hi"}`,
				NeedsApproval: []string{"filesystem", "network"},
			},
		})
		for _, want := range []string{"filesystem", "network"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("expected %q in %q", want, msg)
			}
		}
	})

	t.Run("does not include request ID", func(t *testing.T) {
		msg := r.formatConfirmNotification(ControlState{
			PendingConfirm: &ConfirmPayload{
				ToolName:  "Shell",
				ArgsJSON:  `{"command":"echo hi"}`,
				RequestID: "hex123abc",
			},
		})
		if strings.Contains(msg, "hex123abc") {
			t.Errorf("request ID should not appear in notification, got: %s", msg)
		}
	})

	t.Run("reply prompt is simple", func(t *testing.T) {
		msg := r.formatConfirmNotification(ControlState{
			PendingConfirm: &ConfirmPayload{
				ToolName: "Shell",
				ArgsJSON: `{"command":"echo hi"}`,
			},
		})
		if !strings.Contains(msg, "Reply /allow or /deny") {
			t.Errorf("expected simple reply prompt, got: %s", msg)
		}
	})

	t.Run("empty ArgsJSON does not crash", func(t *testing.T) {
		msg := r.formatConfirmNotification(ControlState{
			PendingConfirm: &ConfirmPayload{ToolName: "Shell"},
		})
		if !strings.Contains(msg, "Shell") {
			t.Errorf("expected tool name, got: %s", msg)
		}
	})

	t.Run("includes Done report", func(t *testing.T) {
		msg := r.formatConfirmNotification(ControlState{
			PendingConfirm: &ConfirmPayload{
				ToolName: "Done",
				ArgsJSON: `{"reason":"loop target is complete","report":"## Completion status\nAll requested work is finished."}`,
			},
		})
		for _, want := range []string{"Done requests completion", "loop target is complete", "## Completion status", "Reply /allow to finish", "/deny <reason>"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("Done confirm missing %q in %q", want, msg)
			}
		}
	})

	t.Run("nil PendingConfirm returns empty", func(t *testing.T) {
		msg := r.formatConfirmNotification(ControlState{})
		if msg != "" {
			t.Errorf("expected empty, got: %s", msg)
		}
	})
}

func TestSummarizeToolArgs(t *testing.T) {
	long := strings.Repeat("x", 350)
	tests := []struct {
		name     string
		toolName string
		argsJSON string
		contains []string
	}{
		{name: "empty", toolName: "Shell", argsJSON: "", contains: []string{""}},
		{name: "invalid json", toolName: "Shell", argsJSON: "{not-json", contains: []string{"{not-json"}},
		{name: "spawn command", toolName: "Spawn", argsJSON: `{"command":"echo hi"}`, contains: []string{"$ echo hi"}},
		{name: "edit path", toolName: "Edit", argsJSON: `{"path":"a.go"}`, contains: []string{"📝 a.go"}},
		{name: "delete paths", toolName: "Delete", argsJSON: `{"paths":["a.go","b.go"]}`, contains: []string{"🗑️ a.go"}},
		{name: "read path", toolName: "Read", argsJSON: `{"path":"README.md"}`, contains: []string{"📖 README.md"}},
		{name: "glob pattern", toolName: "Glob", argsJSON: `{"pattern":"**/*.go"}`, contains: []string{"🔍 **/*.go"}},
		{name: "webfetch url", toolName: "WebFetch", argsJSON: `{"url":"https://example.com"}`, contains: []string{"🌐 https://example.com"}},
		{name: "lsp summary", toolName: "Lsp", argsJSON: `{"operation":"definition","path":"main.go"}`, contains: []string{"🔎 definition main.go"}},
		{name: "done report", toolName: "Done", argsJSON: `{"reason":"ready to finish","report":"## Summary\n- shipped"}`, contains: []string{"## Summary", "shipped"}},
		{name: "generic fallback", toolName: "Other", argsJSON: `{"description":"desc","path":"x","content":"y"}`, contains: []string{"path=x", "content=y"}},
		{name: "truncate command", toolName: "Shell", argsJSON: fmt.Sprintf(`{"command":%q}`, long), contains: []string{"$ ", "…"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarizeToolArgs(tt.toolName, tt.argsJSON)
			if len(tt.contains) == 1 && tt.contains[0] == "" {
				if got != "" {
					t.Fatalf("got %q, want empty", got)
				}
				return
			}
			for _, want := range tt.contains {
				if !strings.Contains(got, want) {
					t.Fatalf("expected %q in %q", want, got)
				}
			}
		})
	}
}

func TestTruncateLineAndTruncate(t *testing.T) {
	if got := truncateLine("a\nb", 10); got != `a\nb` {
		t.Fatalf("newline replacement = %q", got)
	}
	if got := truncateLine("abcdef", 4); got != "abc…" {
		t.Fatalf("truncateLine ascii = %q", got)
	}
	if got := truncateLine("こんにちは", 3); got != "こん…" {
		t.Fatalf("truncateLine japanese = %q", got)
	}
	if got := truncateLine("😀😃😄😁", 3); got != "😀😃…" {
		t.Fatalf("truncateLine emoji = %q", got)
	}
	if got := truncateLine("😀😃😄😁", 2); got != "😀…" {
		t.Fatalf("truncateLine short emoji = %q", got)
	}
	if !utf8.ValidString(truncateLine("😀😃😄😁", 3)) {
		t.Fatal("truncateLine should return valid UTF-8")
	}
	if got := truncate(strings.Repeat("a", maxNotificationRunes+10)); !strings.HasSuffix(got, "...") || len([]rune(got)) != maxNotificationRunes {
		t.Fatalf("truncate len/suffix failed: len=%d got=%q", len(got), got)
	}
}

func TestFormatQuestionNotification(t *testing.T) {
	r := newTestRouter()

	t.Run("shows question text", func(t *testing.T) {
		msg := r.formatQuestionNotification(ControlState{
			PendingQuestion: &QuestionPayload{Question: "Continue?", RequestID: "req-1"},
		})
		if !strings.Contains(msg, "Continue?") {
			t.Errorf("expected question text, got: %s", msg)
		}
	})

	t.Run("shows options with details", func(t *testing.T) {
		msg := r.formatQuestionNotification(ControlState{
			PendingQuestion: &QuestionPayload{
				Question:      "How to proceed?",
				Options:       []string{"yes", "no"},
				OptionDetails: []string{"Yes, proceed", "No, stop"},
				RequestID:     "req-2",
			},
		})
		if !strings.Contains(msg, "yes — Yes, proceed") {
			t.Errorf("expected option with detail, got: %s", msg)
		}
		if !strings.Contains(msg, "no — No, stop") {
			t.Errorf("expected option with detail, got: %s", msg)
		}
	})

	t.Run("shows header default and multi-select", func(t *testing.T) {
		msg := r.formatQuestionNotification(ControlState{
			PendingQuestion: &QuestionPayload{
				Header:        "Pick",
				Question:      "Select files",
				Options:       []string{"A", "B"},
				DefaultAnswer: "A",
				Multiple:      true,
			},
		})
		for _, want := range []string{"Pick: Select files", "Default: A", "multi-select"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("expected %q in %q", want, msg)
			}
		}
	})

	t.Run("skips option detail when same as label", func(t *testing.T) {
		msg := r.formatQuestionNotification(ControlState{
			PendingQuestion: &QuestionPayload{
				Question:      "Pick one",
				Options:       []string{"A", "B"},
				OptionDetails: []string{"A", "B"},
			},
		})
		if strings.Contains(msg, "A — A") {
			t.Errorf("should not repeat label as detail, got: %s", msg)
		}
	})

	t.Run("nil PendingQuestion returns empty", func(t *testing.T) {
		msg := r.formatQuestionNotification(ControlState{})
		if msg != "" {
			t.Errorf("expected empty, got: %s", msg)
		}
	})
}

func TestFormatIdleNotification(t *testing.T) {
	r := &NotificationRouter{}
	if got := r.formatNotification("idle", ControlState{}); got != "✅ Chord: Ready for input" {
		t.Fatalf("idle without expired pending = %q", got)
	}
	if got := r.formatNotification("idle", ControlState{ExpiredQuestion: &QuestionPayload{Question: "Choose env"}}); !strings.Contains(got, "pending question has expired") {
		t.Fatalf("expired question notification = %q", got)
	}
	if got := r.formatNotification("idle_timeout", ControlState{ExpiredConfirm: &ConfirmPayload{RequestID: "req-c"}}); !strings.Contains(got, "pending confirmation has expired") {
		t.Fatalf("expired confirm notification = %q", got)
	}
}

func TestHandleChordEventClearsExpiredPendingWhenNewPendingArrives(t *testing.T) {
	r := &NotificationRouter{mgr: newTestChordManager(&config.Config{}), lastKeyChatID: make(map[string]string), expiredPending: make(map[string]expiredPendingState)}
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat1"}).String()

	r.HandleChordEvent(key, "idle", ControlState{ExpiredConfirm: &ConfirmPayload{RequestID: "old-confirm"}})
	if got := r.lookupExpiredPending(key).Confirm; got == nil || got.RequestID != "old-confirm" {
		t.Fatalf("expired confirm was not recorded: %#v", got)
	}

	r.HandleChordEvent(key, "confirm_request", ControlState{PendingConfirm: &ConfirmPayload{RequestID: "new-confirm"}})
	if got := r.lookupExpiredPending(key); got.Confirm != nil || got.Question != nil {
		t.Fatalf("expired pending was not cleared by new confirm: %#v", got)
	}

	r.HandleChordEvent(key, "idle", ControlState{ExpiredQuestion: &QuestionPayload{RequestID: "old-question"}})
	if got := r.lookupExpiredPending(key).Question; got == nil || got.RequestID != "old-question" {
		t.Fatalf("expired question was not recorded: %#v", got)
	}

	r.HandleChordEvent(key, "question_request", ControlState{PendingQuestion: &QuestionPayload{RequestID: "new-question", Question: "Continue?"}})
	if got := r.lookupExpiredPending(key); got.Confirm != nil || got.Question != nil {
		t.Fatalf("expired pending was not cleared by new question: %#v", got)
	}
}

func TestResolveQuestionAnswers(t *testing.T) {
	opts := []string{"yes", "no", "maybe"}
	q := &QuestionPayload{Options: opts}

	t.Run("single valid index", func(t *testing.T) {
		got := resolveQuestionAnswers("1", q)
		if len(got) != 1 || got[0] != "yes" {
			t.Errorf("got %v, want [yes]", got)
		}
	})

	t.Run("comma-separated indices for multi-select", func(t *testing.T) {
		q := &QuestionPayload{Options: opts, Multiple: true}
		got := resolveQuestionAnswers("1,3", q)
		if len(got) != 2 || got[0] != "yes" || got[1] != "maybe" {
			t.Errorf("got %v, want [yes maybe]", got)
		}
	})

	t.Run("non-numeric content becomes custom text", func(t *testing.T) {
		got := resolveQuestionAnswers("yes, please", q)
		if len(got) != 1 || got[0] != "yes, please" {
			t.Errorf("got %v, want [yes, please]", got)
		}
	})

	t.Run("out-of-range index becomes custom text", func(t *testing.T) {
		got := resolveQuestionAnswers("5", q)
		if len(got) != 1 || got[0] != "5" {
			t.Errorf("got %v, want [5]", got)
		}
	})

	t.Run("mixed numeric and non-numeric becomes custom text", func(t *testing.T) {
		got := resolveQuestionAnswers("1,yes", q)
		if len(got) != 1 || got[0] != "1,yes" {
			t.Errorf("got %v, want [1,yes]", got)
		}
	})

	t.Run("single-select with multiple indices becomes custom text", func(t *testing.T) {
		q := &QuestionPayload{Options: opts, Multiple: false}
		got := resolveQuestionAnswers("1,3", q)
		if len(got) != 1 || got[0] != "1,3" {
			t.Errorf("got %v, want [1,3]", got)
		}
	})

	t.Run("nil question passes through", func(t *testing.T) {
		got := resolveQuestionAnswers("hello", nil)
		if len(got) != 1 || got[0] != "hello" {
			t.Errorf("got %v, want [hello]", got)
		}
	})

	t.Run("empty options passes through", func(t *testing.T) {
		got := resolveQuestionAnswers("1", &QuestionPayload{Options: nil})
		if len(got) != 1 || got[0] != "1" {
			t.Errorf("got %v, want [1]", got)
		}
	})

	t.Run("free text /answer becomes custom text", func(t *testing.T) {
		got := resolveQuestionAnswers("yes", q)
		if len(got) != 1 || got[0] != "yes" {
			t.Errorf("got %v, want [yes]", got)
		}
	})
}

func TestFormatNotification_AssistantInfoToastAndLongRunning(t *testing.T) {
	r := newTestRouter()
	if msg := r.formatNotification("assistant_message", ControlState{LastAssistantText: "final answer"}); msg != "final answer" {
		t.Fatalf("assistant_message = %q, want final answer", msg)
	}
	if msg := r.formatNotification("assistant_message", ControlState{}); msg != "" {
		t.Fatalf("assistant_message empty = %q, want empty", msg)
	}
	if msg := r.formatNotification("activity", ControlState{Busy: true}); msg != "" {
		t.Fatalf("activity should not push: %q", msg)
	}
	if msg := r.formatNotification("info", ControlState{InfoMessage: "something happened"}); !strings.Contains(msg, "ℹ️") {
		t.Fatalf("info notification = %q", msg)
	}
	if msg := r.formatNotification("info", ControlState{}); msg != "" {
		t.Fatalf("empty info = %q", msg)
	}
	if msg := r.formatNotification("toast", ControlState{ToastMessage: "careful", ToastLevel: "warn"}); !strings.Contains(msg, "🔔") {
		t.Fatalf("warn toast = %q", msg)
	}
	if msg := r.formatNotification("toast", ControlState{ToastMessage: "broken", ToastLevel: "error"}); !strings.Contains(msg, "🔔") {
		t.Fatalf("error toast = %q", msg)
	}
	if msg := r.formatNotification("toast", ControlState{ToastMessage: "just info", ToastLevel: "info"}); msg != "" {
		t.Fatalf("info toast = %q, want empty", msg)
	}
	if msg := r.formatLongRunningNotification(ControlState{Phase: "thinking", PhaseDetail: "analyzing", InternalEventsSinceLastPush: 2}); !strings.Contains(msg, "Still working") || !strings.Contains(msg, "2 internal events") || strings.Contains(msg, "thinking") {
		t.Fatalf("long_running = %q", msg)
	}
}

func TestFormatOtherNotifications(t *testing.T) {
	r := newTestRouter()
	if got := r.formatInfoNotification(ControlState{}); got != "" {
		t.Fatalf("empty info = %q", got)
	}
	if got := r.formatToastNotification(ControlState{}); got != "" {
		t.Fatalf("empty toast = %q", got)
	}
	if got := r.formatExitNotification(ControlState{Busy: true}); got == "" {
		t.Fatal("busy exit should notify")
	}
	if got := r.formatExitNotification(ControlState{Busy: false}); got != "" {
		t.Fatalf("idle exit = %q", got)
	}
}

func TestFormatTodosNotification(t *testing.T) {
	r := newTestRouter()
	got := r.formatTodosNotification(ControlState{Todos: []TodoItem{
		{ID: "1", Content: "plan work", Status: "in_progress", ActiveForm: "inspecting"},
		{ID: "2", Content: "write tests", Status: "pending"},
		{ID: "3", Content: "ship", Status: "completed"},
	}})
	for _, want := range []string{"📋 Todos:", "▶ plan work (inspecting)", "⬜ write tests", "✅ ship"} {
		if !strings.Contains(got, want) {
			t.Fatalf("todos notification missing %q in %q", want, got)
		}
	}

	if got := r.formatTodosNotification(ControlState{}); got != "📋 No todos." {
		t.Fatalf("empty todos = %q", got)
	}
}

func TestChatIDLookupHelpers(t *testing.T) {
	r := &NotificationRouter{
		mgr: newTestChordManager(&config.Config{
			IMs: []config.IMAdapterConfig{{Wechat: &config.WechatConfig{}}, {Feishu: &config.FeishuConfig{AppID: "app", AppSecret: "secret", ChatBindings: map[string]string{"feishu-config": "ws1", "feishu-two": "ws2"}}}},
			Workspaces: []config.Workspace{
				{ID: "ws1", Path: "/tmp/ws1"},
				{ID: "ws2", Path: "/tmp/ws2"},
			},
		}),
		lastKeyChatID: map[string]string{
			(processKey{workspaceID: "ws1", imType: "wechat", chatID: "wechat-chat"}).String():    "wechat-chat",
			(processKey{workspaceID: "ws1", imType: "feishu", chatID: "feishu-tracked"}).String(): "feishu-tracked",
		},
	}

	chatIDs := r.chatIDsForWorkspace("ws1")
	if got := chatIDs["wechat"]; got != "wechat-chat" {
		t.Fatalf("wechat chatID = %q", got)
	}
	if got := chatIDs["feishu"]; got != "feishu-tracked" {
		t.Fatalf("feishu chatID = %q", got)
	}
	if got := r.chatIDForAdapter("wechat"); got != "wechat-chat" {
		t.Fatalf("chatIDForAdapter wechat = %q", got)
	}
	if got := r.chatIDForAdapter("weixin"); got != "" {
		t.Fatalf("chatIDForAdapter weixin = %q", got)
	}
	if got := r.chatIDForAdapter("lark"); got != "" {
		t.Fatalf("chatIDForAdapter lark = %q", got)
	}
	if got := r.adapterTypeForChatID("wechat-chat"); got != "wechat" {
		t.Fatalf("adapterTypeForChatID tracked wechat = %q", got)
	}
	if got := r.adapterTypeForChatID("feishu-two"); got != "feishu" {
		t.Fatalf("adapterTypeForChatID config feishu = %q", got)
	}
	if got := r.adapterTypeForChatID("unknown"); got != "" {
		t.Fatalf("adapterTypeForChatID unknown = %q", got)
	}
}

func TestFindAdapterByTypeAndAvailableLoginTargets(t *testing.T) {
	wechat := &stubIMAdapter{typ: "wechat", startLoginFunc: func() (string, error) { return "https://wx-login", nil }}
	feishu := &stubIMAdapter{typ: "feishu", startLoginFunc: func() (string, error) { return "", ErrLoginNotSupported }}
	multi := &MultiAdapter{adapters: []IMAdapter{wechat, feishu}}
	r := &NotificationRouter{adapter: multi}

	if got := r.findAdapterByType("wechat"); got != wechat {
		t.Fatalf("findAdapterByType wechat failed")
	}
	if got := r.findAdapterByType("wx"); got != nil {
		t.Fatalf("findAdapterByType wx = %#v", got)
	}
	if got := r.findAdapterByType("feishu"); got != feishu {
		t.Fatalf("findAdapterByType feishu failed")
	}
	if got := r.findAdapterByType("lark"); got != nil {
		t.Fatalf("findAdapterByType lark = %#v", got)
	}
	if got := r.findAdapterByType("unknown"); got != nil {
		t.Fatalf("findAdapterByType unknown = %#v", got)
	}

	targets := r.availableLoginTargets()
	if len(targets) != 1 || targets[0] != "wechat" {
		t.Fatalf("availableLoginTargets = %v", targets)
	}
}

func TestHandleLogin(t *testing.T) {
	t.Run("no supported targets", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat"}
		r := &NotificationRouter{adapter: sender}
		r.handleLogin("chat", "")
		if got := sender.lastMessage().text; !strings.Contains(got, "No IM adapter supports login renewal") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("show usage when target missing", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat", startLoginFunc: func() (string, error) { return "https://wx-login", nil }}
		r := &NotificationRouter{adapter: sender}
		r.handleLogin("chat", "")
		if got := sender.lastMessage().text; !strings.Contains(got, "/login <platform>") || !strings.Contains(got, "wechat") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("adapter not found", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat", startLoginFunc: func() (string, error) { return "https://wx-login", nil }}
		r := &NotificationRouter{adapter: sender}
		r.handleLogin("chat", "feishu")
		if got := sender.lastMessage().text; !strings.Contains(got, "No feishu adapter found") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("login not supported", func(t *testing.T) {
		loginless := &stubIMAdapter{typ: "feishu"}
		sender := &stubIMAdapter{typ: "wechat"}
		r := &NotificationRouter{adapter: &MultiAdapter{adapters: []IMAdapter{sender, loginless}}}
		r.handleLogin("chat", "feishu")
		if got := sender.lastMessage().text; !strings.Contains(got, "Feishu does not support login renewal") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("login failure", func(t *testing.T) {
		loginErr := errors.New("boom")
		loginAdapter := &stubIMAdapter{typ: "wechat", startLoginFunc: func() (string, error) { return "", loginErr }}
		r := &NotificationRouter{adapter: loginAdapter}
		r.handleLogin("chat", "wechat")
		if got := loginAdapter.lastMessage().text; !strings.Contains(got, "Failed to get WeChat login link") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("login success", func(t *testing.T) {
		loginAdapter := &stubIMAdapter{typ: "wechat", startLoginFunc: func() (string, error) { return "https://wx-login", nil }}
		r := &NotificationRouter{adapter: loginAdapter}
		r.handleLogin("chat", "wechat")
		if got := loginAdapter.lastMessage().text; !strings.Contains(got, "https://wx-login") {
			t.Fatalf("message = %q", got)
		}
	})
}

func TestHandleSessionExpiredAndLoginResult(t *testing.T) {
	wechat := &stubIMAdapter{typ: "wechat"}
	feishu := &stubIMAdapter{typ: "feishu"}
	multi := &MultiAdapter{adapters: []IMAdapter{wechat, feishu}}
	r := &NotificationRouter{
		adapter:       multi,
		mgr:           newTestChordManager(&config.Config{IMs: []config.IMAdapterConfig{{Feishu: &config.FeishuConfig{ChatBindings: map[string]string{"feishu-chat": "ws1"}}}}, Workspaces: []config.Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}),
		lastKeyChatID: map[string]string{(processKey{workspaceID: "ws1", imType: "wechat", chatID: "wechat-chat"}).String(): "wechat-chat"},
	}

	r.HandleSessionExpired("wechat")
	if msgs := feishu.sentMessages(); len(msgs) != 1 || !strings.Contains(msgs[0].text, "/login wechat") {
		t.Fatalf("feishu messages after wechat expiry = %#v", msgs)
	}
	if got := len(wechat.sentMessages()); got != 0 {
		t.Fatalf("wechat should not receive self expiry notification, got %d", got)
	}

	r.HandleSessionExpired("feishu")
	if msgs := wechat.sentMessages(); len(msgs) == 0 || !strings.Contains(msgs[len(msgs)-1].text, "Feishu connection invalid") || strings.Contains(msgs[len(msgs)-1].text, "/login feishu") == false {
		t.Fatalf("wechat messages after feishu expiry = %#v", msgs)
	}

	r.HandleLoginResult("wechat", true, "")
	if msgs := feishu.sentMessages(); !strings.Contains(msgs[len(msgs)-1].text, "WeChat login renewed") {
		t.Fatalf("feishu messages after login success = %#v", msgs)
	}

	r.HandleLoginResult("wechat", false, "network")
	if msgs := feishu.sentMessages(); !strings.Contains(msgs[len(msgs)-1].text, "login renewal failed") {
		t.Fatalf("feishu messages after login failure = %#v", msgs)
	}
}

func TestSendTextAndBroadcastHelpers(t *testing.T) {
	r := &NotificationRouter{
		mgr:           newTestChordManager(&config.Config{IMs: []config.IMAdapterConfig{{Feishu: &config.FeishuConfig{ChatBindings: map[string]string{"feishu-config": "ws1"}}}}, Workspaces: []config.Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}),
		lastKeyChatID: map[string]string{(processKey{workspaceID: "ws1", imType: "wechat", chatID: "wechat-chat"}).String(): "wechat-chat"},
	}

	r.sendText("chat", "hello")

	single := &stubIMAdapter{typ: "wechat"}
	r.adapter = single
	r.sendText("chat", "hello")
	if got := single.lastMessage(); got.chatID != "chat" || got.text != "hello" {
		t.Fatalf("sendText got %#v", got)
	}

	r.sendTextAll("ws1", "workspace-msg")
	if got := len(single.sentMessages()); got != 2 {
		t.Fatalf("single sendTextAll count = %d", got)
	}

	wechat := &stubIMAdapter{typ: "wechat"}
	feishu := &stubIMAdapter{typ: "feishu"}
	r.adapter = &MultiAdapter{adapters: []IMAdapter{wechat, feishu}, router: r}
	r.sendTextAll("ws1", "workspace-msg")
	if got := wechat.lastMessage().chatID; got != "wechat-chat" {
		t.Fatalf("wechat broadcast chatID = %q", got)
	}
	if got := feishu.lastMessage().chatID; got != "feishu-config" {
		t.Fatalf("feishu broadcast chatID = %q", got)
	}
}

func TestBuildFeishuCardsAndButton(t *testing.T) {
	confirm := buildFeishuConfirmCard("chat-1", &ConfirmPayload{
		ToolName:      "Shell",
		ArgsJSON:      `{"command":"pwd"}`,
		RequestID:     "req-1",
		NeedsApproval: []string{"Run shell command"},
	}, feishuCardContext{WorkspaceID: "ws-1", SessionID: "session-1234567890", ProcessKey: "ws-1|feishu|chat-1"})
	header := confirm["header"].(map[string]any)
	if header["template"] != "red" || !strings.Contains(fmt.Sprint(header), "High risk") {
		t.Fatalf("confirm risk header = %v", header)
	}
	if configMap := confirm["config"].(map[string]any); configMap["update_multi"] != true {
		t.Fatalf("confirm card config = %v", configMap)
	}
	body := confirm["body"].(map[string]any)
	elements := body["elements"].([]any)
	if len(elements) < 5 {
		t.Fatalf("confirm card elements = %v", elements)
	}
	confirmText := fmt.Sprint(elements)
	for _, want := range []string{"Run shell command", "`/allow`", "`/deny [reason]`", "Workspace: `ws-1`", "Request: `req-1`"} {
		if !strings.Contains(confirmText, want) {
			t.Fatalf("confirm card missing %q: %v", want, elements)
		}
	}
	allow := elements[len(elements)-2].(map[string]any)
	behaviors := allow["behaviors"].([]any)
	value := behaviors[0].(map[string]any)["value"].(map[string]any)
	for k, want := range map[string]any{"type": "confirm", "action": "allow", "request_id": "req-1", "chat_id": "chat-1", "im_type": "feishu"} {
		if value[k] != want {
			t.Fatalf("confirm button value[%s] = %#v, want %#v in %#v", k, value[k], want, value)
		}
	}
	if strings.Contains(confirmText, "tag:action") || strings.Contains(confirmText, "\"action\"") {
		t.Fatalf("confirm card should not use legacy action tag: %v", elements)
	}

	doneConfirm := buildFeishuConfirmCard("chat-1", &ConfirmPayload{
		ToolName:  "Done",
		ArgsJSON:  `{"reason":"loop target complete","report":"## Completion status\nAll done."}`,
		RequestID: "req-done",
	}, feishuCardContext{WorkspaceID: "ws-1", SessionID: "session-1234567890", ProcessKey: "ws-1|feishu|chat-1"})
	doneHeader := doneConfirm["header"].(map[string]any)
	if doneHeader["template"] != "green" || !strings.Contains(fmt.Sprint(doneHeader), "Done completion") {
		t.Fatalf("Done confirm header = %v", doneHeader)
	}
	doneElements := doneConfirm["body"].(map[string]any)["elements"].([]any)
	doneText := fmt.Sprint(doneElements)
	for _, want := range []string{"Done requests completion", "loop target complete", "## Completion status", "`/deny <reason>`"} {
		if !strings.Contains(doneText, want) {
			t.Fatalf("Done confirm card missing %q: %v", want, doneElements)
		}
	}
	if strings.Contains(doneText, "Deny") {
		t.Fatalf("Done confirm card should not include a no-reason Deny button: %v", doneElements)
	}
	doneButton := doneElements[len(doneElements)-1].(map[string]any)
	if fmt.Sprint(doneButton["text"]) != "map[content:Finish tag:plain_text]" {
		t.Fatalf("Done confirm button = %v", doneButton["text"])
	}

	question := buildFeishuQuestionCard("chat-2", &QuestionPayload{
		Header:        "Choose",
		Question:      "Continue?",
		Options:       []string{"yes", "no"},
		OptionDetails: []string{"Proceed", "Stop"},
		DefaultAnswer: "yes",
		RequestID:     "req-2",
	}, feishuCardContext{})
	qBody := question["body"].(map[string]any)
	qElements := qBody["elements"].([]any)
	if len(qElements) < 5 {
		t.Fatalf("question card elements = %v", qElements)
	}
	questionText := fmt.Sprint(qElements)
	for _, want := range []string{"Choose: Continue?", "Proceed", "Default: yes", "`/answer 1`"} {
		if !strings.Contains(questionText, want) {
			t.Fatalf("question card missing %q: %v", want, qElements)
		}
	}
	if strings.Contains(questionText, "tag:action") || strings.Contains(questionText, "\"action\"") {
		t.Fatalf("question card should not use legacy action tag: %v", qElements)
	}

	btn := feishuCardButton("Allow", "primary", map[string]any{"request_id": "req"})
	buttonBehaviors := btn["behaviors"].([]any)
	if len(buttonBehaviors) != 1 {
		t.Fatalf("button behaviors = %v", buttonBehaviors)
	}
}

func TestProcessEnvelopeNotification(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws"}
	payload := []byte(`{"message":"blocked by missing input","reason":"blocked_error"}`)
	p.processEnvelope(&HeadlessEnvelope{Type: "notification", Payload: payload})
	state := p.State()
	if state.LastNotification == nil || state.LastNotification.Reason != "blocked_error" || state.LastNotification.Message != "blocked by missing input" {
		t.Fatalf("LastNotification = %#v", state.LastNotification)
	}
}

// ---------------------------------------------------------------------------
// processEnvelope: todos parsing ({"todos": [...]})
// ---------------------------------------------------------------------------

func TestProcessEnvelopeTodosNewFormat(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws"}
	payload := []byte(`{"todos":[{"id":"1","content":"Build X","status":"in_progress","active_form":"building X"},{"id":"2","content":"Test Y","status":"completed"}]}`)
	p.processEnvelope(&HeadlessEnvelope{Type: "todos", Payload: payload})
	state := p.State()
	if len(state.Todos) != 2 {
		t.Fatalf("expected 2 todos, got %d", len(state.Todos))
	}
	if state.Todos[0].Content != "Build X" || state.Todos[0].Status != "in_progress" {
		t.Errorf("todo[0] = %+v", state.Todos[0])
	}
	if state.Todos[1].Status != "completed" {
		t.Errorf("todo[1].status = %q, want completed", state.Todos[1].Status)
	}
}

func TestProcessEnvelopeTodosRawArrayIgnored(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws"}
	payload := []byte(`[{"id":"1","content":"Build X","status":"in_progress"}]`)
	p.processEnvelope(&HeadlessEnvelope{Type: "todos", Payload: payload})
	state := p.State()
	if state.Todos != nil {
		t.Fatalf("expected raw array payload to be ignored, got %+v", state.Todos)
	}
}

func TestProcessEnvelopeTodosEmptyWrapper(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws"}
	payload := []byte(`{"todos":[]}`)
	p.processEnvelope(&HeadlessEnvelope{Type: "todos", Payload: payload})
	state := p.State()
	if state.Todos == nil {
		t.Fatal("expected empty todos slice, got nil")
	}
	if len(state.Todos) != 0 {
		t.Fatalf("expected 0 todos, got %d", len(state.Todos))
	}
}

// ---------------------------------------------------------------------------
// processEnvelope: done_completion updates user-visible notification state
// ---------------------------------------------------------------------------

func TestProcessEnvelopeDoneCompletion(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws"}
	p.processEnvelope(&HeadlessEnvelope{Type: "done_completion", Payload: []byte(`{"call_id":"call-done","report":"All done","reason":"ready","status":"success","mode":"normal"}`)})

	state := p.State()
	if state.LastNotification == nil {
		t.Fatal("LastNotification is nil")
	}
	if state.LastNotification.Message != "All done" {
		t.Fatalf("message = %q", state.LastNotification.Message)
	}
	if state.LastNotification.Reason != "done_completion" {
		t.Fatalf("reason = %q", state.LastNotification.Reason)
	}
}

func TestWaitStatus_DeliversResponse(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws", stdin: &captureWriteCloser{}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan ControlState, 1)
	go func() {
		state, err := p.WaitStatus(ctx)
		if err != nil {
			t.Errorf("WaitStatus: %v", err)
		}
		done <- state
	}()

	// Wait until the goroutine has registered its waiter, rather than
	// relying on a fixed sleep that may be too short under load.
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		registered := len(p.statusWaiters) > 0
		p.mu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("WaitStatus did not register a waiter")
		}
		time.Sleep(time.Millisecond)
	}

	payload := []byte(`{"session_id":"s1","busy":false,"phase":"","phase_detail":"","last_error":"","last_outcome":"completed","updated_at":"2025-01-01T00:00:00Z"}`)
	p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Payload: payload})

	select {
	case state := <-done:
		if state.LastOutcome != "completed" {
			t.Errorf("LastOutcome = %q, want completed", state.LastOutcome)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitStatus did not return after status_response")
	}
}

func TestWaitStatus_TimesOutWithoutResponse(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws", stdin: &captureWriteCloser{}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := p.WaitStatus(ctx); err == nil {
		t.Fatal("expected error on context expiry, got nil")
	}
}

func TestNormalizeIMTypeAndNames(t *testing.T) {
	if got := config.NormalizeIMType(" wx "); got != "wx" {
		t.Fatalf("normalize wx = %q", got)
	}
	if got := config.NormalizeIMType("LARK"); got != "lark" {
		t.Fatalf("normalize lark = %q", got)
	}
	if got := config.NormalizeIMType("wechat"); got != "wechat" {
		t.Fatalf("NormalizeIMType wechat = %q", got)
	}
	if got := config.NormalizeIMType("feishu"); got != "feishu" {
		t.Fatalf("NormalizeIMType feishu = %q", got)
	}
	if got := imDisplayName("wechat"); got != "WeChat" {
		t.Fatalf("imDisplayName wechat = %q", got)
	}
	if got := imDisplayName("lark"); got != "lark" {
		t.Fatalf("imDisplayName lark = %q", got)
	}
}

func TestHandleChordCommandAndViews(t *testing.T) {
	newRouterAndProcess := func(state ControlState) (*NotificationRouter, *ChordProcess, *stubIMAdapter, *captureWriteCloser, string, *config.Workspace) {
		cfg := &config.Config{IMs: []config.IMAdapterConfig{{Feishu: &config.FeishuConfig{ChatBindings: map[string]string{"chat-1": "ws1"}}}}, Workspaces: []config.Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}
		mgr := newTestChordManager(cfg)
		sender := &stubIMAdapter{typ: "wechat"}
		stdin := &captureWriteCloser{}
		key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
		proc := &ChordProcess{key: key, workspaceID: "ws1", stdin: stdin, state: state}
		mgr.procs[key] = proc
		r := &NotificationRouter{mgr: mgr, adapter: sender, lastKeyChatID: make(map[string]string), expiredPending: make(map[string]expiredPendingState)}
		return r, proc, sender, stdin, key, &cfg.Workspaces[0]
	}

	t.Run("cancel sends command and ack", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "cancel"}, "wechat", nil)
		if got := sender.lastMessage().text; got != "🛑 Cancel requested." {
			t.Fatalf("message = %q", got)
		}
		if !strings.Contains(stdin.String(), `"type":"cancel"`) {
			t.Fatalf("stdin = %q", stdin.String())
		}
	})

	t.Run("confirm uses matching internal request id", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{PendingConfirm: &ConfirmPayload{RequestID: "req-1"}})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "confirm", Action: "allow", RequestID: "req-1"}, "wechat", nil)
		if got := sender.lastMessage().text; got != "✅ allowed" {
			t.Fatalf("message = %q", got)
		}
		if !strings.Contains(stdin.String(), `"request_id":"req-1"`) || !strings.Contains(stdin.String(), `"action":"allow"`) {
			t.Fatalf("stdin = %q", stdin.String())
		}
	})

	t.Run("confirm rejects mismatched internal request id", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{PendingConfirm: &ConfirmPayload{RequestID: "req-current"}})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "confirm", Action: "allow", RequestID: "req-stale"}, "wechat", nil)
		if got := sender.lastMessage().text; !strings.Contains(got, "No matching pending confirmation") {
			t.Fatalf("message = %q", got)
		}
		if got := stdin.String(); got != "" {
			t.Fatalf("stdin should be empty, got %q", got)
		}
	})

	t.Run("confirm uses current pending request id and deny reason", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{PendingConfirm: &ConfirmPayload{RequestID: "req-pending"}})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "confirm", Action: "deny", Reason: "not safe"}, "wechat", nil)
		if got := sender.lastMessage().text; got != "✅ denied: not safe" {
			t.Fatalf("message = %q", got)
		}
		out := stdin.String()
		for _, want := range []string{`"request_id":"req-pending"`, `"action":"deny"`, `"deny_reason":"not safe"`} {
			if !strings.Contains(out, want) {
				t.Fatalf("stdin missing %s in %q", want, out)
			}
		}
	})

	t.Run("plain text during Done confirm becomes deny reason", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{PendingConfirm: &ConfirmPayload{RequestID: "req-done", ToolName: "Done", ArgsJSON: `{"report":"done"}`}})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "send", Content: "Need to update docs first"}, "wechat", nil)
		if got := stdin.String(); !strings.Contains(got, `"request_id":"req-done"`) || !strings.Contains(got, `"action":"deny"`) || !strings.Contains(got, `"deny_reason":"Need to update docs first"`) {
			t.Fatalf("stdin = %q", got)
		}
		if got := sender.lastMessage().text; !strings.Contains(got, "✅ denied: Need to update docs first") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("Done deny requires reason", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{PendingConfirm: &ConfirmPayload{RequestID: "req-done", ToolName: "Done", ArgsJSON: `{"report":"done"}`}})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "confirm", Action: "deny"}, "wechat", nil)
		if got := sender.lastMessage().text; !strings.Contains(got, "Rejecting Done requires a reason") {
			t.Fatalf("message = %q", got)
		}
		if got := stdin.String(); got != "" {
			t.Fatalf("stdin should be empty, got %q", got)
		}
	})

	t.Run("confirm without pending sends follow-up instead of approval", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "confirm", Action: "allow"}, "wechat", nil)
		if got := sender.lastMessage().text; !strings.Contains(got, "sent as a follow-up message") || strings.Contains(got, "No pending confirmation") {
			t.Fatalf("message = %q", got)
		}
		out := stdin.String()
		if !strings.Contains(out, `"type":"send"`) || !strings.Contains(out, "must not be treated as an approval or denial") || strings.Contains(out, `"type":"confirm"`) {
			t.Fatalf("stdin = %q", out)
		}
	})

	t.Run("question maps numeric answers", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{PendingQuestion: &QuestionPayload{RequestID: "req-q", Options: []string{"yes", "no"}}})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "question", Answers: []string{"1"}}, "wechat", nil)
		if got := sender.lastMessage().text; got != "💬 Answered: yes" {
			t.Fatalf("message = %q", got)
		}
		if !strings.Contains(stdin.String(), `"answers":["yes"]`) {
			t.Fatalf("stdin = %q", stdin.String())
		}
	})

	t.Run("question without pending sends follow-up", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{ExpiredQuestion: &QuestionPayload{Question: "Choose env", Options: []string{"dev", "prod"}}})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "question", Answers: []string{"prod"}}, "wechat", nil)
		if got := sender.lastMessage().text; !strings.Contains(got, "sent as a follow-up message") || strings.Contains(got, "No pending question") {
			t.Fatalf("message = %q", got)
		}
		out := stdin.String()
		for _, want := range []string{`"type":"send"`, "Expired question", "Choose env", "User response", "prod"} {
			if !strings.Contains(out, want) {
				t.Fatalf("stdin missing %q in %q", want, out)
			}
		}
		if strings.Contains(out, `"type":"question"`) {
			t.Fatalf("should not send structured question response: %q", out)
		}
	})

	t.Run("plain send with pending question auto-redirects to answer", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{PendingQuestion: &QuestionPayload{RequestID: "req-auto"}})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "send", Content: "free text answer"}, "wechat", nil)
		if got := sender.lastMessage().text; got != "💬 Answered: free text answer" {
			t.Fatalf("message = %q", got)
		}
		if !strings.Contains(stdin.String(), `"type":"question"`) || !strings.Contains(stdin.String(), `"free text answer"`) {
			t.Fatalf("stdin = %q", stdin.String())
		}
	})

	t.Run("feishu plain send with pending question updates card", func(t *testing.T) {
		cfg := &config.Config{IMs: []config.IMAdapterConfig{{Feishu: &config.FeishuConfig{AppID: "app", AppSecret: "secret", ChatBindings: map[string]string{"chat-1": "ws1"}}}}, Workspaces: []config.Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}
		paths := &config.Paths{StateDir: t.TempDir(), DedupeDir: t.TempDir()}
		mgr := NewChordManager(cfg, paths)
		stdin := &captureWriteCloser{}
		key := (processKey{workspaceID: "ws1", imType: "feishu", chatID: "chat-1"}).String()
		proc := &ChordProcess{key: key, workspaceID: "ws1", stdin: stdin, state: ControlState{PendingQuestion: &QuestionPayload{RequestID: "req-auto", ToolName: "QuestionTool"}}}
		mgr.procs[key] = proc
		feishu := testFeishuAdapter(t, &config.FeishuConfig{AppID: "app", AppSecret: "secret", ChatBindings: map[string]string{"chat-1": "ws1"}})
		defer feishu.dedupe.Close()
		var patchedPath string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/open-apis/auth/v3/app_access_token/internal":
				_, _ = w.Write([]byte(`{"code":0,"msg":"ok","app_access_token":"token","expire":7200}`))
			case "/open-apis/im/v1/messages/om_sent_1":
				patchedPath = r.URL.Path
				_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
			case "/open-apis/im/v1/messages":
				_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_text_1"}}`))
			default:
				t.Fatalf("unexpected path: %s", r.URL.Path)
			}
		}))
		defer server.Close()
		feishu.httpClient = server.Client()
		oldBaseURL := feishuOpenBaseURL
		feishuOpenBaseURL = server.URL
		defer func() { feishuOpenBaseURL = oldBaseURL }()
		r := &NotificationRouter{mgr: mgr, adapter: feishu, lastKeyChatID: make(map[string]string), expiredPending: make(map[string]expiredPendingState), cardHandles: make(map[string]InteractiveCardHandle)}
		r.recordCardHandle(key, "question", "req-auto", &InteractiveCardHandle{MessageID: "om_sent_1"})
		msg := IncomingMessage{IMType: "feishu", ChatID: "chat-1", SenderID: "ou_owner", Text: "free text answer"}
		r.handleChordCommand(&cfg.Workspaces[0], "chat-1", IMCommand{Type: "send", Content: "free text answer"}, "feishu", &msg)
		if patchedPath != "/open-apis/im/v1/messages/om_sent_1" {
			t.Fatalf("patched path = %q", patchedPath)
		}
		if !strings.Contains(stdin.String(), `"type":"question"`) || !strings.Contains(stdin.String(), `"free text answer"`) {
			t.Fatalf("stdin = %q", stdin.String())
		}
	})

	t.Run("send blocks local-only slash commands", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "send", Content: "/model"}, "wechat", nil)
		if got := sender.lastMessage().text; !strings.Contains(got, "only available in local TUI") {
			t.Fatalf("message = %q", got)
		}
		if got := stdin.String(); got != "" {
			t.Fatalf("stdin should be empty, got %q", got)
		}
	})

	t.Run("send writes user message without auxiliary status request", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "send", Content: "hello"}, "wechat", nil)
		if len(sender.sentMessages()) != 0 {
			t.Fatalf("unexpected notification messages: %#v", sender.sentMessages())
		}
		out := stdin.String()
		if !strings.Contains(out, `"type":"send"`) || !strings.Contains(out, `"content":"hello"`) {
			t.Fatalf("stdin = %q", out)
		}
		if strings.Contains(out, `"type":"status"`) {
			t.Fatalf("send should not piggyback a status command, stdin = %q", out)
		}
	})

	t.Run("unknown command type warns", func(t *testing.T) {
		r, _, sender, _, _, ws := newRouterAndProcess(ControlState{})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "mystery"}, "wechat", nil)
		if got := sender.lastMessage().text; !strings.Contains(got, "Unknown command") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("handleCurrent without alive process shows idle binding", func(t *testing.T) {
		r, proc, sender, _, _, ws := newRouterAndProcess(ControlState{})
		proc.cmd = nil
		r.handleCurrent(ws, "chat-1", "wechat")
		if got := sender.lastMessage().text; !strings.Contains(got, "⏸️ Idle") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("handleCurrent with alive process shows state", func(t *testing.T) {
		r, proc, sender, _, _, ws := newRouterAndProcess(ControlState{Busy: true, SessionID: "sess-1"})
		proc.cmd = &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}
		r.handleCurrent(ws, "chat-1", "wechat")
		if got := sender.lastMessage().text; !strings.Contains(got, "sess-1") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("handleTodos without active process", func(t *testing.T) {
		r, proc, sender, _, _, ws := newRouterAndProcess(ControlState{})
		proc.cmd = nil
		r.handleTodos(ws, "chat-1", "wechat")
		if got := sender.lastMessage().text; got != "⏸️ No active session." {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("handleTodos with empty list", func(t *testing.T) {
		r, proc, sender, _, _, ws := newRouterAndProcess(ControlState{})
		proc.cmd = &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}
		r.handleTodos(ws, "chat-1", "wechat")
		if got := sender.lastMessage().text; got != "📋 No todos." {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("handleTodos renders items", func(t *testing.T) {
		r, proc, sender, _, _, ws := newRouterAndProcess(ControlState{Todos: []TodoItem{{Content: "one", Status: "pending"}, {Content: "two", Status: "in_progress", ActiveForm: "doing two"}, {Content: "three", Status: "completed"}, {Content: "four", Status: "cancelled"}}})
		proc.cmd = &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}
		r.handleTodos(ws, "chat-1", "wechat")
		msg := sender.lastMessage().text
		for _, want := range []string{"📋 Todos:", "⬜ one", "▶ two (doing two)", "✅ three", "❌ four"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("expected %q in %q", want, msg)
			}
		}
	})
}

func TestFeishuCardRiskLevelsAndQuestionFallbackPolicy(t *testing.T) {
	for _, tt := range []struct {
		tool     string
		template string
		title    string
	}{
		{tool: "Delete", template: "red", title: "High risk"},
		{tool: "Read", template: "blue", title: "Low risk"},
		{tool: "Grep", template: "blue", title: "Low risk"},
		{tool: "Glob", template: "blue", title: "Low risk"},
		{tool: "Lsp", template: "blue", title: "Low risk"},
		{tool: "Write", template: "orange", title: "Medium risk"},
	} {
		t.Run(tt.tool, func(t *testing.T) {
			card := buildFeishuConfirmCard("chat", &ConfirmPayload{ToolName: tt.tool, RequestID: "req"}, feishuCardContext{})
			header := card["header"].(map[string]any)
			if header["template"] != tt.template || !strings.Contains(fmt.Sprint(header), tt.title) {
				t.Fatalf("header = %v", header)
			}
		})
	}

	if !shouldSendFeishuQuestionCard(&QuestionPayload{Options: []string{"1", "2", "3", "4", "5", "6"}}) {
		t.Fatal("short 6-option single-select should use a card")
	}
	if shouldSendFeishuQuestionCard(&QuestionPayload{Multiple: true, Options: []string{"1", "2"}}) {
		t.Fatal("multi-select should fallback to text")
	}
	if shouldSendFeishuQuestionCard(&QuestionPayload{}) {
		t.Fatal("free-answer question should fallback to text in this implementation")
	}
	if shouldSendFeishuQuestionCard(&QuestionPayload{Options: []string{"1", "2", "3", "4", "5", "this option label is intentionally too long for mobile buttons"}}) {
		t.Fatal("long option label should fallback to text when there are more than five options")
	}
	if shouldSendFeishuQuestionCard(&QuestionPayload{Options: []string{"this option label is intentionally too long for mobile buttons", "no"}}) {
		t.Fatal("long option label should fallback to text even when there are at most five options")
	}
	if shouldSendFeishuQuestionCard(&QuestionPayload{Options: []string{"yes", "no"}, OptionDetails: []string{"this detail is intentionally made very long so the interactive card would become too tall on mobile clients and should fallback to plain text instead of rendering a card", "short"}}) {
		t.Fatal("long option detail should fallback to text")
	}
}

func TestSummarizeToolArgsTruncatesLongPathsAndURLs(t *testing.T) {
	longPath := "/" + strings.Repeat("deep/", 60) + "file.txt"
	for _, tt := range []struct {
		name     string
		tool     string
		argsJSON string
		prefix   string
	}{
		{name: "write path", tool: "Write", argsJSON: fmt.Sprintf(`{"path":%q}`, longPath), prefix: "📝 "},
		{name: "delete path", tool: "Delete", argsJSON: fmt.Sprintf(`{"paths":[%q]}`, longPath), prefix: "🗑️ "},
		{name: "read path", tool: "Read", argsJSON: fmt.Sprintf(`{"path":%q}`, longPath), prefix: "📖 "},
		{name: "glob pattern", tool: "Glob", argsJSON: fmt.Sprintf(`{"pattern":%q}`, strings.Repeat("src/**/", 40)+"*.go"), prefix: "🔍 "},
		{name: "webfetch url", tool: "WebFetch", argsJSON: fmt.Sprintf(`{"url":%q}`, "https://example.com/"+strings.Repeat("segment/", 40)+"?"+strings.Repeat("q=1&", 30)), prefix: "🌐 "},
		{name: "lsp path", tool: "Lsp", argsJSON: fmt.Sprintf(`{"operation":"definition","path":%q}`, longPath), prefix: "🔎 "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			summary := summarizeToolArgs(tt.tool, tt.argsJSON)
			if !strings.HasPrefix(summary, tt.prefix) {
				t.Fatalf("summary prefix = %q, want %q", summary, tt.prefix)
			}
			if !strings.Contains(summary, "…") {
				t.Fatalf("summary should be truncated: %q", summary)
			}
		})
	}
}

func TestBuildFeishuQuestionCardListsPlainOptions(t *testing.T) {
	card := buildFeishuQuestionCard("chat", &QuestionPayload{Question: "Pick", Options: []string{"yes", "no"}, RequestID: "req"}, feishuCardContext{})
	text := fmt.Sprint(card["body"])
	for _, want := range []string{"1. yes", "2. no"} {
		if !strings.Contains(text, want) {
			t.Fatalf("question card missing %q: %v", want, card)
		}
	}
}

func TestFeishuCardJSONRoundTrip(t *testing.T) {
	card := buildFeishuQuestionCard("chat", &QuestionPayload{Question: "Q?", Options: []string{"A"}, RequestID: "req"}, feishuCardContext{})
	data, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	if !strings.Contains(string(data), `"schema":"2.0"`) {
		t.Fatalf("unexpected card json: %s", data)
	}
}
