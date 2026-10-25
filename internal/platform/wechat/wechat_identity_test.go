package wechat

import (
	"encoding/json"
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
			var msg ILinkMessage
			if err := json.Unmarshal([]byte(tc.body), &msg); err != nil {
				t.Fatal(err)
			}
			if got := msg.inboundMessageID(); got != tc.want {
				t.Fatalf("identity = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWechatIngressBusinessIdentitySeparatesSenders(t *testing.T) {
	for _, pair := range [][2]string{{"sender", "id"}, {"sender:other", "id"}, {"sender", "other:id"}, {"sender\"", "id"}} {
		id := WechatIngressBusinessID(pair[0], pair[1])
		var parts []string
		if err := json.Unmarshal([]byte(id[len("message:"):]), &parts); err != nil || len(parts) != 2 || parts[0] != pair[0] || parts[1] != pair[1] {
			t.Fatal(id, parts, err)
		}
	}
	if WechatIngressBusinessID("first", "42") == WechatIngressBusinessID("second", "42") {
		t.Fatal("different senders share a business identity")
	}
}
