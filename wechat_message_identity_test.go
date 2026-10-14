package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestWechatInboundMessageIdentity(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"number", `{"message_id":18446744073709551615}`, "18446744073709551615"},
		{"string", `{"message_id":"18446744073709551615"}`, "18446744073709551615"},
		{"priority", `{"message_id":123,"client_id":"outbound","item_list":[{"msg_id":"456"}]}`, "123"},
		{"item", `{"item_list":[{}, {"msg_id":"456"}]}`, "456"},
		{"outbound only", `{"client_id":"outbound"}`, ""},
		{"zero", `{"message_id":0}`, ""},
		{"negative", `{"message_id":-1}`, ""},
		{"overflow", `{"message_id":18446744073709551616}`, ""},
		{"fraction", `{"message_id":1.5}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var msg ilinkMessage
			if err := json.Unmarshal([]byte(tc.body), &msg); err != nil {
				t.Fatal(err)
			}
			if got := msg.inboundMessageID(); got != tc.want {
				t.Fatalf("identity = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWechatServerMessagePairsAndDeduplicates(t *testing.T) {
	r, code := pairedTestRouter(t)
	a := newTestWechatAdapter(t)
	a.imCfg = *r.getConfig().IMConfigByType("wechat")
	a.msgRouter = r
	a.token.Store(&TokenData{AccountID: "bot"})
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
		var response ilinkGetUpdatesResponse
		if err := json.Unmarshal([]byte(body), &response); err != nil {
			t.Fatal(err)
		}
		result := a.receiveIncomingMessage(t.Context(), response.Msgs[0], a.token.Load())
		if result.Disposition != ReceiveAccepted && result.Disposition != ReceiveDuplicate {
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

func TestWechatIngressBusinessIdentitySeparatesSenders(t *testing.T) {
	for _, pair := range [][2]string{{"sender", "id"}, {"sender:other", "id"}, {"sender", "other:id"}, {"sender\"", "id"}} {
		id := wechatIngressBusinessID(pair[0], pair[1])
		var parts []string
		if err := json.Unmarshal([]byte(id[len("message:"):]), &parts); err != nil || len(parts) != 2 || parts[0] != pair[0] || parts[1] != pair[1] {
			t.Fatal(id, parts, err)
		}
	}
	if wechatIngressBusinessID("first", "42") == wechatIngressBusinessID("second", "42") {
		t.Fatal("different senders share a business identity")
	}
}
