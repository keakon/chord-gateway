package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
		IMs: []config.IMAdapterConfig{{Wechat: &config.WechatConfig{Owner: &config.OwnerIdentity{AccountID: "bot", UserID: "owner"}, ChatBindings: map[string]string{"chat-1": "test", "chat": "test"}}}},
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

func TestHandleLoginWithoutTargetDoesNotStartLogin(t *testing.T) {
	adapter := &stubIMAdapter{
		typ:                  "wechat",
		supportsLoginRenewal: true,
		startLoginFunc: func() (string, error) {
			return "qr-url", nil
		},
	}
	r := NewNotificationRouter(newTestChordManager(testConfig()))
	r.SetAdapter(adapter)

	r.handleLogin("chat-1", "")

	if adapter.startLoginCalls != 0 {
		t.Fatalf("StartLogin calls = %d, want 0", adapter.startLoginCalls)
	}
	if got := adapter.lastMessage().text; !strings.Contains(got, "Supported platforms: wechat") {
		t.Fatalf("message = %q", got)
	}
}

func TestHandleLoginWithTargetStartsLogin(t *testing.T) {
	adapter := &stubIMAdapter{
		typ:                  "wechat",
		supportsLoginRenewal: true,
		startLoginFunc: func() (string, error) {
			return "qr-url", nil
		},
	}
	r := NewNotificationRouter(newTestChordManager(testConfig()))
	r.SetAdapter(adapter)

	r.handleLogin("chat-1", "wechat")

	if adapter.startLoginCalls != 1 {
		t.Fatalf("StartLogin calls = %d, want 1", adapter.startLoginCalls)
	}
	if got := adapter.lastMessage().text; !strings.Contains(got, "qr-url") {
		t.Fatalf("message = %q", got)
	}
}

