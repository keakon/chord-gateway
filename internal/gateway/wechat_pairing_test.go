package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
	"github.com/keakon/chord-gateway/internal/platform/wechat"
)

func TestWechatPairedReadinessRequiresSuccess(t *testing.T) {
	// The failing cases need one backoff round to reach the next poll; shrink
	// the pacing so readiness is not gated on real timers.
	swapTestValue(t, &wechat.ILinkPollInterval, time.Millisecond)
	swapTestValue(t, &wechat.ILinkRetryDelay, time.Millisecond)
	for _, tc := range []struct {
		name  string
		body  string
		ready bool
	}{
		{"success", `{"ret":0,"errcode":0}`, true},
		{"empty", `{"ret":-1}`, false},
		{"negative_error", `{"errcode":-1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := pairedTestRouter(t)
			p := r.admission.pairing
			p.PlatformUnavailable("wechat")
			a := newTestWechatAdapter(t)
			a.SetIMConfigForTest(*r.getConfig().IMConfigByType("wechat"))
			a.SetHostForTest(r)
			a.StoreTokenForTest(&wechat.TokenData{AccountID: "bot", Token: "test-token", BaseURL: "https://example.invalid"})
			store := NewIngressStore()
			service := NewIngressService(store, r)
			if err := service.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { service.Close(); store.Close() })
			calls := 0
			a.SetTransportForTest(imageRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				if calls > 1 {
					a.Disconnect()
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			}))
			a.MonitorLoopForTest(a.AdapterContextForTest())
			p.mu.Lock()
			defer p.mu.Unlock()
			state := p.platforms["wechat"]
			if got := state != nil && state.ready; got != tc.ready {
				t.Fatalf("ready = %v, want %v", got, tc.ready)
			}
		})
	}
}

func TestWechatSendOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name              string
		status            int
		body              string
		unknown, rejected bool
	}{
		{"accepted", 200, `{"ret":0}`, false, false},
		{"rejected", 400, `{"ret":1}`, false, true},
		{"api rejection", 200, `{"ret":1}`, false, true},
		{"timeout", 408, ``, true, true},
		{"server error", 500, ``, true, true},
		{"malformed", 200, `{}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			{
				a := newTestWechatAdapter(t)
				a.StoreTokenForTest(&wechat.TokenData{AccountID: "bot", BaseURL: "https://example.invalid"})
				a.CachePairedContextForTest("bot", "chat", "context", true)
				calls := 0
				a.SetTransportForTest(imageRoundTripper(func(*http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
				}))
				err := a.SendTextContext(context.Background(), "chat", "sample")
				if (err != nil) != tc.rejected || errors.Is(err, im.ErrDeliveryOutcomeUnknown) != tc.unknown || calls != 1 {
					t.Fatalf("error=%v calls=%d", err, calls)
				}
			}
		})
	}
}

func TestWechatPairedRejectsLateAccountResponse(t *testing.T) {
	r, _ := pairedTestRouter(t)
	a := newTestWechatAdapter(t)
	a.SetIMConfigForTest(*r.getConfig().IMConfigByType("wechat"))
	a.SetHostForTest(r)
	a.StoreTokenForTest(&wechat.TokenData{AccountID: "different", Token: "new"})
	old := &wechat.TokenData{AccountID: "bot", Token: "old"}
	result := a.ReceiveIncomingMessageForTest(t.Context(), wechatTextMessage("101", "sender", "/bind /tmp/project invalid", "context"), old)
	if result.Disposition != im.ReceiveRetryable || result.Reason != "transport_identity_changed" || a.ReplyContextCountForTest() != 0 {
		t.Fatal("late response affected account", result)
	}
}

func TestResetTransferGuardPreservesIdentity(t *testing.T) {
	r, code := pairedTestRouter(t)
	dir := t.TempDir()
	dispatchPairingTest(r, im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "owner", MessageID: "first", Text: fmt.Sprintf("/bind %q %s", dir, code)})
	stateDir := filepath.Dir(r.mgr.pins.path)
	if err := os.WriteFile(filepath.Join(stateDir, "session-transfers.json.pending"), []byte(`{"invalid":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := resetPairing(context.Background(), r.mgr.store, &config.Paths{StateDir: stateDir}, "wechat"); err == nil {
		t.Fatal("reset bypassed pending transfer state")
	}
	if r.getConfig().IMConfigByType("wechat").OwnerIdentity() == nil {
		t.Fatal("guarded reset removed owner")
	}
}

func TestWechatBindPreservesBoundReplyContext(t *testing.T) {
	for _, command := range []string{"same_directory", "busy", "invalid"} {
		t.Run(command, func(t *testing.T) {
			a, s, p, _ := newWechatIngressTest(t)
			ws, err := s.router.getConfig().ResolveWorkspace("wechat", "owner")
			if err != nil {
				t.Fatal(err)
			}
			text := fmt.Sprintf("/bind %q", ws.Path)
			if command == "busy" {
				p.mu.Lock()
				p.state.Busy = true
				p.mu.Unlock()
				text = fmt.Sprintf("/bind %q", t.TempDir())
			} else if command == "invalid" {
				text = "/bind"
			}
			a.CachePairedContextForTest("bot", "owner", "task-context", true)
			result := a.ReceiveIncomingMessageForTest(t.Context(), wechatTextMessage("102", "owner", text, "bind-context"), a.TokenForTest())
			if result.Disposition != im.ReceiveAccepted {
				t.Fatal(result)
			}
			waitIngressCondition(t, func() bool { record, err := s.store.Get(result.RecordID); return err == nil && record.Terminal() })
			cacheKey := compositeKey("bot", "owner")
			entry, ok := a.ReplyContextForTest(cacheKey)
			if !ok || !entry.Bound || entry.Token != "bind-context" || !entry.Committed {
				t.Fatal("bind demoted or lost bound reply context")
			}
			entry.Expires = time.Now().Add(-time.Second)
			a.SetReplyContextForTest(cacheKey, entry)
			if err := a.SendTextContext(context.Background(), "owner", "task result"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
