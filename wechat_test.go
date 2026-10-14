package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
)

func newTestWechatAdapter(t *testing.T) *WechatAdapter {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	storageDir := filepath.Join(dir, "wechat")
	a := &WechatAdapter{
		imCfg:         config.IMAdapterConfig{Wechat: &config.WechatConfig{}},
		storageDir:    storageDir,
		tokenFile:     filepath.Join(storageDir, "token.json"),
		httpClient:    &http.Client{Timeout: 5 * time.Second},
		replyContexts: make(map[string]wechatReplyContext),
		ctx:           ctx,
		cancel:        cancel,
	}
	t.Cleanup(a.closeIngressAccount)
	a.token.Store(&TokenData{Token: "sample-token", AccountID: "bot"})
	return a
}

func TestIlinkGetUpdatesResponseStatusKind(t *testing.T) {
	cases := []struct {
		name string
		resp *ilinkGetUpdatesResponse
		want ilinkGetUpdatesStatus
	}{
		{name: "ok", resp: &ilinkGetUpdatesResponse{Ret: 0}, want: ilinkGetUpdatesStatusOK},
		{name: "empty", resp: &ilinkGetUpdatesResponse{Ret: -1, ErrCode: 0, Msgs: nil}, want: ilinkGetUpdatesStatusEmpty},
		{name: "session expired by errcode", resp: &ilinkGetUpdatesResponse{ErrCode: ilinkSessionExpired}, want: ilinkGetUpdatesStatusSessionExpired},
		{name: "error", resp: &ilinkGetUpdatesResponse{Ret: 1, ErrCode: 500}, want: ilinkGetUpdatesStatusError},
		{name: "nil", resp: nil, want: ilinkGetUpdatesStatusError},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.resp.statusKind(); got != tt.want {
				t.Fatalf("statusKind() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWechatAdapter_HelperMethods(t *testing.T) {
	t.Run("baseURL prefers config then token and trims slash", func(t *testing.T) {
		a := newTestWechatAdapter(t)
		a.imCfg.Wechat.BaseURL = "https://example.com/"
		if got := a.baseURL(); got != "https://example.com" {
			t.Fatalf("baseURL from config = %q", got)
		}
		a.imCfg.Wechat.BaseURL = ""
		a.token.Store(&TokenData{BaseURL: "https://token.example.com/"})
		if got := a.baseURL(); got != "https://token.example.com" {
			t.Fatalf("baseURL from token = %q", got)
		}
		a.token.Store(nil)
		if got := a.baseURL(); got != ilinkDefaultBaseURL {
			t.Fatalf("baseURL empty = %q", got)
		}
	})

	t.Run("botType defaults and tokenString", func(t *testing.T) {
		a := newTestWechatAdapter(t)
		a.token.Store(nil)
		if got := a.botType(); got != ilinkDefaultBotType {
			t.Fatalf("botType = %q", got)
		}
		if got := a.tokenString(); got != "" {
			t.Fatalf("tokenString nil = %q", got)
		}
		a.token.Store(&TokenData{Token: "abc"})
		if got := a.tokenString(); got != "abc" {
			t.Fatalf("tokenString = %q", got)
		}
	})

	t.Run("buildHeaders includes auth when token exists", func(t *testing.T) {
		a := newTestWechatAdapter(t)
		a.token.Store(&TokenData{Token: "abc"})
		h := a.buildHeaders()
		if got := h.Get("Content-Type"); got != "application/json" {
			t.Fatalf("content-type = %q", got)
		}
		if got := h.Get("AuthorizationType"); got != "ilink_bot_token" {
			t.Fatalf("AuthorizationType = %q", got)
		}
		if got := h.Get("Authorization"); got != "Bearer abc" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := h.Get("X-WECHAT-UIN"); got == "" {
			t.Fatal("X-WECHAT-UIN should be set")
		}
	})

	t.Run("randomWechatUIN returns base64 text", func(t *testing.T) {
		got := randomWechatUIN()
		if got == "" || strings.Contains(got, " ") {
			t.Fatalf("randomWechatUIN = %q", got)
		}
	})

	t.Run("Type returns wechat and Disconnect calls cancel", func(t *testing.T) {
		a := newTestWechatAdapter(t)
		called := false
		a.cancel = func() { called = true }
		if got := a.Type(); got != "wechat" {
			t.Fatalf("Type = %q", got)
		}
		a.Disconnect()
		if !called {
			t.Fatal("Disconnect did not call cancel")
		}
	})
}

func TestWechatAdapterTrustedLoginBaseURL(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		raw        string
		want       string
		wantErr    bool
	}{
		{name: "unconfigured HTTPS endpoint", raw: " https://api.example.com/ ", want: "https://api.example.com"},
		{name: "configured endpoint matches case-insensitively", configured: "HTTPS://API.EXAMPLE.COM/root/", raw: "https://api.example.com/v2/", want: "https://api.example.com/v2"},
		{name: "userinfo rejected", raw: "https://user@api.example.com", wantErr: true},
		{name: "missing host rejected", raw: "https:///path", wantErr: true},
		{name: "unconfigured HTTP rejected", raw: "http://api.example.com", wantErr: true},
		{name: "configured scheme mismatch rejected", configured: "https://api.example.com", raw: "http://api.example.com", wantErr: true},
		{name: "configured host mismatch rejected", configured: "https://api.example.com", raw: "https://evil.example.com", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newTestWechatAdapter(t)
			a.imCfg.Wechat.BaseURL = tt.configured
			got, err := a.trustedLoginBaseURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("trustedLoginBaseURL(%q) = %q, want error", tt.raw, got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("trustedLoginBaseURL(%q) = %q, %v; want %q", tt.raw, got, err, tt.want)
			}
		})
	}
}

func TestWechatAdapter_APIHelpers(t *testing.T) {
	t.Run("apiGet and apiPost send expected request", func(t *testing.T) {
		var gotMethod, gotPath, gotAuth string
		var gotBody map[string]any
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path + "?" + r.URL.RawQuery
			gotAuth = r.Header.Get("Authorization")
			if r.Method == http.MethodPost {
				defer r.Body.Close()
				if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
					t.Fatalf("decode request body: %v", err)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
		}))
		defer ts.Close()

		a := newTestWechatAdapter(t)
		a.imCfg.Wechat.BaseURL = ts.URL
		a.token.Store(&TokenData{Token: "abc"})

		resp, err := a.apiGet("foo/bar")
		if err != nil {
			t.Fatalf("apiGet error = %v", err)
		}
		_ = resp.Body.Close()
		if gotMethod != http.MethodGet || gotPath != "/foo/bar?" || gotAuth != "Bearer abc" {
			t.Fatalf("GET method=%q path=%q auth=%q", gotMethod, gotPath, gotAuth)
		}

		resp, err = a.apiPost("foo/post", map[string]any{"x": "y"})
		if err != nil {
			t.Fatalf("apiPost error = %v", err)
		}
		_ = resp.Body.Close()
		if gotMethod != http.MethodPost || gotPath != "/foo/post?" {
			t.Fatalf("POST method=%q path=%q", gotMethod, gotPath)
		}
		if gotBody["x"] != "y" {
			t.Fatalf("body = %#v", gotBody)
		}
	})

	t.Run("apiPost requires account", func(t *testing.T) {
		a := newTestWechatAdapter(t)
		a.token.Store(nil)
		if _, err := a.apiPost("foo", nil); err == nil {
			t.Fatal("missing account accepted")
		}
	})
}