func TestHandleIncomingMessageNoConfigLoaded(t *testing.T) {
	sender := &stubIMAdapter{typ: "console"}
	r := &NotificationRouter{adapter: sender, lastKeyChatID: make(map[string]string)}

	r.dispatchMessage(IncomingMessage{IMType: "console", ChatID: "chat", SenderID: "user", Text: "/status"})

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
	cfg := &config.Config{IMs: []config.IMAdapterConfig{{Wechat: &config.WechatConfig{Owner: &config.OwnerIdentity{AccountID: "bot", UserID: "owner"}, ChatBindings: map[string]string{"chat-1": "missing"}}}}, Workspaces: []config.Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}
	r := &NotificationRouter{
		mgr:           newTestChordManager(cfg),
		adapter:       sender,
		lastKeyChatID: make(map[string]string),
	}

	r.dispatchMessage(IncomingMessage{IMType: "wechat", ChatID: "chat-1", SenderID: "chat-1", Text: "hello"})
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

	r.dispatchMessage(IncomingMessage{IMType: "console", ChatID: "chat-1", Text: "hello"})
	if got := sender.lastMessage().text; !strings.Contains(got, "console requires exactly one workspace") {
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
			PendingConfirm: &ConfirmPayload{RequestID: "req-confirm"},
			Questions:      map[string]*QuestionPayload{"req-question": {RequestID: "req-question", Options: []string{"yes", "no"}, OptionIDs: []string{"yes", "no"}, Version: 1}},
		},
	}
	r := &NotificationRouter{mgr: mgr, adapter: sender, lastKeyChatID: make(map[string]string), expiredPending: make(map[string]expiredPendingState)}

	r.dispatchMessage(IncomingMessage{
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
	attachQuestionTestReceipts(mgr.procs[key])
	r.dispatchMessage(IncomingMessage{
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

func TestHandleHandoffCommand(t *testing.T) {
	ws := &config.Workspace{ID: "ws1", Path: t.TempDir()}
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()

	t.Run("accept with agent and pool", func(t *testing.T) {
		mgr := newTestChordManager(&config.Config{Workspaces: []config.Workspace{*ws}})
		sender := &stubIMAdapter{typ: "wechat"}
		r := NewNotificationRouter(mgr)
		r.SetAdapter(sender)
		stdin := &captureWriteCloser{}
		proc := &ChordProcess{key: key, workspaceID: "ws1", stdin: stdin}
		proc.state.PendingHandoff = &HandoffPayload{RequestID: "handoff-1", Agents: []HandoffAgentOption{{Name: "builder", Default: true}}}
		attachQuestionTestReceipts(proc)
		mgr.procs[key] = proc
		defer r.stopReminder(key)

		r.handleHandoffCommand(ws, "chat-1", IMCommand{Type: "handoff", Action: "accept", Agent: "reviewer", Pool: "fast"}, key, proc)

		var sent map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdin.String())), &sent); err != nil {
			t.Fatalf("handoff command JSON = %q: %v", stdin.String(), err)
		}
		if sent["type"] != "handoff" || sent["request_id"] != "handoff-1" || sent["action"] != "accept" || sent["agent"] != "reviewer" || sent["pool"] != "fast" {
			t.Fatalf("handoff command = %#v", sent)
		}
		if got := sender.lastMessage().text; got != "✅ handoff accepted: reviewer (fast)" {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("accept default agent", func(t *testing.T) {
		mgr := newTestChordManager(&config.Config{Workspaces: []config.Workspace{*ws}})
		sender := &stubIMAdapter{typ: "wechat"}
		r := NewNotificationRouter(mgr)
		r.SetAdapter(sender)
		stdin := &captureWriteCloser{}
		proc := &ChordProcess{key: key, workspaceID: "ws1", stdin: stdin}
		proc.state.PendingHandoff = &HandoffPayload{RequestID: "handoff-2", Agents: []HandoffAgentOption{{Name: "reviewer"}, {Name: "builder", Default: true}}}
		attachQuestionTestReceipts(proc)
		mgr.procs[key] = proc
		defer r.stopReminder(key)

		r.handleHandoffCommand(ws, "chat-1", IMCommand{Type: "handoff", Action: "accept"}, key, proc)

		var sent map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdin.String())), &sent); err != nil {
			t.Fatalf("handoff command JSON = %q: %v", stdin.String(), err)
		}
		if _, ok := sent["agent"]; ok {
			t.Fatalf("default handoff command should omit agent, got %#v", sent)
		}
		if got := sender.lastMessage().text; got != "✅ handoff accepted: builder" {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("deny with reason", func(t *testing.T) {
		mgr := newTestChordManager(&config.Config{Workspaces: []config.Workspace{*ws}})
		sender := &stubIMAdapter{typ: "wechat"}
		r := NewNotificationRouter(mgr)
		r.SetAdapter(sender)
		stdin := &captureWriteCloser{}
		proc := &ChordProcess{key: key, workspaceID: "ws1", stdin: stdin}
		proc.state.PendingHandoff = &HandoffPayload{RequestID: "handoff-3"}
		attachQuestionTestReceipts(proc)
		mgr.procs[key] = proc
		defer r.stopReminder(key)

		r.handleHandoffCommand(ws, "chat-1", IMCommand{Type: "handoff", Action: "deny", Reason: "revise plan"}, key, proc)

		var sent map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdin.String())), &sent); err != nil {
			t.Fatalf("handoff command JSON = %q: %v", stdin.String(), err)
		}
		if sent["action"] != "deny" || sent["deny_reason"] != "revise plan" {
			t.Fatalf("handoff deny command = %#v", sent)
		}
		if got := sender.lastMessage().text; got != "✅ handoff denied: revise plan" {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("no pending handoff", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat"}
		r := &NotificationRouter{adapter: sender}
		proc := &ChordProcess{}

		r.handleHandoffCommand(ws, "chat-1", IMCommand{Type: "handoff", Action: "accept"}, key, proc)

		if got := sender.lastMessage().text; got != "⚠️ No pending handoff to respond to." {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("send failure", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat"}
		r := &NotificationRouter{adapter: sender}
		proc := &ChordProcess{stdin: &errorWriteCloser{err: errors.New("closed pipe")}}
		proc.state.PendingHandoff = &HandoffPayload{RequestID: "handoff-4"}

		r.handleHandoffCommand(ws, "chat-1", IMCommand{Type: "handoff", Action: "accept"}, key, proc)

		if got := sender.lastMessage().text; got != "❌ Failed to send handoff response." {
			t.Fatalf("message = %q", got)
		}
	})
}

func TestHandleLocalShellCommand(t *testing.T) {
	ws := &config.Workspace{ID: "ws1", Path: t.TempDir()}
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()

	t.Run("sends local shell command", func(t *testing.T) {
		mgr := newTestChordManager(&config.Config{Workspaces: []config.Workspace{*ws}})
		sender := &stubIMAdapter{typ: "wechat"}
		r := NewNotificationRouter(mgr)
		r.SetAdapter(sender)
		stdin := &captureWriteCloser{}
		proc := &ChordProcess{key: key, workspaceID: "ws1", stdin: stdin}
		attachQuestionTestReceipts(proc)
		mgr.procs[key] = proc
		defer r.stopReminder(key)

		r.handleLocalShellCommand(ws, "chat-1", IMCommand{Type: "local_shell", Content: " pwd "}, key, proc)

		var sent map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdin.String())), &sent); err != nil {
			t.Fatalf("local_shell command JSON = %q: %v", stdin.String(), err)
		}
		if sent["type"] != "local_shell" || sent["command"] != "pwd" {
			t.Fatalf("local_shell command = %#v", sent)
		}
		if got := sender.sentMessages(); len(got) != 0 {
			t.Fatalf("successful local shell should not send immediate message, got %#v", got)
		}
	})

	t.Run("empty command", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat"}
		r := &NotificationRouter{adapter: sender}

		r.handleLocalShellCommand(ws, "chat-1", IMCommand{Type: "local_shell", Content: "   "}, key, &ChordProcess{})

		if got := sender.lastMessage().text; got != "⚠️ Empty command after !" {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("send failure", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat"}
		r := &NotificationRouter{adapter: sender}
		proc := &ChordProcess{stdin: &errorWriteCloser{err: errors.New("closed pipe")}}

		r.handleLocalShellCommand(ws, "chat-1", IMCommand{Type: "local_shell", Content: "pwd"}, key, proc)

		if got := sender.lastMessage().text; got != "❌ Failed to run local shell command." {
			t.Fatalf("message = %q", got)
		}
	})
}

func TestHandleSendRetriesInFreshSessionWhenPinnedProcessUnavailable(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	stdinFile := filepath.Join(dir, "stdin.txt")
	fakeChord := filepath.Join(dir, "fake-chord.sh")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + shellQuote(argsFile) + "\n" +
		"while IFS= read -r line; do printf '%s\\n' \"$line\" >> " + shellQuote(stdinFile) + "; done\n"
	if err := os.WriteFile(fakeChord, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake chord: %v", err)
	}
	ws := &config.Workspace{ID: "ws1", Path: t.TempDir()}
	cfg := &config.Config{ChordPath: fakeChord, Workspaces: []config.Workspace{*ws}}
	mgr := NewChordManager(cfg, &config.Paths{StateDir: t.TempDir()})
	sender := &stubIMAdapter{typ: "wechat"}
	r := NewNotificationRouter(mgr)
	r.SetAdapter(sender)
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	if err := mgr.pins.Set(key, "old-session"); err != nil {
		t.Fatalf("set old pin: %v", err)
	}
	oldProc := &ChordProcess{key: key, workspaceID: "ws1", stdin: &errorWriteCloser{err: errors.New("closed pipe")}}
	mgr.procs[key] = oldProc

	r.handleSendCommand(ws, "chat-1", IMCommand{Type: "send", Content: "hello after cleanup"}, IncomingMessage{IMType: "wechat", ChatID: "chat-1"}, key, oldProc)
	defer mgr.StopAll(time.Second)

	if got := mgr.pins.Get(key); got != "" {
		t.Fatalf("pin = %q, want cleared", got)
	}
	if got := sender.lastMessage().text; !strings.Contains(got, "Previous Chord session was not found or is busy") {
		t.Fatalf("message = %q", got)
	}
	args := readFakeChordArgs(t, argsFile)
	if strings.Contains(args, "--resume") || strings.Contains(args, "old-session") {
		t.Fatalf("fallback spawn args = %q, want no pinned resume", args)
	}
	var stdin string
	for i := 0; i < 200; i++ {
		data, err := os.ReadFile(stdinFile)
		if err == nil && strings.Contains(string(data), "hello after cleanup") {
			stdin = string(data)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(stdin, "hello after cleanup") {
		t.Fatalf("fresh session stdin = %q, want original message", stdin)
	}
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

	t.Run("subagent lifecycle events route to chat", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat"}
		r := &NotificationRouter{adapter: sender}
		key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
		r.HandleChordEvent(key, "agent_started", ControlState{LastAgentStarted: &AgentStartedPayload{AgentID: "agent-1", TaskID: "adhoc-1", AgentType: "reviewer", Description: "Review changes"}})
		r.HandleChordEvent(key, "agent_notify", ControlState{LastAgentNotify: &AgentNotifyPayload{AgentID: "agent-1", TaskID: "adhoc-1", AgentType: "reviewer", Kind: "progress", Message: "Tests pass"}})
		r.HandleChordEvent(key, "agent_done", ControlState{LastAgentDone: &AgentDonePayload{AgentID: "agent-1", TaskID: "adhoc-1", AgentType: "reviewer", Summary: "Reviewed"}})
		messages := sender.sentMessages()
		if len(messages) != 3 {
			t.Fatalf("messages = %#v", messages)
		}
		for i, want := range []string{"Delegated reviewer · adhoc-1", "reviewer · adhoc-1 · progress", "reviewer · adhoc-1 completed"} {
			if messages[i].chatID != "chat-1" || !strings.Contains(messages[i].text, want) {
				t.Fatalf("message[%d] = %#v, want %q", i, messages[i], want)
			}
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
		mgr:     newTestChordManager(&config.Config{IMs: []config.IMAdapterConfig{{Feishu: &config.FeishuConfig{Owner: &config.OwnerIdentity{AccountID: "app", UserID: "owner"}, ChatBindings: map[string]string{"feishu-chat": "ws1"}}}, {Wechat: &config.WechatConfig{Owner: &config.OwnerIdentity{AccountID: "bot", UserID: "owner"}, ChatBindings: map[string]string{"wechat-chat": "ws1"}}}}, Workspaces: []config.Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}),
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

func TestHandleNewSendsCommandToActiveProcess(t *testing.T) {
	stateDir := t.TempDir()
	workspaceDir := t.TempDir()
	ws := &config.Workspace{ID: "ws1", Path: workspaceDir}
	cfg := &config.Config{ChordPath: makeFakeChordBinary(t, ""), Workspaces: []config.Workspace{*ws}}
	mgr := NewChordManager(cfg, &config.Paths{StateDir: stateDir})
	sender := &stubIMAdapter{typ: "wechat"}
	r := &NotificationRouter{mgr: mgr, adapter: sender}
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	stdin := &captureWriteCloser{}
	mgr.procs[key] = &ChordProcess{key: key, workspaceID: "ws1", stdin: stdin}
	if err := mgr.pins.Set(key, "old-session"); err != nil {
		t.Fatalf("set old pin: %v", err)
	}

	r.handleNew(ws, "chat-1", "wechat")

	if got := mgr.pins.Get(key); got != "old-session" {
		t.Fatalf("pin = %q, want retained until session_switched", got)
	}
	if got := sender.lastMessage().text; got != "🆕 /new sent to chord process." {
		t.Fatalf("message = %q", got)
	}
	if !strings.Contains(stdin.String(), `"/new"`) {
		t.Fatalf("stdin should contain /new command, got %q", stdin.String())
	}
}

func TestHandleNewStartsFreshSessionAndClearsPinWhenUnavailable(t *testing.T) {
	fakeChord, argsFile := makeFakeChordBinaryWithArgsFile(t, "")
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

	r.handleNew(ws, "chat-1", "wechat")
	defer mgr.StopAll(time.Second)

	if got := mgr.pins.Get(key); got != "" {
		t.Fatalf("pin = %q, want cleared", got)
	}
	if got := sender.lastMessage().text; got != "🆕 New session started." {
		t.Fatalf("message = %q", got)
	}
	args := readFakeChordArgs(t, argsFile)
	if strings.Contains(args, "--resume") || strings.Contains(args, "old-session") || strings.Contains(args, "legacy-session") {
		t.Fatalf("/new spawn args = %q, want no pinned resume", args)
	}
}

func TestHandleResumePinsSessionAndStartsWithResumeArgs(t *testing.T) {
	fakeChord, argsFile := makeFakeChordBinaryWithArgsFile(t, "ready:session-123")
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

func TestHandleResumeFailureClearsPinAndReports(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	fakeChord := filepath.Join(dir, "fake-chord.sh")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + shellQuote(argsFile) + "\n" +
		"exit 1\n"
	if err := os.WriteFile(fakeChord, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake chord: %v", err)
	}
	workspaceDir := t.TempDir()
	ws := &config.Workspace{ID: "ws1", Path: workspaceDir}
	cfg := &config.Config{ChordPath: fakeChord, Workspaces: []config.Workspace{*ws}}
	mgr := NewChordManager(cfg, &config.Paths{StateDir: t.TempDir()})
	sender := &stubIMAdapter{typ: "wechat"}
	r := &NotificationRouter{mgr: mgr, adapter: sender}
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()

	r.handleResume(ws, "chat-1", "missing-session", "wechat")

	if got := mgr.pins.Get(key); got != "" {
		t.Fatalf("pin = %q, want cleared", got)
	}
	if got := sender.lastMessage().text; got != "❌ Failed to resume session missing-session. It may not exist or may be busy." {
		t.Fatalf("message = %q", got)
	}
	if got := mgr.GetProcessForKey(key); got != nil {
		t.Fatalf("failed resume process should be removed, got %#v", got)
	}
}

func TestHandleResumeDoesNotSpawnProcessForBusyCheck(t *testing.T) {
	fakeChord, argsFile := makeFakeChordBinaryWithArgsFile(t, "ready:session-123")
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
		if got := sender.lastMessage().text; got != "❌ Failed to start new session." {
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

func TestHandleTodosNoActiveSession(t *testing.T) {
	ws := &config.Workspace{ID: "ws1", Path: t.TempDir()}
	cfg := &config.Config{Workspaces: nil}
	mgr := newTestChordManager(cfg)

	sender := &stubIMAdapter{typ: "wechat"}
	r := &NotificationRouter{mgr: mgr, adapter: sender}
	r.handleTodos(ws, "chat-1", "wechat")
	if got := sender.lastMessage().text; got != "⏸️ No active session." {
		t.Fatalf("todos message = %q", got)
	}
}

// ---------------------------------------------------------------------------
// parseIMCommand
// ---------------------------------------------------------------------------

func TestParseIMCommand(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		wantType      string
		wantContent   string
		wantRequestID string
		wantAction    string
		wantAnswers   []string
		wantSessionID string
		wantReason    string
		wantAgent     string
		wantPool      string
		wantInvalid   bool
	}{
		{name: "status", input: "/status", wantType: "status"},
		{name: "summary", input: "/summary", wantType: "send", wantContent: "/summary"},
		{name: "cancel", input: "/cancel", wantType: "cancel"},
		{name: "allow with request_id", input: "/allow req-1", wantType: "confirm", wantAction: "allow", wantRequestID: "req-1"},
		{name: "deny with reason", input: "/deny not good", wantType: "confirm", wantAction: "deny", wantReason: "not good"},
		{name: "answer", input: "/answer yes", wantType: "question", wantAnswers: []string{"yes"}},
		{name: "handoff default", input: "/handoff", wantType: "handoff", wantAction: "accept"},
		{name: "handoff agent", input: "/handoff builder", wantType: "handoff", wantAction: "accept", wantAgent: "builder"},
		{name: "handoff agent and pool", input: "/handoff builder fast", wantType: "handoff", wantAction: "accept", wantAgent: "builder", wantPool: "fast"},
		{name: "handoff quoted pool", input: "/handoff builder \"fast pool\"", wantType: "handoff", wantAction: "accept", wantAgent: "builder", wantPool: "fast pool"},
		{name: "handoff extra argument", input: "/handoff builder fast extra", wantType: "handoff", wantAction: "accept", wantInvalid: true},
		{name: "handoff deny", input: "/handoff-deny revise plan", wantType: "handoff", wantAction: "deny", wantReason: "revise plan"},
		{name: "role menu", input: "/role", wantType: "role", wantContent: ""},
		{name: "role numeric", input: "/role 2", wantType: "role", wantContent: "2"},
		{name: "role name", input: "/role planner", wantType: "role", wantContent: "planner"},
		{name: "new", input: "/new", wantType: "new"},
		{name: "resume with session_id", input: "/resume 123", wantType: "resume", wantSessionID: "123"},
		{name: "sessions", input: "/sessions", wantType: "sessions"},
		{name: "todos", input: "/todos", wantType: "todos"},
		{name: "plain text becomes send", input: "hello world", wantType: "send", wantContent: "hello world"},
		{name: "bang local shell", input: "!pwd", wantType: "local_shell", wantContent: "pwd"},
		{name: "fullwidth bang local shell", input: "！go test ./...", wantType: "local_shell", wantContent: "go test ./..."},
		{name: "empty bang local shell", input: "!   ", wantType: "local_shell", wantContent: ""},
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
			if got.Invalid != tt.wantInvalid {
				t.Errorf("Invalid = %v, want %v", got.Invalid, tt.wantInvalid)
			}
			if got.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tt.wantReason)
			}
			if got.Agent != tt.wantAgent {
				t.Errorf("Agent = %q, want %q", got.Agent, tt.wantAgent)
			}
			if got.Pool != tt.wantPool {
				t.Errorf("Pool = %q, want %q", got.Pool, tt.wantPool)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// formatStatus
// ---------------------------------------------------------------------------

func TestFormatStatusShowsLastCompactionOutcome(t *testing.T) {
	s := formatBindingStatus(nil, "", "", ControlState{LastCompaction: &CompactionStatusPayload{Status: "skipped", Trigger: "model_driven", Reason: "projected savings too small"}})
	if !strings.Contains(s, "Last context checkpoint") || !strings.Contains(s, "skipped") || !strings.Contains(s, "model-driven") || !strings.Contains(s, "projected savings") {
		t.Fatalf("status with compaction outcome = %q, want checkpoint line", s)
	}
	// A started-only compaction (no terminal outcome yet) is not shown.
	s = formatBindingStatus(nil, "", "", ControlState{LastCompaction: &CompactionStatusPayload{Status: "started", Trigger: "model_driven"}})
	if strings.Contains(s, "Last context checkpoint") {
		t.Fatalf("started-only compaction must not be shown in status, got %q", s)
	}
}

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
			Busy:      false,
			Questions: map[string]*QuestionPayload{"r2": {Question: "Continue?", RequestID: "r2", Version: 1}},
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

	t.Run("current role shows theater masks", func(t *testing.T) {
		s := formatBindingStatus(nil, "", "", ControlState{CurrentRole: "engineer"})
		if !containsEmoji(s, "🎭") || !strings.Contains(s, "Role: engineer") {
			t.Errorf("expected 🎭 with current role, got: %s", s)
		}
		s = formatBindingStatus(nil, "", "", ControlState{})
		if containsEmoji(s, "🎭") {
			t.Errorf("expected no 🎭 without current role, got: %s", s)
		}
	})
}

func containsEmoji(s, emoji string) bool {
	return strings.Contains(s, emoji)
}

func TestConfiguredHeadlessSubscribeEvents(t *testing.T) {
	got := configuredHeadlessSubscribeEvents(&config.Config{})
	wantCore := []string{"session_transfer_preparing", "session_transfer_status", "session_transfer_saved", "session_transfer_released", "activity", "assistant_message", "confirm_request", "question_request", "question_updated", "question_resolved", "handoff_request", "handoff_cancelled", "idle", "error", "notification", "done_completion", "local_shell_result", "agent_done", "role_change", "compaction_status", "session_switched", "background_result", imageResultEvent, mcpStatusChangedEvent}
	if strings.Join(got, ",") != strings.Join(wantCore, ",") {
		t.Fatalf("default subscribe events = %v, want %v", got, wantCore)
	}

	got = configuredHeadlessSubscribeEvents(&config.Config{EventVisibility: config.EventVisibility{
		AgentStarted: true,
		AgentNotify:  true,
		Info:         true,
		Toast:        true,
		Todos:        true,
	}})
	wantAll := []string{"session_transfer_preparing", "session_transfer_status", "session_transfer_saved", "session_transfer_released", "activity", "assistant_message", "confirm_request", "question_request", "question_updated", "question_resolved", "handoff_request", "handoff_cancelled", "idle", "error", "notification", "done_completion", "local_shell_result", "agent_done", "role_change", "compaction_status", "session_switched", "background_result", imageResultEvent, mcpStatusChangedEvent, "agent_started", "agent_notify", "info", "toast", "todos"}
	if strings.Join(got, ",") != strings.Join(wantAll, ",") {
		t.Fatalf("configured subscribe events = %v, want %v", got, wantAll)
	}
}

// context_notice is opt-in: it is the only event whose chat delivery is a
// product decision rather than a correctness requirement, so it must stay out
// of the default set and appear exactly once when enabled.
func TestConfiguredHeadlessSubscribeEventsContextNoticeIsOptIn(t *testing.T) {
	got := configuredHeadlessSubscribeEvents(&config.Config{})
	if slices.Contains(got, "context_notice") {
		t.Fatalf("context_notice must not be subscribed by default, got %v", got)
	}

	got = configuredHeadlessSubscribeEvents(&config.Config{EventVisibility: config.EventVisibility{ContextNotice: true}})
	want := append([]string{"session_transfer_preparing", "session_transfer_status", "session_transfer_saved", "session_transfer_released", "activity", "assistant_message", "confirm_request", "question_request", "question_updated", "question_resolved", "handoff_request", "handoff_cancelled", "idle", "error", "notification", "done_completion", "local_shell_result", "agent_done", "role_change", "compaction_status", "session_switched", "background_result", imageResultEvent, mcpStatusChangedEvent}, "context_notice")
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("context_notice-only subscribe events = %v, want %v", got, want)
	}
}

// background_result is subscribed by default because finished job results have
// no other delivery channel; an explicit false opts that push out.
func TestConfiguredHeadlessSubscribeEventsBackgroundResultOptOut(t *testing.T) {
	base := configuredHeadlessSubscribeEvents(&config.Config{})
	if !slices.Contains(base, "background_result") {
		t.Fatalf("background_result must be subscribed by default, got %v", base)
	}

	disabled := false
	got := configuredHeadlessSubscribeEvents(&config.Config{EventVisibility: config.EventVisibility{BackgroundResult: &disabled}})
	want := slices.DeleteFunc(slices.Clone(base), func(event string) bool { return event == "background_result" })
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("opt-out subscribe events = %v, want %v", got, want)
	}

	enabled := true
	got = configuredHeadlessSubscribeEvents(&config.Config{EventVisibility: config.EventVisibility{BackgroundResult: &enabled}})
	if strings.Join(got, ",") != strings.Join(base, ",") {
		t.Fatalf("explicit true subscribe events = %v, want %v", got, base)
	}
}

func TestRecordCardHandlePrunesExpiredEntries(t *testing.T) {
	r := &NotificationRouter{cardHandles: make(map[string]cardHandleEntry)}
	key := (processKey{workspaceID: "ws1", imType: "feishu", chatID: "chat-1"}).String()

	staleKey := cardHandleKey(key, "confirm", "req-stale")
	r.cardHandles[staleKey] = cardHandleEntry{handle: InteractiveCardHandle{MessageID: "om_stale"}, recordedAt: time.Now().Add(-interactiveCardHandleTTL - time.Minute)}
	freshKey := cardHandleKey(key, "question", "req-fresh")
	r.cardHandles[freshKey] = cardHandleEntry{handle: InteractiveCardHandle{MessageID: "om_fresh"}, recordedAt: time.Now().Add(-time.Minute)}

	r.recordCardHandle(key, "confirm", "req-new", &InteractiveCardHandle{MessageID: "om_new"})

	if _, ok := r.cardHandles[staleKey]; ok {
		t.Fatal("expired card handle was not pruned")
	}
	if _, ok := r.cardHandles[freshKey]; !ok {
		t.Fatal("fresh card handle should be kept")
	}
	if handle, ok := r.takeCardHandle(key, "confirm", "req-new"); !ok || handle.MessageID != "om_new" {
		t.Fatalf("new card handle = %#v, ok=%v", handle, ok)
	}
}

func TestTakeCardHandleDropsExpiredEntries(t *testing.T) {
	r := &NotificationRouter{cardHandles: make(map[string]cardHandleEntry)}
	key := (processKey{workspaceID: "ws1", imType: "feishu", chatID: "chat-1"}).String()

	staleKey := cardHandleKey(key, "confirm", "req-stale")
	r.cardHandles[staleKey] = cardHandleEntry{handle: InteractiveCardHandle{MessageID: "om_stale"}, recordedAt: time.Now().Add(-interactiveCardHandleTTL - time.Minute)}
	if _, ok := r.takeCardHandle(key, "confirm", "req-stale"); ok {
		t.Fatal("expired card handle must not be returned")
	}
	if _, ok := r.cardHandles[staleKey]; ok {
		t.Fatal("expired card handle must be removed on take")
	}

	freshKey := cardHandleKey(key, "question", "req-fresh")
	r.cardHandles[freshKey] = cardHandleEntry{handle: InteractiveCardHandle{MessageID: "om_fresh"}, recordedAt: time.Now().Add(-time.Minute)}
	if handle, ok := r.takeCardHandle(key, "question", "req-fresh"); !ok || handle.MessageID != "om_fresh" {
		t.Fatalf("fresh card handle = %#v, ok=%v", handle, ok)
	}
}

// ---------------------------------------------------------------------------
// formatNotification
// ---------------------------------------------------------------------------

func TestFormatNotification_NotificationEvent(t *testing.T) {
	// chord emits a single notification reason today; any other value is
	// forwarded as a plain message instead of being given a wrong icon.
	tests := []struct {
		name   string
		reason string
		msg    string
		emoji  string
	}{
		{name: "user input required", reason: notificationReasonUserInputRequired, msg: "Chord: Loop requires your decision", emoji: "❓"},
		{name: "other reason", reason: "other", msg: "plain text", emoji: "plain text"},
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
	if msg := r.formatNotification("question_request", ControlState{Questions: map[string]*QuestionPayload{"q-test": {Question: "Continue?", RequestID: "q-test", Version: 1}}}); msg == "" {
		t.Fatal("question_request should push")
	}
	if msg := r.formatNotification("handoff_request", ControlState{PendingHandoff: &HandoffPayload{PlanPath: ".chord/plans/plan-001.md", PlanText: "# Plan", Agents: []HandoffAgentOption{{Name: "builder", Default: true, ModelPools: []string{"fast"}, CurrentModelPool: "fast"}}}}); !strings.Contains(msg, "# Plan") || !strings.Contains(msg, "/handoff") || !strings.Contains(msg, "builder") {
		t.Fatalf("handoff_request notification missing plan or instructions: %q", msg)
	}
	if msg := r.formatNotification("error", ControlState{LastError: "boom"}); msg != "" {
		t.Fatalf("error = %q, want empty", msg)
	}
	if msg := r.formatNotification("idle", ControlState{LastOutcome: "completed"}); msg != "✅ Chord: Ready for input" {
		t.Fatalf("idle = %q, want %q", msg, "✅ Chord: Ready for input")
	}
	if msg := r.formatNotification("agent_done", ControlState{LastAssistantAgentID: "agent-1", LastAssistantText: "Reviewed", LastAgentDone: &AgentDonePayload{AgentID: "agent-1", TaskID: "adhoc-1", AgentType: "reviewer", Summary: "Reviewed"}}); !strings.Contains(msg, "reviewer · adhoc-1 completed") || !strings.Contains(msg, "Reviewed") {
		t.Fatalf("agent_done = %q", msg)
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
		Busy:           true,
		SessionID:      "sess-1",
		Phase:          "planning",
		PhaseDetail:    "writing plan",
		LastOutcome:    "completed",
		PendingConfirm: &ConfirmPayload{ToolName: "Shell"},
		Questions:      map[string]*QuestionPayload{"q-test": {Question: "Continue?", RequestID: "q-test", Version: 1}},
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
			Questions: map[string]*QuestionPayload{"req-1": {Question: "Continue?", RequestID: "req-1", Version: 1}},
		})
		if !strings.Contains(msg, "Continue?") {
			t.Errorf("expected question text, got: %s", msg)
		}
	})

	t.Run("shows options with details", func(t *testing.T) {
		msg := r.formatQuestionNotification(ControlState{
			Questions: map[string]*QuestionPayload{"req-2": {
				Question:      "How to proceed?",
				Options:       []string{"yes", "no"},
				OptionDetails: []string{"Yes, proceed", "No, stop"},
				RequestID:     "req-2",
				OptionIDs:     []string{"yes", "no"}, Version: 1}},
		})
		if !strings.Contains(msg, "yes — Yes, proceed") {
			t.Errorf("expected option with detail, got: %s", msg)
		}
		if !strings.Contains(msg, "no — No, stop") {
			t.Errorf("expected option with detail, got: %s", msg)
		}
	})

	t.Run("shows header and multi-select without a default answer", func(t *testing.T) {
		msg := r.formatQuestionNotification(ControlState{
			Questions: map[string]*QuestionPayload{"q-test": {
				Header:    "Pick",
				Question:  "Select files",
				Options:   []string{"A", "B"},
				Multiple:  true,
				RequestID: "q-test", OptionIDs: []string{"A", "B"}, Version: 1}},
		})
		for _, want := range []string{"Pick: Select files", "multi-select"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("expected %q in %q", want, msg)
			}
		}
		if strings.Contains(msg, "Default:") {
			t.Fatalf("a question must not advertise a default answer: %q", msg)
		}
	})

	t.Run("skips option detail when same as label", func(t *testing.T) {
		msg := r.formatQuestionNotification(ControlState{
			Questions: map[string]*QuestionPayload{"q-test": {
				Question:      "Pick one",
				Options:       []string{"A", "B"},
				OptionDetails: []string{"A", "B"},
				RequestID:     "q-test", OptionIDs: []string{"A", "B"}, Version: 1}},
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
	if got := r.formatNotification("idle", ControlState{SuppressUserNotification: true}); got != "" {
		t.Fatalf("suppressed idle = %q, want empty", got)
	}
	if got := r.formatNotification("idle", ControlState{Questions: map[string]*QuestionPayload{"q-test": {Question: "Choose env", RequestID: "q-test", Version: 1, Outcome: "no_response"}}}); got != "✅ Chord: Ready for input" {
		t.Fatalf("expired question notification = %q", got)
	}
	if got := r.formatNotification("idle", ControlState{SuppressUserNotification: true, ExpiredConfirm: &ConfirmPayload{RequestID: "req-c"}}); !strings.Contains(got, "pending confirmation has expired") {
		t.Fatalf("expired confirmation should override suppression = %q", got)
	}
	if got := r.formatNotification("idle_timeout", ControlState{ExpiredConfirm: &ConfirmPayload{RequestID: "req-c"}}); !strings.Contains(got, "pending confirmation has expired") {
		t.Fatalf("expired confirm notification = %q", got)
	}
	if got := r.formatNotification("idle_timeout", ControlState{ExpiredHandoff: &HandoffPayload{RequestID: "req-h"}}); !strings.Contains(got, "pending handoff request has expired") {
		t.Fatalf("expired handoff notification = %q", got)
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
	if got := r.lookupExpiredPending(key); got.Confirm != nil {
		t.Fatalf("expired pending was not cleared by new confirm: %#v", got)
	}

	r.HandleChordEvent(key, "idle", ControlState{ExpiredHandoff: &HandoffPayload{RequestID: "old-handoff"}})
	if got := r.lookupExpiredPending(key).Handoff; got == nil || got.RequestID != "old-handoff" {
		t.Fatalf("expired handoff was not recorded: %#v", got)
	}

	r.HandleChordEvent(key, "handoff_request", ControlState{PendingHandoff: &HandoffPayload{RequestID: "new-handoff"}})
	if got := r.lookupExpiredPending(key); got.Confirm != nil || got.Handoff != nil {
		t.Fatalf("expired pending was not cleared by new handoff: %#v", got)
	}
}

func TestHandleChordEventClearsExpiredConfirmWhenChordDropsIt(t *testing.T) {
	r := &NotificationRouter{mgr: newTestChordManager(&config.Config{}), lastKeyChatID: make(map[string]string), expiredPending: make(map[string]expiredPendingState)}
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat1"}).String()
	r.HandleChordEvent(key, "idle", ControlState{ExpiredConfirm: &ConfirmPayload{RequestID: "c-1"}})
	if r.lookupExpiredPending(key).Confirm == nil {
		t.Fatal("missing expired confirmation")
	}
	r.HandleChordEvent(key, "idle", ControlState{})
	if got := r.lookupExpiredPending(key); got.Confirm != nil || got.Handoff != nil {
		t.Fatalf("stale expired pending: %#v", got)
	}
}

func TestFormatHandoffCancelledNotification(t *testing.T) {
	r := &NotificationRouter{}
	if got := r.formatNotification("handoff_cancelled", ControlState{}); got != "" {
		t.Fatalf("handoff_cancelled without a cancelled request = %q, want empty", got)
	}
	got := r.formatNotification("handoff_cancelled", ControlState{ExpiredHandoff: &HandoffPayload{RequestID: "handoff-1"}})
	if !strings.Contains(got, "cancelled") {
		t.Fatalf("handoff_cancelled notification = %q", got)
	}
}

func TestHandleChordEventHandoffCancelledNotifiesAndRecords(t *testing.T) {
	sender := &stubIMAdapter{typ: "wechat"}
	r := &NotificationRouter{adapter: sender, lastKeyChatID: make(map[string]string), expiredPending: make(map[string]expiredPendingState)}
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()

	r.HandleChordEvent(key, "handoff_cancelled", ControlState{ExpiredHandoff: &HandoffPayload{RequestID: "handoff-1"}})

	if got := sender.lastMessage(); got.chatID != "chat-1" || !strings.Contains(got.text, "cancelled") {
		t.Fatalf("cancellation message = %#v", got)
	}
	if expired := r.lookupExpiredPending(key).Handoff; expired == nil || expired.RequestID != "handoff-1" {
		t.Fatalf("cancelled handoff was not recorded: %#v", expired)
	}
}

func TestHandoffCommandAfterCancelledFallsBackToNoPending(t *testing.T) {
	ws := &config.Workspace{ID: "ws1", Path: t.TempDir()}
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	sender := &stubIMAdapter{typ: "wechat"}
	r := &NotificationRouter{adapter: sender}
	proc := &ChordProcess{key: key, workspaceID: "ws1"}

	proc.processEnvelope(&HeadlessEnvelope{Type: "handoff_request", Payload: json.RawMessage(`{"request_id":"handoff-1","plan_path":"plan.md","version":1}`)})
	proc.processEnvelope(&HeadlessEnvelope{Type: "handoff_cancelled", Payload: json.RawMessage(`{"request_id":"handoff-1","reason":"superseded","version":2}`)})

	for _, cmd := range []IMCommand{
		{Type: "handoff", Action: "accept"},
		{Type: "handoff", Action: "deny", Reason: "no longer needed"},
	} {
		r.handleHandoffCommand(ws, "chat-1", cmd, key, proc)
		if got := sender.lastMessage().text; got != "⚠️ No pending handoff to respond to." {
			t.Fatalf("action %q after cancellation = %q", cmd.Action, got)
		}
	}
}

func TestFormatNotification_AssistantInfoToastAndLongRunning(t *testing.T) {
	r := newTestRouter()
	if msg := r.formatNotification("assistant_message", ControlState{LastAssistantText: "final answer"}); msg != "final answer" {
		t.Fatalf("assistant_message = %q, want final answer", msg)
	}
	if msg := r.formatNotification("assistant_message", ControlState{}); msg != "" {
		t.Fatalf("assistant_message empty = %q, want empty", msg)
	}
	if msg := r.formatNotification("assistant_message", ControlState{LastAssistantText: "reviewed", LastAssistantAgentID: "agent-1", LastAssistantTaskID: "adhoc-1", LastAssistantAgentType: "reviewer"}); msg != "🤖 reviewer · adhoc-1\n\nreviewed" {
		t.Fatalf("subagent assistant_message = %q", msg)
	}
	if msg := r.formatNotification("agent_started", ControlState{LastAgentStarted: &AgentStartedPayload{AgentID: "agent-1", TaskID: "adhoc-1", AgentType: "reviewer", Description: "Review changes"}}); !strings.Contains(msg, "🧩 Delegated reviewer · adhoc-1") || !strings.Contains(msg, "Review changes") {
		t.Fatalf("agent_started = %q", msg)
	}
	if msg := r.formatNotification("agent_notify", ControlState{LastAgentNotify: &AgentNotifyPayload{AgentID: "agent-1", TaskID: "adhoc-1", AgentType: "reviewer", Kind: "blocked", Subtype: "stall_resolved", Message: "Tests pass"}}); !strings.Contains(msg, "📣 reviewer · adhoc-1 · blocked/stall_resolved") || !strings.Contains(msg, "Tests pass") {
		t.Fatalf("agent_notify = %q", msg)
	}
	if msg := r.formatNotification("agent_notify", ControlState{LastAgentNotify: &AgentNotifyPayload{AgentID: "agent-1", TaskID: "adhoc-1", AgentType: "reviewer", Kind: "progress", Message: "Tests pass"}}); !strings.Contains(msg, "📣 reviewer · adhoc-1 · progress") || strings.Contains(msg, "/") {
		t.Fatalf("agent_notify without subtype = %q", msg)
	}
	if msg := r.formatNotification("agent_notify", ControlState{LastAgentNotify: &AgentNotifyPayload{AgentID: "agent-1", TaskID: "adhoc-1", AgentType: "reviewer", Subtype: "stall_resolved", Message: "Tests pass"}}); !strings.Contains(msg, "📣 reviewer · adhoc-1 · stall_resolved") || !strings.Contains(msg, "Tests pass") {
		t.Fatalf("agent_notify without kind = %q", msg)
	}
	if msg := r.formatNotification("local_shell_result", ControlState{LastLocalShell: &LocalShellPayload{Command: "pwd", Output: "/tmp/ws\n"}}); !strings.Contains(msg, "✅ Local shell: pwd") || !strings.Contains(msg, "/tmp/ws") {
		t.Fatalf("local_shell_result = %q", msg)
	}
	if msg := r.formatNotification("local_shell_result", ControlState{LastLocalShell: &LocalShellPayload{Command: "false", Failed: true, Error: "exit status 1"}}); !strings.Contains(msg, "❌ Local shell: false") || !strings.Contains(msg, "exit status 1") {
		t.Fatalf("failed local_shell_result = %q", msg)
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
	if msg := r.formatLongRunningNotification(ControlState{Phase: "thinking", PhaseDetail: "analyzing", InternalEventsSinceLastPush: 2}, time.Now()); !strings.Contains(msg, "Still working") || !strings.Contains(msg, "2 internal events") || strings.Contains(msg, "thinking") {
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
	wechat := &stubIMAdapter{typ: "wechat", supportsLoginRenewal: true, startLoginFunc: func() (string, error) { return "https://wx-login", nil }}
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
		sender := &stubIMAdapter{typ: "wechat", supportsLoginRenewal: true, startLoginFunc: func() (string, error) { return "https://wx-login", nil }}
		r := &NotificationRouter{adapter: sender}
		r.handleLogin("chat", "")
		if got := sender.lastMessage().text; !strings.Contains(got, "/login <platform>") || !strings.Contains(got, "wechat") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("adapter not found", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat", supportsLoginRenewal: true, startLoginFunc: func() (string, error) { return "https://wx-login", nil }}
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
		loginAdapter := &stubIMAdapter{typ: "wechat", supportsLoginRenewal: true, startLoginFunc: func() (string, error) { return "", loginErr }}
		r := &NotificationRouter{adapter: loginAdapter}
		r.handleLogin("chat", "wechat")
		if got := loginAdapter.lastMessage().text; !strings.Contains(got, "Failed to get WeChat login link") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("login success", func(t *testing.T) {
		loginAdapter := &stubIMAdapter{typ: "wechat", supportsLoginRenewal: true, startLoginFunc: func() (string, error) { return "https://wx-login", nil }}
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
		mgr:           newTestChordManager(&config.Config{IMs: []config.IMAdapterConfig{{Feishu: &config.FeishuConfig{Owner: &config.OwnerIdentity{AccountID: "app", UserID: "owner"}, ChatBindings: map[string]string{"feishu-chat": "ws1"}}}, {Wechat: &config.WechatConfig{Owner: &config.OwnerIdentity{AccountID: "bot", UserID: "owner"}, ChatBindings: map[string]string{"wechat-chat": "ws1"}}}}, Workspaces: []config.Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}),
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

func TestSendTextHelper(t *testing.T) {
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

}

func BenchmarkNotificationRouterSnapshotLastKeyChatIDParallel(b *testing.B) {
	r := &NotificationRouter{lastKeyChatID: make(map[string]string, 64)}
	for i := 0; i < 64; i++ {
		key := (processKey{workspaceID: "ws", imType: "wechat", chatID: fmt.Sprintf("chat-%d", i)}).String()
		r.lastKeyChatID[key] = fmt.Sprintf("chat-%d", i)
	}
	_ = r.snapshotLastKeyChatID()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = r.snapshotLastKeyChatID()
		}
	})
}

func TestNotificationRouterBindingSnapshotIsCopyOnWrite(t *testing.T) {
	r := &NotificationRouter{lastKeyChatID: make(map[string]string)}
	r.recordChatID("key-1", "chat-1")
	before := r.snapshotLastKeyChatID()
	r.recordChatID("key-2", "chat-2")
	after := r.snapshotLastKeyChatID()

	if _, ok := before["key-2"]; ok {
		t.Fatal("previous snapshot changed after a later binding update")
	}
	if got := after["key-1"]; got != "chat-1" {
		t.Fatalf("existing binding = %q, want chat-1", got)
	}
	if got := after["key-2"]; got != "chat-2" {
		t.Fatalf("new binding = %q, want chat-2", got)
	}
}

func TestNotificationRouterBindingSnapshotConcurrentReadWrite(t *testing.T) {
	r := &NotificationRouter{lastKeyChatID: make(map[string]string)}
	r.recordChatID("initial", "chat")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = r.snapshotLastKeyChatID()["initial"]
			}
		}()
	}
	for i := 0; i < 100; i++ {
		r.recordChatID(fmt.Sprintf("key-%d", i), fmt.Sprintf("chat-%d", i))
	}
	wg.Wait()
	if got := len(r.snapshotLastKeyChatID()); got != 101 {
		t.Fatalf("snapshot size = %d, want 101", got)
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
		RequestID:     "req-2",
	}, feishuCardContext{})
	qBody := question["body"].(map[string]any)
	qElements := qBody["elements"].([]any)
	if len(qElements) < 5 {
		t.Fatalf("question card elements = %v", qElements)
	}
	questionText := fmt.Sprint(qElements)
	for _, want := range []string{"Choose: Continue?", "Proceed", "/answer @req-2 1"} {
		if !strings.Contains(questionText, want) {
			t.Fatalf("question card missing %q: %v", want, qElements)
		}
	}
	if strings.Contains(questionText, "Default:") {
		t.Fatalf("question card must not advertise a default answer: %v", qElements)
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
	capture := &captureWriteCloser{}
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws", stdin: capture, state: ControlState{Questions: map[string]*QuestionPayload{
		"open": {RequestID: "open"}, "closed": {RequestID: "closed", Outcome: "answered"},
	}}}
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
		registered := len(p.statusWaiters.waiters) > 0
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
	var command struct {
		Type        string   `json:"type"`
		QuestionIDs []string `json:"question_ids"`
	}
	if err := json.Unmarshal([]byte(capture.String()), &command); err != nil || command.Type != "status" || len(command.QuestionIDs) != 1 || command.QuestionIDs[0] != "open" {
		t.Fatalf("status did not request the unknown decision: %+v %v", command, err)
	}

}

// A status_response snapshot is copied on chord's command path while pushes
// emit on its event loop, so a snapshot taken before a switch can arrive after
// that switch's push. The gateway must drop the stale snapshot instead of
// rolling SessionID/busy back.
func TestStaleStatusResponseDoesNotRollBackPushedState(t *testing.T) {
	proc := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws"}

	// Fresh snapshot before any push: applied unconditionally.
	proc.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 1, Payload: json.RawMessage(`{"session_id":"sess-A","busy":false,"last_outcome":"completed"}`)})
	// A newer push moves the session forward.
	proc.processEnvelope(&HeadlessEnvelope{Type: "session_switched", Seq: 2, Payload: json.RawMessage(`{"session_id":"sess-B"}`)})
	// The stale snapshot (copied before the switch) arrives late.
	proc.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 1, Payload: json.RawMessage(`{"session_id":"sess-A","busy":true,"last_outcome":"completed"}`)})

	if proc.state.SessionID != "sess-B" {
		t.Fatalf("SessionID = %q, want sess-B (stale status_response must not roll back the push)", proc.state.SessionID)
	}
	if proc.state.Busy {
		t.Fatalf("Busy = true, want false (stale snapshot must not overwrite pushed state)")
	}
	if proc.state.LastEnvelopeSeq != 2 {
		t.Fatalf("LastEnvelopeSeq = %d, want 2", proc.state.LastEnvelopeSeq)
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

func waitForRoleRequest(t *testing.T, stdin *captureWriteCloser, action string) {
	t.Helper()
	target := `"action":"` + action + `"`
	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for role %s request; stdin=%q", action, stdin.String())
		}
		if strings.Contains(stdin.String(), target) {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWaitRoleList_DeliversResponse(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws", stdin: &captureWriteCloser{}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan RoleResponse, 1)
	go func() {
		resp, err := p.WaitRoleList(ctx)
		if err != nil {
			t.Errorf("WaitRoleList: %v", err)
		}
		done <- resp
	}()

	waitForRoleRequest(t, p.stdin.(*captureWriteCloser), "list")
	p.processEnvelope(&HeadlessEnvelope{Type: "role_response", Payload: []byte(`{"ok":true,"role":"builder","roles":[{"name":"builder","current":true},{"name":"planner"}]}`)})

	resp := <-done
	if !resp.OK || resp.Role != "builder" || len(resp.Roles) != 2 || resp.Roles[1].Name != "planner" {
		t.Fatalf("WaitRoleList response = %#v", resp)
	}
}

func TestWaitRoleSwitch_SendsSetAndDeliversResponse(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws", stdin: &captureWriteCloser{}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan RoleResponse, 1)
	go func() {
		resp, err := p.WaitRoleSwitch(ctx, "planner")
		if err != nil {
			t.Errorf("WaitRoleSwitch: %v", err)
		}
		done <- resp
	}()

	waitForRoleRequest(t, p.stdin.(*captureWriteCloser), "set")
	if s := p.stdin.(*captureWriteCloser).String(); !strings.Contains(s, `"role":"planner"`) {
		t.Fatalf("role set command = %q", s)
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "role_response", Payload: []byte(`{"ok":true,"role":"planner"}`)})

	resp := <-done
	if !resp.OK || resp.Role != "planner" {
		t.Fatalf("WaitRoleSwitch response = %#v", resp)
	}
}

func TestWaitRoleCommandsSerialize(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws", stdin: &captureWriteCloser{}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	listDone := make(chan RoleResponse, 1)
	go func() {
		resp, err := p.WaitRoleList(ctx)
		if err != nil {
			t.Errorf("WaitRoleList: %v", err)
		}
		listDone <- resp
	}()
	waitForRoleRequest(t, p.stdin.(*captureWriteCloser), "list")

	setStarted := make(chan struct{})
	setDone := make(chan RoleResponse, 1)
	go func() {
		close(setStarted)
		resp, err := p.WaitRoleSwitch(ctx, "planner")
		if err != nil {
			t.Errorf("WaitRoleSwitch: %v", err)
		}
		setDone <- resp
	}()
	<-setStarted
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		if strings.Contains(p.stdin.(*captureWriteCloser).String(), `"action":"set"`) {
			t.Fatal("role set must wait until the in-flight list round-trip finishes")
		}
		time.Sleep(time.Millisecond)
	}

	p.processEnvelope(&HeadlessEnvelope{Type: "role_response", Payload: []byte(`{"ok":true,"role":"builder","roles":[{"name":"builder"}]}`)})
	select {
	case resp := <-listDone:
		if !resp.OK || resp.Role != "builder" {
			t.Fatalf("WaitRoleList response = %#v", resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("list waiter did not finish")
	}

	waitForRoleRequest(t, p.stdin.(*captureWriteCloser), "set")
	p.processEnvelope(&HeadlessEnvelope{Type: "role_response", Payload: []byte(`{"ok":true,"role":"planner"}`)})
	select {
	case resp := <-setDone:
		if !resp.OK || resp.Role != "planner" {
			t.Fatalf("WaitRoleSwitch response = %#v", resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("set waiter did not finish")
	}
}

func TestWaitRoleResponse_TimesOutWithoutResponse(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws", stdin: &captureWriteCloser{}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := p.WaitRoleList(ctx); err == nil {
		t.Fatal("expected error on context expiry, got nil")
	}
}

func TestRoleEnvelopesTrackCurrentRole(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws", stdin: &captureWriteCloser{}}

	p.processEnvelope(&HeadlessEnvelope{Type: "role_change", Payload: []byte(`{"role":"planner"}`)})
	if got := p.State().CurrentRole; got != "planner" {
		t.Fatalf("CurrentRole after role_change = %q", got)
	}

	p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Payload: []byte(`{"current_role":"builder"}`)})
	if got := p.State().CurrentRole; got != "builder" {
		t.Fatalf("CurrentRole after status_response = %q", got)
	}

	// A role_response reports the newly active role on success.
	p.processEnvelope(&HeadlessEnvelope{Type: "role_response", Payload: []byte(`{"ok":true,"role":"planner","roles":[{"name":"builder"},{"name":"planner","current":true}]}`)})
	if got := p.State().CurrentRole; got != "planner" {
		t.Fatalf("CurrentRole after role_response = %q", got)
	}
}

func roleHandlerTestRouter(sender *stubIMAdapter, proc *ChordProcess) (*NotificationRouter, string) {
	key := (processKey{workspaceID: "ws1", imType: sender.typ, chatID: "chat-1"}).String()
	if proc.key == "" {
		proc.key = key
	}
	return &NotificationRouter{adapter: sender, lastKeyChatID: make(map[string]string)}, key
}

// runRoleHandler drives handleRoleCommand in a goroutine and feeds it
// role_response envelopes whenever it issues the matching role request.
// responses must line up with requests by index.
func runRoleHandler(t *testing.T, sender *stubIMAdapter, proc *ChordProcess, cmd IMCommand, incoming IncomingMessage, requests []string, responses []RoleResponse) {
	t.Helper()
	r, key := roleHandlerTestRouter(sender, proc)
	ws := &config.Workspace{ID: "ws1", Path: "/tmp/ws1"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.handleRoleCommand(ws, "chat-1", cmd, incoming, key, proc)
	}()

	need := map[string]int{}
	for i, req := range requests {
		need[req]++
		target := `"action":"` + req + `"`
		deadline := time.Now().Add(2 * time.Second)
		for {
			if time.Now().After(deadline) {
				t.Fatalf("timeout waiting for role %s request #%d; stdin=%q", req, need[req], proc.stdin.(*captureWriteCloser).String())
			}
			if strings.Count(proc.stdin.(*captureWriteCloser).String(), target) >= need[req] {
				break
			}
			time.Sleep(time.Millisecond)
		}
		payload, err := json.Marshal(responses[i])
		if err != nil {
			t.Fatalf("marshal role response: %v", err)
		}
		proc.processEnvelope(&HeadlessEnvelope{Type: "role_response", Payload: payload})
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("role handler did not finish; last message = %q", sender.lastMessage().text)
	}
}

func newRoleTestProcess(imType string) *ChordProcess {
	return &ChordProcess{
		key:         (processKey{workspaceID: "ws1", imType: imType, chatID: "chat-1"}).String(),
		workspaceID: "ws1",
		stdin:       &captureWriteCloser{},
	}
}

func roleListResponse(current string, names ...string) RoleResponse {
	roles := make([]RoleInfo, 0, len(names))
	for _, n := range names {
		roles = append(roles, RoleInfo{Name: n, Current: n == current})
	}
	return RoleResponse{OK: true, Role: current, Roles: roles}
}

func TestRoleMenuShownForBareRole(t *testing.T) {
	sender := &stubIMAdapter{typ: "wechat"}
	proc := newRoleTestProcess("wechat")
	runRoleHandler(t, sender, proc, IMCommand{Type: "role"}, IncomingMessage{IMType: "wechat", ChatID: "chat-1"},
		[]string{"list"}, []RoleResponse{roleListResponse("builder", "builder", "planner")})
	msg := sender.lastMessage().text
	requireContains(t, msg, "Current role: builder")
	requireContains(t, msg, "1. builder (current)")
	requireContains(t, msg, "2. planner")
	requireContains(t, msg, "Reply /role <number> or /role <name> to switch.")
}

func TestRoleSwitchByNumber(t *testing.T) {
	sender := &stubIMAdapter{typ: "wechat"}
	proc := newRoleTestProcess("wechat")
	runRoleHandler(t, sender, proc, IMCommand{Type: "role", Content: "2"}, IncomingMessage{IMType: "wechat", ChatID: "chat-1"},
		[]string{"list", "set"}, []RoleResponse{roleListResponse("builder", "builder", "planner"), {OK: true, Role: "planner"}})
	requireContains(t, sender.lastMessage().text, "✅ Switched role: planner")
}

func TestRoleSwitchByNameSkipsStaleNumber(t *testing.T) {
	sender := &stubIMAdapter{typ: "wechat"}
	proc := newRoleTestProcess("wechat")
	runRoleHandler(t, sender, proc, IMCommand{Type: "role", Content: "reviewer"}, IncomingMessage{IMType: "wechat", ChatID: "chat-1"},
		[]string{"list"}, []RoleResponse{roleListResponse("builder", "builder", "planner")})
	msg := sender.lastMessage().text
	requireContains(t, msg, `"reviewer" is not an available role`)
	if s := proc.stdin.(*captureWriteCloser).String(); strings.Contains(s, `"action":"set"`) {
		t.Fatalf("no role set expected for unavailable name, stdin=%q", s)
	}
}

func TestRoleAlreadyCurrentIsNoOp(t *testing.T) {
	sender := &stubIMAdapter{typ: "wechat"}
	proc := newRoleTestProcess("wechat")
	runRoleHandler(t, sender, proc, IMCommand{Type: "role", Content: "1"}, IncomingMessage{IMType: "wechat", ChatID: "chat-1"},
		[]string{"list"}, []RoleResponse{roleListResponse("builder", "builder", "planner")})
	requireContains(t, sender.lastMessage().text, "builder is already the current role")
	if s := proc.stdin.(*captureWriteCloser).String(); strings.Contains(s, `"action":"set"`) {
		t.Fatalf("no role set expected for current role, stdin=%q", s)
	}
}

func TestRoleInvalidNumberIsRejected(t *testing.T) {
	sender := &stubIMAdapter{typ: "wechat"}
	proc := newRoleTestProcess("wechat")
	runRoleHandler(t, sender, proc, IMCommand{Type: "role", Content: "99"}, IncomingMessage{IMType: "wechat", ChatID: "chat-1"},
		[]string{"list"}, []RoleResponse{roleListResponse("builder", "builder", "planner")})
	requireContains(t, sender.lastMessage().text, "Invalid role selection")
	if s := proc.stdin.(*captureWriteCloser).String(); strings.Contains(s, `"action":"set"`) {
		t.Fatalf("no role set expected for an invalid number, stdin=%q", s)
	}
}

func TestRoleSwitchRejectedMessageIsShown(t *testing.T) {
	sender := &stubIMAdapter{typ: "wechat"}
	proc := newRoleTestProcess("wechat")
	runRoleHandler(t, sender, proc, IMCommand{Type: "role", Content: "planner"}, IncomingMessage{IMType: "wechat", ChatID: "chat-1"},
		[]string{"list", "set"},
		[]RoleResponse{roleListResponse("builder", "builder", "planner"), {OK: false, Message: "resolve the pending handoff before switching role"}})
	requireContains(t, sender.lastMessage().text, "resolve the pending handoff before switching role")
}

func TestRoleInternalActionSwitchesRole(t *testing.T) {
	sender := &stubIMAdapter{typ: "feishu"}
	proc := newRoleTestProcess("feishu")
	incoming := IncomingMessage{IMType: "feishu", ChatID: "chat-1", InternalAction: &InternalAction{Type: "role", Action: "switch", RequestID: "role-123", Value: "planner"}}
	runRoleHandler(t, sender, proc, IMCommand{Type: "role", Content: "planner"}, incoming,
		[]string{"list", "set"},
		[]RoleResponse{roleListResponse("builder", "builder", "planner"), {OK: true, Role: "planner"}})
	requireContains(t, sender.lastMessage().text, "✅ Switched role: planner")
}

func TestRoleInternalActionNumericNameMatchedByName(t *testing.T) {
	// A card button carries the exact role name. A role literally named "2"
	// must switch by name even though "2" parses as a menu number: position
	// 2 here is "planner", so the numeric branch would switch to the wrong
	// role instead.
	sender := &stubIMAdapter{typ: "feishu"}
	proc := newRoleTestProcess("feishu")
	incoming := IncomingMessage{IMType: "feishu", ChatID: "chat-1", InternalAction: &InternalAction{Type: "role", Action: "switch", RequestID: "role-456", Value: "2"}}
	runRoleHandler(t, sender, proc, IMCommand{Type: "role", Content: "2"}, incoming,
		[]string{"list", "set"},
		[]RoleResponse{roleListResponse("builder", "builder", "planner", "2"), {OK: true, Role: "2"}})
	requireContains(t, sender.lastMessage().text, "✅ Switched role: 2")
	if s := proc.stdin.(*captureWriteCloser).String(); !strings.Contains(s, `"role":"2"`) {
		t.Fatalf("role set must target the literal name, stdin=%q", s)
	}
}

func TestRoleFailureResolvesFeishuMenuCard(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failSwitch bool
		timeout    bool
		want       string
	}{
		{name: "list rejected", want: "backend unavailable"},
		{name: "switch write failed", failSwitch: true, want: "Could not confirm the role switch. Send /status to check the current role."},
		{name: "switch response timeout", timeout: true, want: "Could not confirm the role switch. Send /status to check the current role."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.timeout {
				// Shorten the switch round-trip instead of waiting out the
				// real 10-second timeout.
				oldTimeout := roleSwitchTimeout
				roleSwitchTimeout = 50 * time.Millisecond
				defer func() { roleSwitchTimeout = oldTimeout }()
			}
			feishu := testFeishuAdapter(t, &config.FeishuConfig{AppID: "app", AppSecret: "secret"})
			defer feishu.dedupe.Close()
			var patchedPath, patchedBody, sentText string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
					_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"token","expire":7200}`))
				case r.URL.Path == "/open-apis/im/v1/messages/om_role_1" && r.Method == http.MethodPatch:
					patchedPath = r.URL.Path
					body, _ := io.ReadAll(r.Body)
					patchedBody = string(body)
					_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
				case strings.HasPrefix(r.URL.Path, "/open-apis/im/v1/messages"):
					body, _ := io.ReadAll(r.Body)
					sentText = string(body)
					_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_new"}}`))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
				}
			}))
			defer server.Close()
			feishu.httpClient = server.Client()
			oldBaseURL := feishuOpenBaseURL
			feishuOpenBaseURL = server.URL
			defer func() { feishuOpenBaseURL = oldBaseURL }()

			r := &NotificationRouter{adapter: feishu, lastKeyChatID: make(map[string]string)}
			proc := newRoleTestProcess("feishu")
			key := proc.key
			r.recordCardHandle(key, "role", "role-1", &InteractiveCardHandle{MessageID: "om_role_1"})
			ws := &config.Workspace{ID: "ws1", Path: "/tmp/ws1"}
			incoming := IncomingMessage{IMType: "feishu", ChatID: "chat-1", InternalAction: &InternalAction{Type: "role", Action: "switch", RequestID: "role-1", Value: "planner"}}

			done := make(chan struct{})
			go func() {
				defer close(done)
				r.handleRoleCommand(ws, "chat-1", IMCommand{Type: "role", Content: "planner"}, incoming, key, proc)
			}()
			waitForRoleRequest(t, proc.stdin.(*captureWriteCloser), "list")
			if tc.failSwitch || tc.timeout {
				if tc.failSwitch {
					proc.mu.Lock()
					proc.stdin = nil
					proc.mu.Unlock()
				}
				proc.processEnvelope(&HeadlessEnvelope{Type: "role_response", Payload: []byte(`{"ok":true,"role":"builder","roles":[{"name":"builder"},{"name":"planner"}]}`)})
			} else {
				proc.processEnvelope(&HeadlessEnvelope{Type: "role_response", Payload: []byte(`{"ok":false,"message":"backend unavailable"}`)})
			}
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("role handler did not finish after failure")
			}

			if patchedPath != "/open-apis/im/v1/messages/om_role_1" {
				t.Fatalf("menu card was not resolved, patched path = %q", patchedPath)
			}
			if handle, ok := r.takeCardHandle(key, "role", "role-1"); ok {
				t.Fatalf("resolved card handle should be consumed, got %#v", handle)
			}
			if sentText != "" {
				t.Fatalf("card-originated failure must not send a duplicate chat line, body = %q", sentText)
			}
			if !strings.Contains(patchedBody, tc.want) {
				t.Fatalf("card does not show the failure, body = %q", patchedBody)
			}
		})
	}
}

