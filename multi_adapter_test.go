package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/keakon/chord-gateway/config"
)

func TestMultiAdapter_Connect(t *testing.T) {
	t.Run("all adapters connect", func(t *testing.T) {
		wechat := &stubIMAdapter{typ: "wechat"}
		feishu := &stubIMAdapter{typ: "feishu"}
		m := &MultiAdapter{adapters: []IMAdapter{wechat, feishu}}
		if err := m.Connect(); err != nil {
			t.Fatalf("Connect() error = %v", err)
		}
		if wechat.connectCalls != 1 || feishu.connectCalls != 1 {
			t.Fatalf("connect calls = wechat:%d feishu:%d", wechat.connectCalls, feishu.connectCalls)
		}
	})

	t.Run("joins connect errors", func(t *testing.T) {
		wantWX := errors.New("wx down")
		wantFS := errors.New("fs down")
		wechat := &stubIMAdapter{typ: "wechat", connectFunc: func() error { return wantWX }}
		feishu := &stubIMAdapter{typ: "feishu", connectFunc: func() error { return wantFS }}
		m := &MultiAdapter{adapters: []IMAdapter{wechat, feishu}}
		err := m.Connect()
		if err == nil {
			t.Fatal("expected error")
		}
		if !errors.Is(err, wantWX) || !errors.Is(err, wantFS) {
			t.Fatalf("joined error = %v", err)
		}
	})
}

func TestMultiAdapter_SendText(t *testing.T) {
	t.Run("routes via router adapter type", func(t *testing.T) {
		wechat := &stubIMAdapter{typ: "wechat"}
		feishu := &stubIMAdapter{typ: "feishu"}
		r := &NotificationRouter{mgr: newTestChordManager(&config.Config{IMs: []config.IMAdapterConfig{{Feishu: &config.FeishuConfig{ChatBindings: map[string]string{"feishu-chat": "ws1"}}}}, Workspaces: []config.Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}), lastKeyChatID: map[string]string{(processKey{workspaceID: "ws1", imType: "wechat", chatID: "wechat-chat"}).String(): "wechat-chat"}}
		m := &MultiAdapter{adapters: []IMAdapter{wechat, feishu}, router: r}
		if err := m.SendText("feishu-chat", "hello"); err != nil {
			t.Fatalf("SendText() error = %v", err)
		}
		if len(feishu.sentMessages()) != 1 || len(wechat.sentMessages()) != 0 {
			t.Fatalf("wechat=%v feishu=%v", wechat.sentMessages(), feishu.sentMessages())
		}
	})

	t.Run("falls back to first successful adapter", func(t *testing.T) {
		wantErr := errors.New("wx fail")
		wechat := &stubIMAdapter{typ: "wechat", sendFunc: func(chatID, text string) error { return wantErr }}
		feishu := &stubIMAdapter{typ: "feishu"}
		m := &MultiAdapter{adapters: []IMAdapter{wechat, feishu}}
		if err := m.SendText("chat", "hello"); err != nil {
			t.Fatalf("SendText() error = %v", err)
		}
		if len(wechat.sentMessages()) != 1 || len(feishu.sentMessages()) != 1 {
			t.Fatalf("wechat=%v feishu=%v", wechat.sentMessages(), feishu.sentMessages())
		}
	})

	t.Run("returns joined errors when all fail", func(t *testing.T) {
		errWX := errors.New("wx fail")
		errFS := errors.New("fs fail")
		wechat := &stubIMAdapter{typ: "wechat", sendFunc: func(chatID, text string) error { return errWX }}
		feishu := &stubIMAdapter{typ: "feishu", sendFunc: func(chatID, text string) error { return errFS }}
		m := &MultiAdapter{adapters: []IMAdapter{wechat, feishu}}
		err := m.SendText("chat", "hello")
		if err == nil || !errors.Is(err, errWX) || !errors.Is(err, errFS) {
			t.Fatalf("SendText() error = %v", err)
		}
	})

	t.Run("no adapters configured", func(t *testing.T) {
		m := &MultiAdapter{}
		err := m.SendText("chat", "hello")
		if err == nil || err.Error() != "no IM adapters configured" {
			t.Fatalf("SendText() error = %v", err)
		}
	})
}