func TestWechatAdapter_QRAndUpdatesHelpers(t *testing.T) {
	t.Run("getBotQRCode and getQRCodeStatus decode JSON", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "get_bot_qrcode"):
				_, _ = io.WriteString(w, `{"qrcode":"qr-id","qrcode_img_content":"https://qr"}`)
			case strings.Contains(r.URL.Path, "get_qrcode_status"):
				_, _ = io.WriteString(w, `{"status":"confirmed","bot_token":"tok","baseurl":"https://api","ilink_bot_id":"bot","ilink_user_id":"u1"}`)
			default:
				t.Fatalf("unexpected path %s", r.URL.Path)
			}
		}))
		defer ts.Close()

		a := newTestWechatAdapter(t)
		a.imCfg.Wechat.BaseURL = ts.URL
		qr, err := a.getBotQRCode()
		if err != nil || qr.QRCode != "qr-id" || qr.QRCodeImgContent != "https://qr" {
			t.Fatalf("getBotQRCode = %#v, %v", qr, err)
		}
		status, err := a.getQRCodeStatus("qr-id")
		if err != nil || status.Status != "confirmed" || status.BotToken != "tok" {
			t.Fatalf("getQRCodeStatus = %#v, %v", status, err)
		}
	})

	t.Run("getUpdates posts sync buf and decodes JSON", func(t *testing.T) {
		var gotReq ilinkGetUpdatesRequest
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer r.Body.Close()
			if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			_, _ = io.WriteString(w, `{"ret":0,"get_updates_buf":"next","msgs":[]}`)
		}))
		defer ts.Close()

		a := newTestWechatAdapter(t)
		a.imCfg.Wechat.BaseURL = ts.URL
		a.syncBuf = "prev"
		resp, err := a.getUpdates()
		if err != nil {
			t.Fatalf("getUpdates error = %v", err)
		}
		if gotReq.GetUpdatesBuf != "prev" || gotReq.BaseInfo.ChannelVersion != ilinkChannelVersion {
			t.Fatalf("request = %#v", gotReq)
		}
		if resp.GetUpdatesBuf != "next" {
			t.Fatalf("response = %#v", resp)
		}
	})
}