func TestRoleSuccessResolvesFeishuMenuCardWithoutChatLine(t *testing.T) {
	feishu := testFeishuAdapter(t, &config.FeishuConfig{AppID: "app", AppSecret: "secret"})
	defer feishu.dedupe.Close()
	var patchedPath, patchedBody, sentText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"token","expire":7200}`))
		case r.URL.Path == "/open-apis/im/v1/messages/om_role_1" && r.Method == http.MethodPatch:
			patchedPath = r.URL.Path
			body, _ := io.ReadAll(r.Body)
			patchedBody = string(body)
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
		case strings.HasPrefix(r.URL.Path, "/open-apis/im/v1/messages"):
			body, _ := io.ReadAll(r.Body)
			sentText = string(body)
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_new"}}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
		}
	}))
	defer server.Close()
	feishu.httpClient = server.Client()
	oldBaseURL := feishuOpenBaseURL
	feishuOpenBaseURL = server.URL
	defer func() { feishuOpenBaseURL = oldBaseURL }()

	r := &NotificationRouter{adapter: feishu, lastKeyChatID: make(map[string]string)}
	proc := newRoleTestProcess("feishu")
	key := proc.key
	r.recordCardHandle(key, "role", "role-1", &InteractiveCardHandle{MessageID: "om_role_1"})
	ws := &config.Workspace{ID: "ws1", Path: "/tmp/ws1"}
	incoming := IncomingMessage{IMType: "feishu", ChatID: "chat-1", InternalAction: &InternalAction{Type: "role", Action: "switch", RequestID: "role-1", Value: "planner"}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.handleRoleCommand(ws, "chat-1", IMCommand{Type: "role", Content: "planner"}, incoming, key, proc)
	}()
	waitForRoleRequest(t, proc.stdin.(*captureWriteCloser), "list")
	proc.processEnvelope(&HeadlessEnvelope{Type: "role_response", Payload: []byte(`{"ok":true,"role":"builder","roles":[{"name":"builder"},{"name":"planner"}]}`)})
	waitForRoleRequest(t, proc.stdin.(*captureWriteCloser), "set")
	proc.processEnvelope(&HeadlessEnvelope{Type: "role_response", Payload: []byte(`{"ok":true,"role":"planner"}`)})
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("role handler did not finish after switch")
	}

	if patchedPath != "/open-apis/im/v1/messages/om_role_1" {
		t.Fatalf("menu card was not resolved, patched path = %q", patchedPath)
	}
	if sentText != "" {
		t.Fatalf("card-originated switch must not send a duplicate chat line, body = %q", sentText)
	}
	if !strings.Contains(patchedBody, "Switched role: planner") {
		t.Fatalf("card does not show the switch, body = %q", patchedBody)
	}
}

func TestCommandFromInternalActionRole(t *testing.T) {
	cmd := commandFromInternalAction(&InternalAction{Type: "role", Action: "switch", RequestID: "role-1", Value: "planner"})
	if cmd.Type != "role" || cmd.Content != "planner" {
		t.Fatalf("commandFromInternalAction(role) = %#v", cmd)
	}
}

func TestValidFeishuRoleCardAction(t *testing.T) {
	if !isValidFeishuCardAction("role", "switch", "planner", nil) {
		t.Fatal("role/switch/name should be valid")
	}
	if isValidFeishuCardAction("role", "switch", "", nil) {
		t.Fatal("role switch without a role name must be invalid")
	}
	if isValidFeishuCardAction("role", "allow", "planner", nil) {
		t.Fatal("role/allow must be invalid")
	}
	if isValidFeishuCardAction("confirm", "switch", "planner", nil) {
		t.Fatal("confirm/switch must stay invalid")
	}
}

func TestValidFeishuQuestionCardAction(t *testing.T) {
	// A single-select button carries the exact option label.
	if !isValidFeishuCardAction("question", "answer", "yes", nil) {
		t.Fatal("question/answer with a label should be valid")
	}
	// A form submission carries option numbers instead of a label.
	if !isValidFeishuCardAction("question", "answer", "", []string{"1", "3"}) {
		t.Fatal("question/answer with form answers should be valid")
	}
	if isValidFeishuCardAction("question", "answer", "", nil) {
		t.Fatal("question/answer without a value or form answers must be invalid")
	}
	if isValidFeishuCardAction("question", "allow", "yes", []string{"1"}) {
		t.Fatal("question/allow must be invalid even with form answers")
	}
}

func TestBuildFeishuRoleCard(t *testing.T) {
	card := buildFeishuRoleCard("oc_chat1", "role-1", "builder", []RoleInfo{{Name: "builder", Current: true}, {Name: "planner"}})
	data, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal role card: %v", err)
	}
	raw := string(data)
	requireContains(t, raw, `"content":"Switch role"`)
	requireContains(t, raw, "Current role: builder")
	requireContains(t, raw, "2. planner")
	if strings.Contains(raw, `"value":"builder"`) {
		t.Fatalf("current role must not be a button: %s", raw)
	}
	// Buttons carry role context for the card-action callback.
	requireContains(t, raw, `"type":"role"`)
	requireContains(t, raw, `"action":"switch"`)
	requireContains(t, raw, `"request_id":"role-1"`)
	requireContains(t, raw, `"chat_id":"oc_chat1"`)
	requireContains(t, raw, `"value":"planner"`)
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
		attachQuestionTestReceipts(proc)
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
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{Questions: map[string]*QuestionPayload{"req-q": {RequestID: "req-q", Options: []string{"yes", "no"}, OptionIDs: []string{"yes", "no"}, Version: 1}}})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "question", Answers: []string{"1"}}, "wechat", nil)
		if got := sender.lastMessage().text; got != "✅ Answer accepted (req-q): yes" {
			t.Fatalf("message = %q", got)
		}
		if out := stdin.String(); !strings.Contains(out, `"answers":["yes"]`) || !strings.Contains(out, `"action":"answer"`) {
			t.Fatalf("stdin = %q", out)
		}
	})

	t.Run("historical question requires explicit target", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{Questions: map[string]*QuestionPayload{"req-q": {RequestID: "req-q", Question: "Choose environment", Options: []string{"dev", "prod"}, OptionIDs: []string{"dev", "prod"}, Version: 1, Outcome: "no_response"}}})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "question", Answers: []string{"prod"}}, "wechat", nil)
		if !strings.Contains(sender.lastMessage().text, "No pending question") || stdin.String() != "" {
			t.Fatal("historical question guessed without target")
		}
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "question", RequestID: "req-q", Answers: []string{"prod"}}, "wechat", nil)
		if !strings.Contains(sender.lastMessage().text, "Answer accepted") || !strings.Contains(stdin.String(), `"action":"revise"`) {
			t.Fatalf("historical revision: %s %s", sender.lastMessage().text, stdin.String())
		}
	})

	t.Run("session switch drops the expired question follow-up", func(t *testing.T) {
		expired := ControlState{Questions: map[string]*QuestionPayload{"req-q": {RequestID: "req-q", Question: "Choose env", Version: 1, Outcome: "no_response"}}}
		r, proc, sender, _, key, ws := newRouterAndProcess(expired)
		proc.onEvent = r.HandleChordEvent
		// The expired question was pushed to the chat, so the router mirrors it.
		r.HandleChordEvent(key, "question_resolved", expired)

		proc.processEnvelope(&HeadlessEnvelope{Type: "session_switched", Payload: json.RawMessage(`{"session_id":"sess-b"}`)})

		if got := proc.State().Questions["req-q"]; got != nil {
			t.Fatalf("ExpiredQuestion after switch = %+v, want nil", got)
		}
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "question", Answers: []string{"prod"}}, "wechat", nil)
		if got := sender.lastMessage().text; !strings.Contains(got, "No pending question") {
			t.Fatalf("late answer after a session switch = %q, want it rejected", got)
		}
	})

	t.Run("plain send keeps pending question", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{Questions: map[string]*QuestionPayload{"req-auto": {RequestID: "req-auto", Version: 1}}})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "send", Content: "free text answer"}, "wechat", nil)
		if got := sender.lastMessage().text; got != "" {
			t.Fatalf("message = %q", got)
		}
		if !strings.Contains(stdin.String(), `"type":"send"`) || strings.Contains(stdin.String(), `"type":"question"`) || !strings.Contains(stdin.String(), `"free text answer"`) {
			t.Fatalf("stdin = %q", stdin.String())
		}
	})

	t.Run("feishu plain send keeps question card", func(t *testing.T) {
		cfg := &config.Config{IMs: []config.IMAdapterConfig{{Feishu: &config.FeishuConfig{AppID: "app", AppSecret: "secret", ChatBindings: map[string]string{"chat-1": "ws1"}}}}, Workspaces: []config.Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}
		paths := &config.Paths{StateDir: t.TempDir(), DedupeDir: t.TempDir()}
		mgr := NewChordManager(cfg, paths)
		stdin := &captureWriteCloser{}
		key := (processKey{workspaceID: "ws1", imType: "feishu", chatID: "chat-1"}).String()
		proc := &ChordProcess{key: key, workspaceID: "ws1", stdin: stdin, state: ControlState{Questions: map[string]*QuestionPayload{"req-auto": {RequestID: "req-auto", ToolName: "QuestionTool", Version: 1}}}}
		attachQuestionTestReceipts(proc)
		mgr.procs[key] = proc
		feishu := testFeishuAdapter(t, &config.FeishuConfig{AppID: "app", AppSecret: "secret", ChatBindings: map[string]string{"chat-1": "ws1"}})
		defer feishu.dedupe.Close()
		var patchedPath string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/open-apis/auth/v3/tenant_access_token/internal":
				_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"token","expire":7200}`))
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
		r := &NotificationRouter{mgr: mgr, adapter: feishu, lastKeyChatID: make(map[string]string), expiredPending: make(map[string]expiredPendingState), cardHandles: make(map[string]cardHandleEntry)}
		r.recordCardHandle(key, "question", "req-auto", &InteractiveCardHandle{MessageID: "om_sent_1"})
		msg := IncomingMessage{IMType: "feishu", ChatID: "chat-1", SenderID: "ou_owner", Text: "free text answer"}
		r.handleChordCommand(&cfg.Workspaces[0], "chat-1", IMCommand{Type: "send", Content: "free text answer"}, "feishu", &msg)
		// Submitting an answer must not patch the card: only the core
		// question_resolved(answered) event proves the answer was accepted.
		if patchedPath != "" {
			t.Fatalf("card must not be patched on submit, patched path = %q", patchedPath)
		}
		if !strings.Contains(stdin.String(), `"type":"send"`) || strings.Contains(stdin.String(), `"type":"question"`) || !strings.Contains(stdin.String(), `"free text answer"`) {
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

func TestFeishuCardRiskLevelsAndQuestionCardShape(t *testing.T) {
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
}

func TestShouldSendFeishuQuestionCard(t *testing.T) {
	if !shouldSendFeishuQuestionCard(&QuestionPayload{Options: []string{"1", "2", "3", "4", "5", "6"}}) {
		t.Fatal("short 6-option single-select should use a card")
	}
	if !shouldSendFeishuQuestionCard(&QuestionPayload{Multiple: true, Options: []string{"1", "2"}}) {
		t.Fatal("multi-select should use a card with a checkable form")
	}
	if shouldSendFeishuQuestionCard(&QuestionPayload{}) {
		t.Fatal("free-answer question should fallback to text")
	}
	if !shouldSendFeishuQuestionCard(&QuestionPayload{Options: []string{"1", "2", "3", "4", "5", "this option label is intentionally too long for mobile buttons"}}) {
		t.Fatal("a long option label should switch the card to numbered buttons, not fallback to text")
	}
	if !shouldSendFeishuQuestionCard(&QuestionPayload{Options: []string{"this option label is intentionally too long for mobile buttons", "no"}}) {
		t.Fatal("a long option label should stay a card even with few options")
	}
	if !shouldSendFeishuQuestionCard(&QuestionPayload{Options: []string{"yes", "no"}, OptionIDs: []string{"yes", "no"}, OptionDetails: []string{"this detail is intentionally made very long so the detail no longer decides whether the interactive card is used", "short"}}) {
		t.Fatal("a long option detail should stay a card: details are card body text, not button labels")
	}
	if shouldSendFeishuQuestionCard(&QuestionPayload{Options: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"}}) {
		t.Fatal("more than ten options should fallback to text")
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
	card := buildFeishuQuestionCard("chat", &QuestionPayload{Question: "Pick", Options: []string{"yes", "no"}, OptionIDs: []string{"yes", "no"}, RequestID: "req"}, feishuCardContext{})
	text := fmt.Sprint(card["body"])
	for _, want := range []string{"1. yes", "2. no"} {
		if !strings.Contains(text, want) {
			t.Fatalf("question card missing %q: %v", want, card)
		}
	}
}

func TestFeishuCardJSONRoundTrip(t *testing.T) {
	card := buildFeishuQuestionCard("chat", &QuestionPayload{Question: "Q?", Options: []string{"A"}, OptionIDs: []string{"A"}, RequestID: "req"}, feishuCardContext{})
	data, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	if !strings.Contains(string(data), `"schema":"2.0"`) {
		t.Fatalf("unexpected card json: %s", data)
	}
}

func TestApplyQuestionResolvedLifecycle(t *testing.T) {
	state := ControlState{}
	state.applyQuestionState(&QuestionPayload{RequestID: "q-1", Version: 1})
	state.applyQuestionState(&QuestionPayload{RequestID: "q-2", Version: 1})
	state.applyQuestionState(&QuestionPayload{RequestID: "q-1", Version: 2, Outcome: QuestionResolvedAnswered})
	if state.uniquePendingQuestion() == nil || state.uniquePendingQuestion().RequestID != "q-2" || state.Questions["q-1"].Outcome != QuestionResolvedAnswered {
		t.Fatal("targeted close lost state")
	}
	if state.applyQuestionState(&QuestionPayload{RequestID: "q-1", Version: 1}) {
		t.Fatal("stale state reopened question")
	}

}

func TestTransitionToIdleQuestionLifecycle(t *testing.T) {
	p := &ChordProcess{}
	p.state.applyQuestionState(&QuestionPayload{RequestID: "q-1", Version: 1})
	p.state.applyQuestionState(&QuestionPayload{RequestID: "q-2", Version: 2, Outcome: QuestionResolvedNoResponse})
	p.transitionToIdle("", true)
	if p.State().pendingQuestionCount() != 1 || p.State().Questions["q-2"].Outcome != QuestionResolvedNoResponse {
		t.Fatal("idle removed question state")
	}
	p.state.PendingConfirm = &ConfirmPayload{RequestID: "c-1"}
	p.state.PendingHandoff = &HandoffPayload{RequestID: "h-1"}
	p.transitionToIdle("", true)
	if p.state.ExpiredConfirm == nil || p.state.ExpiredHandoff == nil {
		t.Fatal("confirm/handoff expiry changed")
	}

}

func TestProcessEnvelopeQuestionResolvedClearsPending(t *testing.T) {
	proc := &ChordProcess{}
	proc.processEnvelope(&HeadlessEnvelope{Type: "question_request", Payload: json.RawMessage(`{"request_id":"q-1","version":1}`)})
	proc.processEnvelope(&HeadlessEnvelope{Type: "question_resolved", Payload: json.RawMessage(`{"request_id":"q-9","reason":"no_response","version":2}`)})
	if proc.State().uniquePendingQuestion() == nil {
		t.Fatal("unrelated close removed pending")
	}
	proc.processEnvelope(&HeadlessEnvelope{Type: "question_resolved", Payload: json.RawMessage(`{"request_id":"q-1","reason":"no_response","version":2}`)})
	if proc.State().uniquePendingQuestion() != nil || proc.State().Questions["q-1"].Outcome != QuestionResolvedNoResponse {
		t.Fatal("terminal record lost")
	}

}

func TestQuestionCommandRevisesDeclinedWhileNewQuestionPending(t *testing.T) {
	ws := &config.Workspace{ID: "ws1", Path: t.TempDir()}
	sender := &stubIMAdapter{typ: "wechat"}
	r := &NotificationRouter{adapter: sender}
	stdin := &captureWriteCloser{}
	proc := &ChordProcess{stdin: stdin}
	attachQuestionTestReceipts(proc)
	proc.state.applyQuestionState(&QuestionPayload{RequestID: "old", Version: 2, Outcome: QuestionResolvedDeclined})
	proc.state.applyQuestionState(&QuestionPayload{RequestID: "new", Version: 1})
	r.handleQuestionCommand(ws, "chat-1", IMCommand{Type: "question", RequestID: "old", Answers: []string{"yes"}}, IncomingMessage{IMType: "wechat", MessageID: "input-1"}, "ws1|wechat|chat-1", proc)
	if !strings.Contains(stdin.String(), `"action":"revise"`) || !strings.Contains(sender.lastMessage().text, "accepted") {
		t.Fatal("historical answer failed with a new pending question")
	}
	if proc.State().Questions["old"].Outcome != QuestionResolvedDeclined || proc.State().pendingQuestionCount() != 1 {
		t.Fatal("revision rewrote original outcome or affected new question")
	}
}

func TestFormatQuestionResolvedNotification(t *testing.T) {
	r := &NotificationRouter{}
	for _, tc := range []struct {
		reason string
		want   string
	}{
		{QuestionResolvedAnswered, "accepted"},
		{QuestionResolvedDeclined, "declined"},
		{QuestionResolvedNoResponse, "deadline"},
		{QuestionResolvedSuperseded, "superseded"},
		{QuestionResolvedCancelled, "cancelled"},
		{QuestionResolvedError, "error"},
	} {
		got := r.formatNotification("question_resolved", ControlState{LastQuestionResolved: &QuestionPayload{RequestID: "q", Outcome: tc.reason}})
		if !strings.Contains(strings.ToLower(got), tc.want) {
			t.Errorf("reason %q notification = %q, want to mention %q", tc.reason, got, tc.want)
		}
	}
	if got := r.formatNotification("question_resolved", ControlState{}); got != "" {
		t.Fatalf("question_resolved without a payload = %q, want empty", got)
	}
}

func TestCommandFromInternalActionQuestion(t *testing.T) {
	cmd := commandFromInternalAction(&InternalAction{Type: "question", Action: "answer", RequestID: "q-1", Value: "yes"})
	if cmd.Type != "question" || cmd.RequestID != "q-1" || strings.Join(cmd.Answers, ",") != "yes" {
		t.Fatalf("button click command = %#v", cmd)
	}
	cmd = commandFromInternalAction(&InternalAction{Type: "question", Action: "answer", RequestID: "q-1", Values: []string{"1", "3"}})
	if strings.Join(cmd.Answers, ",") != "1,3" {
		t.Fatalf("form submit command = %#v", cmd)
	}
}

func TestFeishuQuestionFormAnswersFromCallback(t *testing.T) {
	answers, err := feishuQuestionFormAnswers(map[string]interface{}{
		"option_2": true, "option_3": false, "option_1": true,
		"unrelated": "kept out of the answers",
	})
	if err != nil || strings.Join(answers, ",") != "1,2" {
		t.Fatalf("answers=%v err=%v", answers, err)
	}
	if got, err := feishuQuestionFormAnswers(map[string]interface{}{"option_1": false}); err != nil || len(got) != 0 {
		t.Fatalf("unchecked options selected: %v err=%v", got, err)
	}
	for _, value := range []any{nil, "true", "false", "unknown", []any{true}, map[string]any{"checked": false}, 1} {
		if answers, err := feishuQuestionFormAnswers(map[string]interface{}{"option_1": true, "option_2": value}); err == nil || len(answers) != 0 {
			t.Fatalf("unknown checker shape accepted: %v answers=%v", value, answers)
		}
	}
}

func TestBuildFeishuQuestionCardNumberedButtonsForLongLabels(t *testing.T) {
	longLabel := "enable detail TTL cleanup behind a flag, default off (recommended)"
	card := buildFeishuQuestionCard("oc_chat1", &QuestionPayload{Question: "Pick", Options: []string{longLabel, "no"}, OptionIDs: []string{"long-option", "no-option"}, RequestID: "req"}, feishuCardContext{})
	data, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal question card: %v", err)
	}
	raw := string(data)
	requireContains(t, raw, "1. "+longLabel)
	requireContains(t, raw, `"content":"1"`)
	requireContains(t, raw, `"content":"2"`)
	// The button value carries the stable option ID.
	requireContains(t, raw, `"value":"long-option"`)
	if strings.Contains(raw, `"content":"`+longLabel+`"`) {
		t.Fatalf("a long label must not become a button label: %s", raw)
	}
}

func TestBuildFeishuQuestionCardKeepsShortLabelsOnButtons(t *testing.T) {
	card := buildFeishuQuestionCard("oc_chat1", &QuestionPayload{Question: "Pick", Options: []string{"yes", "no"}, OptionIDs: []string{"yes", "no"}, RequestID: "req"}, feishuCardContext{})
	data, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal question card: %v", err)
	}
	raw := string(data)
	requireContains(t, raw, `"content":"yes"`)
	requireContains(t, raw, `"content":"no"`)
}

func TestBuildFeishuQuestionCardMultiSelectForm(t *testing.T) {
	card := buildFeishuQuestionCard("oc_chat1", &QuestionPayload{Question: "Pick", Multiple: true, Options: []string{"one", "two"}, OptionIDs: []string{"one", "two"}, RequestID: "req"}, feishuCardContext{})
	data, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal question card: %v", err)
	}
	raw := string(data)
	for _, want := range []string{
		`"tag":"form"`,
		`"name":"question_form"`,
		`"tag":"checker"`,
		`"name":"option_1"`,
		`"name":"option_2"`,
		`"name":"question_submit"`,
		`"form_action_type":"submit"`,
		`"action":"answer"`,
		`"request_id":"req"`,
	} {
		requireContains(t, raw, want)
	}
	if count := strings.Count(raw, `"tag":"checker"`); count != 2 {
		t.Fatalf("expected one checker per option, got %d: %s", count, raw)
	}
}

