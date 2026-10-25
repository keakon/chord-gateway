package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
	"github.com/keakon/chord-gateway/internal/platform/wechat"
)

func TestWechatServerMessagePairsAndDeduplicates(t *testing.T) {
	r, code := pairedTestRouter(t)
	a := newTestWechatAdapter(t)
	a.SetIMConfigForTest(*r.getConfig().IMConfigByType("wechat"))
	a.SetHostForTest(r)
	a.StoreTokenForTest(&wechat.TokenData{AccountID: "bot"})
	store := NewIngressStore()
	service := NewIngressService(store, r)
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close(); store.Close() })
	text, _ := json.Marshal(fmt.Sprintf("/bind %q %s", t.TempDir(), code))
	// Replay a full getupdates response, with no client_id and a uint64 ID
	// beyond float64's exact range. Every subsequent wire encoding is the
	// same server delivery and must not produce another binding reply.
	for _, identity := range []string{`"message_id":18446744073709551615`, `"message_id":"18446744073709551615"`, `"client_id":"different-outbound-id","message_id":18446744073709551615`} {
		body := fmt.Sprintf(`{"ret":0,"msgs":[{%s,"from_user_id":"owner","message_type":1,"context_token":"reply-context","item_list":[{"type":1,"text_item":{"text":%s}}]}]}`, identity, text)
		var response wechat.ILinkGetUpdatesResponse
		if err := json.Unmarshal([]byte(body), &response); err != nil {
			t.Fatal(err)
		}
		result := a.ReceiveIncomingMessageForTest(t.Context(), response.Msgs[0], a.TokenForTest())
		if result.Disposition != im.ReceiveAccepted && result.Disposition != im.ReceiveDuplicate {
			t.Fatal(result)
		}
		waitIngressCondition(t, func() bool {
			record, err := store.Get(result.RecordID)
			return err == nil && record.State == "completed"
		})
	}
	waitIngressCondition(t, func() bool {
		adapter := r.currentAdapter().(*pairingTestAdapter)
		adapter.mu.Lock()
		defer adapter.mu.Unlock()
		return len(adapter.messages) == 1
	})
	owner := r.getConfig().IMConfigByType("wechat").OwnerIdentity()
	if owner == nil || owner.UserID != "owner" {
		t.Fatal("server-ID-only message did not pair")
	}
	adapter := r.currentAdapter().(*pairingTestAdapter)
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if len(adapter.messages) != 1 {
		t.Fatalf("duplicate replies: %v", adapter.messages)
	}
	if len(r.mgr.procs) != 0 {
		t.Fatal("pairing or redelivery started a process")
	}
}