func TestWechatAdapter_MessageHandling(t *testing.T) {
	t.Run("common ingress stores context and delivers normalized text", func(t *testing.T) {
		a, service, _, writer := newWechatIngressTest(t)
		msg := IncomingMessage{SenderID: "owner", ChatID: "owner"}
		_ = service
		result := a.receiveIncomingMessage(a.ctx, ilinkMessage{
			FromUserID: msg.SenderID, MessageID: "103", MessageType: ilinkMessageTypeUser, ContextToken: "ctx-1",
			ItemList: []ilinkItem{{Type: ilinkItemTypeText, TextItem: &ilinkTextItem{Text: "hello"}}, {Type: ilinkItemTypeText, TextItem: &ilinkTextItem{Text: "world"}}},
		}, a.token.Load())
		if result.Disposition != ReceiveAccepted {
			t.Fatal(result)
		}
		waitIngressCondition(t, func() bool {
			for _, entry := range writer.snapshot() {
				if entry["type"] == "send" && entry["content"] == "hello\nworld" {
					return true
				}
			}
			return false
		})
		a.mu.Lock()
		contextToken := a.replyContexts[compositeKey("bot", msg.ChatID)].token
		a.mu.Unlock()
		if contextToken != "ctx-1" {
			t.Fatalf("context token = %q", contextToken)
		}
	})

	t.Run("ignores non-user and empty-text messages", func(t *testing.T) {
		a := newTestWechatAdapter(t)
		a.receiveIncomingMessage(a.ctx, ilinkMessage{MessageType: ilinkMessageTypeBot, FromUserID: "u1"}, a.token.Load())
		a.receiveIncomingMessage(a.ctx, ilinkMessage{MessageType: ilinkMessageTypeUser, FromUserID: "u1", ItemList: []ilinkItem{{Type: 99}}}, a.token.Load())
		if len(a.replyContexts) != 0 {
			t.Fatalf("context tokens = %#v", a.replyContexts)
		}
	})
}