func TestHandleIncomingMessageQuestionFormSubmit(t *testing.T) {
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
			Questions: map[string]*QuestionPayload{"req-q": {RequestID: "req-q", Multiple: true, Options: []string{"yes, always", "no"}, OptionIDs: []string{"yes, always", "no"}, Version: 1}},
		},
	}
	attachQuestionTestReceipts(mgr.procs[key])
	r := &NotificationRouter{mgr: mgr, adapter: sender, lastKeyChatID: make(map[string]string), expiredPending: make(map[string]expiredPendingState)}
	defer r.stopReminder(key)

	r.dispatchMessage(IncomingMessage{
		IMType: "feishu",
		ChatID: "oc_chat",
		InternalAction: &InternalAction{
			Type:      "question",
			Action:    "answer",
			RequestID: "req-q",
			Values:    []string{"1", "2"},
		},
	})
	out := stdin.String()
	if !strings.Contains(out, `"answers":["yes, always","no"]`) {
		t.Fatalf("form answers were sent as option numbers: %q", out)
	}
	if got := sender.lastMessage().text; !strings.Contains(got, "yes, always, no") {
		t.Fatalf("message = %q", got)
	}
}

func TestHandleIncomingMessageQuestionNumericLabelButton(t *testing.T) {
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
			Questions: map[string]*QuestionPayload{"req-q": {RequestID: "req-q", Options: []string{"0", "1"}, OptionIDs: []string{"opt-0", "opt-1"}, Version: 1}},
		},
	}
	attachQuestionTestReceipts(mgr.procs[key])
	r := &NotificationRouter{mgr: mgr, adapter: sender, lastKeyChatID: make(map[string]string), expiredPending: make(map[string]expiredPendingState)}
	defer r.stopReminder(key)

	r.dispatchMessage(IncomingMessage{
		IMType: "feishu",
		ChatID: "oc_chat",
		InternalAction: &InternalAction{
			Type:      "question",
			Action:    "answer",
			RequestID: "req-q",
			Value:     "opt-1",
		},
	})
	out := stdin.String()
	if !strings.Contains(out, `"answers":["opt-1"],"custom":false`) {
		t.Fatalf("clicking the option labeled 1 sent %q", out)
	}
	if got := sender.lastMessage().text; !strings.Contains(got, "Answer accepted (req-q): 1") {
		t.Fatalf("message = %q", got)
	}
}
