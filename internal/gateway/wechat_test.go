package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
	"github.com/keakon/chord-gateway/internal/platform/wechat"
)

// newTestWechatAdapter stays available to the root joint tests; the adapter
// itself lives in the platform package.
func newTestWechatAdapter(t *testing.T) *wechat.WechatAdapter {
	t.Helper()
	return wechat.NewAdapterForTest(t)
}

func wechatTextMessage(id, sender, text, reply string) wechat.ILinkMessage {
	return wechat.ILinkMessage{MessageID: json.Number(id), MessageType: wechat.ILinkMessageTypeUser, FromUserID: sender, ContextToken: reply, ItemList: []wechat.ILinkItem{{Type: wechat.ILinkItemTypeText, TextItem: &wechat.ILinkTextItem{Text: text}}}}
}

func wechatTaskWrites(writer *ingressTestWriter) []map[string]any {
	var tasks []map[string]any
	for _, command := range writer.snapshot() {
		if command["type"] == "send" {
			tasks = append(tasks, command)
		}
	}
	return tasks
}

func TestWechatAdapter_MessageHandling(t *testing.T) {
	t.Run("common ingress stores context and delivers normalized text", func(t *testing.T) {
		a, service, _, writer := newWechatIngressTest(t)
		msg := im.IncomingMessage{SenderID: "owner", ChatID: "owner"}
		_ = service
		result := a.ReceiveIncomingMessageForTest(a.AdapterContextForTest(), wechat.ILinkMessage{
			FromUserID: msg.SenderID, MessageID: "103", MessageType: wechat.ILinkMessageTypeUser, ContextToken: "ctx-1",
			ItemList: []wechat.ILinkItem{{Type: wechat.ILinkItemTypeText, TextItem: &wechat.ILinkTextItem{Text: "hello"}}, {Type: wechat.ILinkItemTypeText, TextItem: &wechat.ILinkTextItem{Text: "world"}}},
		}, a.TokenForTest())
		if result.Disposition != im.ReceiveAccepted {
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
		contextToken, ok := a.ReplyContextForTest(compositeKey("bot", msg.ChatID))
		if !ok || contextToken.Token != "ctx-1" {
			t.Fatalf("context token = %q", contextToken.Token)
		}
	})
}

func TestWechatAdapterReloginAndPersistence(t *testing.T) {
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
		a.SetBaseURLForTest(ts.URL)
		service, _, _, _ := ingressTestService(t)
		a.SetHostForTest(service.router)
		a.StoreTokenForTest(&wechat.TokenData{Token: "old-tok", BaseURL: ts.URL, AccountID: "bot", UserID: "old-user", SavedAt: "old"})
		t.Cleanup(a.CloseIngressAccountForTest)
		a.SaveTokenForTest()
		ctx, ctxCancel := context.WithCancel(context.Background())
		cancel = ctxCancel
		defer ctxCancel()
		a.SetConnectionForTest(ctx, ctxCancel)
		if err := a.ConnectILinkForTest(ctx); err != nil {
			t.Fatalf("connectILink error = %v", err)
		}
		if getUpdates == 0 || getQR != 1 || getStatus != 1 {
			t.Fatalf("getUpdates=%d getQR=%d getStatus=%d", getUpdates, getQR, getStatus)
		}
		tok := a.TokenForTest()
		if tok == nil || tok.Token != "new-tok" || tok.AccountID != "bot" {
			t.Fatalf("token after re-login = %#v", tok)
		}
		loaded := a.LoadTokenForTest()
		if loaded == nil || loaded.Token != "new-tok" {
			t.Fatalf("persisted token after re-login = %#v", loaded)
		}
	})
}

func TestConsoleAdapterPrintsLocally(t *testing.T) {
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
}