func TestWechatAdapter_SendMessageAndSendILinkText(t *testing.T) {
	t.Run("sendMessage rejects non-JSON 200 and reports session expiry", func(t *testing.T) {
		var hit int
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hit++
			if hit == 1 {
				_, _ = io.WriteString(w, `ok`)
				return
			}
			_, _ = io.WriteString(w, `{"errcode":-14,"errmsg":"expired"}`)
		}))
		defer ts.Close()

		a := newTestWechatAdapter(t)
		a.imCfg.Wechat.BaseURL = ts.URL
		if err := a.sendMessage(t.Context(), a.token.Load(), ilinkSendMessageRequest{}); err == nil {
			t.Fatal("sendMessage accepted an unknown non-JSON outcome")
		}
		err := a.sendMessage(t.Context(), a.token.Load(), ilinkSendMessageRequest{})
		if err == nil || !strings.Contains(err.Error(), "errcode=-14") {
			t.Fatalf("sendMessage session error = %v", err)
		}
	})

	t.Run("SendText sends split segments with context token", func(t *testing.T) {
		var bodies []ilinkSendMessageRequest
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer r.Body.Close()
			var body ilinkSendMessageRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			bodies = append(bodies, body)
			_, _ = io.WriteString(w, `{"errcode":0}`)
		}))
		defer ts.Close()

		a := newTestWechatAdapter(t)
		a.imCfg.Wechat.BaseURL = ts.URL
		a.token.Store(&TokenData{AccountID: "bot"})
		a.cachePairedContext("bot", "chat-1", "ctx-1", true)
		longText := strings.Repeat("a", ilinkMaxMessageLen+10)
		if err := a.SendText("chat-1", longText); err != nil {
			t.Fatalf("SendText error = %v", err)
		}
		if len(bodies) < 2 {
			t.Fatalf("expected split segments, got %d", len(bodies))
		}
		if bodies[0].Msg.ContextToken != "ctx-1" || bodies[0].Msg.ToUserID != "chat-1" || bodies[0].Msg.MessageType != ilinkMessageTypeBot {
			t.Fatalf("first body = %#v", bodies[0])
		}
	})

	t.Run("ConsoleAdapter prints locally", func(t *testing.T) {
		a := newConsoleAdapter(nil)
		defer a.cancel()
		origStdout := os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		os.Stdout = w
		defer func() { os.Stdout = origStdout }()
		if err := a.SendText("chat-1", "hello"); err != nil {
			t.Fatalf("SendText error = %v", err)
		}
		_ = w.Close()
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if got := string(data); !strings.Contains(got, "[chat-1] hello") {
			t.Fatalf("stdout = %q", got)
		}
	})
}