func TestMultiAdapter_SendTextViaFindDisconnectAndStartLogin(t *testing.T) {
	wechat := &stubIMAdapter{typ: "wechat", startLoginFunc: func() (string, error) { return "https://wx-login", nil }}
	feishu := &stubIMAdapter{typ: "feishu"}
	m := &MultiAdapter{adapters: []IMAdapter{wechat, feishu}}

	if err := m.SendTextVia("wechat", "chat-1", "hello"); err != nil {
		t.Fatalf("SendTextVia() error = %v", err)
	}
	if got := wechat.lastMessage(); got.chatID != "chat-1" || got.text != "hello" {
		t.Fatalf("wechat last message = %#v", got)
	}
	if err := m.SendTextVia("unknown", "chat-1", "hello"); err == nil || err.Error() != `adapter "unknown" not found` {
		t.Fatalf("SendTextVia unknown error = %v", err)
	}

	if got := m.FindAdapterByType("feishu"); got != feishu {
		t.Fatalf("FindAdapterByType(feishu) = %#v", got)
	}
	if got := m.FindAdapterByType("lark"); got != nil {
		t.Fatalf("FindAdapterByType(lark) = %#v", got)
	}
	if got := m.FindAdapterByType("unknown"); got != nil {
		t.Fatalf("FindAdapterByType(unknown) = %#v", got)
	}

	if len(m.adapters) != 2 || m.adapters[0] != wechat || m.adapters[1] != feishu {
		t.Fatalf("adapters = %#v", m.adapters)
	}

	if got := m.Type(); got != "multi" {
		t.Fatalf("Type() = %q", got)
	}

	qrURL, err := m.StartLogin()
	if !errors.Is(err, ErrLoginNotSupported) || qrURL != "" {
		t.Fatalf("StartLogin() = %q, %v", qrURL, err)
	}

	m.Disconnect()
	if wechat.disconnectCalls != 1 || feishu.disconnectCalls != 1 {
		t.Fatalf("disconnect calls = wechat:%d feishu:%d", wechat.disconnectCalls, feishu.disconnectCalls)
	}
}

func TestMultiAdapter_StartLoginWithoutWechat(t *testing.T) {
	m := &MultiAdapter{adapters: []IMAdapter{&stubIMAdapter{typ: "feishu"}}}
	qrURL, err := m.StartLogin()
	if !errors.Is(err, ErrLoginNotSupported) || qrURL != "" {
		t.Fatalf("StartLogin() = %q, %v", qrURL, err)
	}
}

func TestMultiAdapter_BroadcastTextExcept(t *testing.T) {
	wechat := &stubIMAdapter{typ: "wechat"}
	feishu := &stubIMAdapter{typ: "feishu"}
	console := &stubIMAdapter{typ: "console"}
	r := &NotificationRouter{mgr: newTestChordManager(&config.Config{IMs: []config.IMAdapterConfig{{Feishu: &config.FeishuConfig{ChatBindings: map[string]string{"feishu-fallback": "ws1"}}}}, Workspaces: []config.Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}), lastKeyChatID: map[string]string{(processKey{workspaceID: "ws1", imType: "wechat", chatID: "wechat-chat"}).String(): "wechat-chat"}}
	m := &MultiAdapter{adapters: []IMAdapter{wechat, feishu, console}, router: r}

	m.BroadcastTextExcept("wechat", map[string]string{"feishu": "feishu-direct"}, "cross notify")
	if len(wechat.sentMessages()) != 0 {
		t.Fatalf("excluded adapter should not receive messages: %v", wechat.sentMessages())
	}
	if got := feishu.lastMessage().chatID; got != "feishu-direct" {
		t.Fatalf("feishu chatID = %q", got)
	}
	if len(console.sentMessages()) != 0 {
		t.Fatalf("console should be skipped without chatID: %v", console.sentMessages())
	}
}

func TestNewMultiAdapter_ErrorsForUnsupportedAdapter(t *testing.T) {
	cfg := &config.Config{IMs: []config.IMAdapterConfig{{}}}
	_, err := NewMultiAdapter(cfg, testPaths(t), nil)
	if err == nil || err.Error() != "create  adapter: unsupported IM type: " {
		t.Fatalf("NewMultiAdapter() error = %v", err)
	}
}

func TestNewMultiAdapter_CreatesConfiguredAdapters(t *testing.T) {
	paths := testPaths(t)
	cfg := &config.Config{IMs: []config.IMAdapterConfig{{Wechat: &config.WechatConfig{}}, {Feishu: &config.FeishuConfig{AppID: "app", AppSecret: "secret"}}}}
	m, err := NewMultiAdapter(cfg, paths, nil)
	if err != nil {
		t.Fatalf("NewMultiAdapter() error = %v", err)
	}
	defer m.Disconnect()
	if len(m.adapters) != 2 {
		t.Fatalf("len(adapters) = %d", len(m.adapters))
	}
	if got := fmt.Sprintf("%T,%T", m.adapters[0], m.adapters[1]); got != "*main.WechatAdapter,*main.FeishuAdapter" {
		t.Fatalf("adapter types = %s", got)
	}
}