func TestWechatAdapter_PersistenceAndSplitText(t *testing.T) {
	t.Run("token persistence round trip and invalid files", func(t *testing.T) {
		a := newTestWechatAdapter(t)
		a.token.Store(&TokenData{Token: "tok", BaseURL: "https://api", AccountID: "acc", UserID: "u1", SavedAt: "now"})
		a.saveToken()
		loaded := a.loadToken()
		if loaded == nil || loaded.Token != "tok" || loaded.AccountID != "acc" {
			t.Fatalf("loaded token = %#v", loaded)
		}
		if err := os.WriteFile(a.tokenFile, []byte("not-json"), 0o600); err != nil {
			t.Fatalf("write invalid token file: %v", err)
		}
		if got := a.loadToken(); got != nil {
			t.Fatalf("invalid token file should return nil, got %#v", got)
		}
	})

	t.Run("clearToken removes persisted token", func(t *testing.T) {
		a := newTestWechatAdapter(t)
		a.token.Store(&TokenData{Token: "tok", BaseURL: "https://api", AccountID: "acc", UserID: "u1", SavedAt: "now"})
		a.saveToken()
		if _, err := os.Stat(a.tokenFile); err != nil {
			t.Fatalf("token file should exist before clearToken: %v", err)
		}
		if filepath.Base(filepath.Dir(a.tokenFile)) != "wechat" {
			t.Fatalf("default token path = %q, want a wechat subdirectory", a.tokenFile)
		}
		a.clearToken()
		if a.token.Load() != nil {
			t.Fatalf("token=%#v after clearToken", a.token.Load())
		}
		if _, err := os.Stat(a.tokenFile); !os.IsNotExist(err) {
			t.Fatalf("token file should be removed, stat error = %v", err)
		}
	})

	t.Run("custom token path persists outside default storage dir", func(t *testing.T) {
		a := newTestWechatAdapter(t)
		customPath := filepath.Join(t.TempDir(), "secrets", "wechat-token.json")
		a.tokenFile = customPath
		a.token.Store(&TokenData{Token: "custom", BaseURL: "https://api", AccountID: "acc", UserID: "u1", SavedAt: "now"})
		a.saveToken()
		if _, err := os.Stat(customPath); err != nil {
			t.Fatalf("custom token file should exist: %v", err)
		}
		loaded := a.loadToken()
		if loaded == nil || loaded.Token != "custom" {
			t.Fatalf("loaded custom token = %#v", loaded)
		}
	})

	t.Run("connectILink re-logins when saved token is expired", func(t *testing.T) {
		var cancel context.CancelFunc
		var serverURL string
		var getUpdates, getQR, getStatus int
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "getupdates"):
				getUpdates++
				if getUpdates == 1 {
					_, _ = io.WriteString(w, `{"errcode":-14,"errmsg":"expired"}`)
					return
				}
				_, _ = io.WriteString(w, `{"ret":0,"msgs":[]}`)
				go cancel()
			case strings.Contains(r.URL.Path, "get_bot_qrcode"):
				getQR++
				_, _ = io.WriteString(w, `{"qrcode":"qr-id","qrcode_img_content":"https://qr"}`)
			case strings.Contains(r.URL.Path, "get_qrcode_status"):
				getStatus++
				_, _ = io.WriteString(w, `{"status":"confirmed","bot_token":"new-tok","baseurl":"`+serverURL+`","ilink_bot_id":"bot","ilink_user_id":"u1"}`)
			default:
				t.Fatalf("unexpected path %s", r.URL.Path)
			}
		}))
		serverURL = ts.URL
		defer ts.Close()

		a := newTestWechatAdapter(t)
		a.imCfg.Wechat.BaseURL = ts.URL
		service, _, _, _ := ingressTestService(t)
		a.msgRouter = service.router
		a.token.Store(&TokenData{Token: "old-tok", BaseURL: ts.URL, AccountID: "bot", UserID: "old-user", SavedAt: "old"})
		t.Cleanup(a.closeIngressAccount)
		a.saveToken()
		ctx, ctxCancel := context.WithCancel(context.Background())
		cancel = ctxCancel
		defer ctxCancel()
		a.ctx = ctx
		if err := a.connectILink(ctx); err != nil {
			t.Fatalf("connectILink error = %v", err)
		}
		if getUpdates == 0 || getQR != 1 || getStatus != 1 {
			t.Fatalf("getUpdates=%d getQR=%d getStatus=%d", getUpdates, getQR, getStatus)
		}
		tok := a.token.Load()
		if tok == nil || tok.Token != "new-tok" || tok.AccountID != "bot" {
			t.Fatalf("token after re-login = %#v", tok)
		}
		loaded := a.loadToken()
		if loaded == nil || loaded.Token != "new-tok" {
			t.Fatalf("persisted token after re-login = %#v", loaded)
		}
	})

	t.Run("NewWechatAdapter loads credentials without starting legacy writers", func(t *testing.T) {
		dir := t.TempDir()
		wechatDir := filepath.Join(dir, "wechat")
		if err := os.MkdirAll(wechatDir, 0o700); err != nil {
			t.Fatalf("create wechat dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(wechatDir, "token.json"), []byte(`{"token":"tok","baseUrl":"https://api","accountId":"acc","userId":"u1","savedAt":"now"}`), 0o600); err != nil {
			t.Fatalf("write token.json: %v", err)
		}
		if err := os.WriteFile(filepath.Join(wechatDir, "sync-buf.json"), []byte(`{"version":1,"account_id":"acc","get_updates_buf":"sync-1"}`), 0o600); err != nil {
			t.Fatalf("write sync-buf.json: %v", err)
		}
		paths := testPaths(t)
		paths.StateDir = dir
		a, err := NewWechatAdapter(
			&config.Config{IMs: []config.IMAdapterConfig{{Wechat: &config.WechatConfig{}}}},
			config.IMAdapterConfig{Wechat: &config.WechatConfig{}},
			paths,
			nil,
		)
		if err != nil {
			t.Fatalf("NewWechatAdapter error = %v", err)
		}
		tok := a.token.Load()
		if tok == nil || tok.Token != "tok" || a.syncBuf != "" {
			t.Fatalf("adapter token=%#v syncBuf=%q", tok, a.syncBuf)
		}
	})

	t.Run("splitText respects maxLen and newline preference", func(t *testing.T) {
		if got := splitText("short", 10); len(got) != 1 || got[0] != "short" {
			t.Fatalf("split short = %#v", got)
		}
		got := splitText("abcde\n12345\nxyz", 6)
		if len(got) < 2 {
			t.Fatalf("split long = %#v", got)
		}
		for _, seg := range got {
			if len(seg) > 6 {
				t.Fatalf("segment too long: %#v", got)
			}
		}
	})

	t.Run("sleep returns when context is cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		sleepCtx(ctx, time.Second)
		if time.Since(start) > 100*time.Millisecond {
			t.Fatalf("sleep took too long after cancel")
		}
	})
}
