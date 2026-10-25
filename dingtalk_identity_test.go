package main

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

func dingtalkIdentityTestConfig() *config.DingtalkConfig {
	return &config.DingtalkConfig{ClientID: "test-client", ClientSecret: "test-secret", CorpID: "test-corp", RobotCode: "test-robot"}
}

func dingtalkIdentityTestCallback() dingtalkCallback {
	callback := dingtalkCallback{MsgID: "test-message", ConversationType: "1", ConversationID: "test-conversation",
		SenderStaffID: "test-staff", SenderCorpID: "test-corp", ChatbotCorpID: "test-corp", RobotCode: "test-robot", MsgType: "text"}
	callback.Text.Content = " \t\n/bind \"/project/path @literal\" nonce\n"
	return callback
}

func dingtalkIdentityTestPayload(t *testing.T, callback dingtalkCallback) string {
	t.Helper()
	data, err := json.Marshal(callback)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertDingtalkIdentityCategory(t *testing.T, err error, category string, terminal bool) {
	t.Helper()
	var identityErr *dingtalkIdentityError
	if !errors.As(err, &identityErr) || identityErr.Category != category || identityErr.Terminal() != terminal || err.Error() != category {
		t.Fatalf("error = %v, want %s (terminal %v)", err, category, terminal)
	}
}

func TestDingtalkTargetsRoundTripAndScope(t *testing.T) {
	for _, kind := range []string{"private", "group"} {
		raw := "original:conversation/id?opaque=1"
		target, err := dingtalkEncodeTarget("test-corp", kind, raw)
		if err != nil {
			t.Fatal(err)
		}
		decodedKind, decodedRaw, err := dingtalkDecodeTarget("test-corp", target)
		if err != nil || decodedKind != kind || decodedRaw != raw {
			t.Fatalf("decoded %s/%s: %v", decodedKind, decodedRaw, err)
		}
		for _, bad := range []string{target + "=", "legacy-chat-id"} {
			_, _, err = dingtalkDecodeTarget("test-corp", bad)
			assertDingtalkIdentityCategory(t, err, "invalid_target", false)
		}
		_, _, err = dingtalkDecodeTarget("other-corp", target)
		assertDingtalkIdentityCategory(t, err, "invalid_target", false)
	}
	other, _ := im.EncodeScopedID("target-v1:", "slack-thread", "test-corp", "raw")
	_, _, err := dingtalkDecodeTarget("test-corp", other)
	assertDingtalkIdentityCategory(t, err, "invalid_target", false)
}

func TestDingtalkNormalizeIdentityAndText(t *testing.T) {
	cfg := dingtalkIdentityTestConfig()
	callback := dingtalkIdentityTestCallback()
	msg, err := normalizeDingtalkCallback(cfg, dingtalkIdentityTestPayload(t, callback))
	if err != nil {
		t.Fatal(err)
	}
	account, _ := dingtalkAccountID(cfg)
	target, _ := dingtalkEncodeTarget(cfg.CorpID, "private", callback.SenderStaffID)
	if msg.AccountID != account || msg.ChatID != target || msg.ChatKind != "private" || msg.IMType != "dingtalk" || msg.SenderID != callback.SenderStaffID || msg.MessageID != "message:test-message" || msg.Text != "/bind \"/project/path @literal\" nonce\n" {
		t.Fatalf("normalized: %+v", msg)
	}
	callback.ConversationType, callback.IsInAtList = "2", true
	callback.ConversationID = "original/group:id?opaque=1"
	msg, err = normalizeDingtalkCallback(cfg, dingtalkIdentityTestPayload(t, callback))
	if err != nil {
		t.Fatal(err)
	}
	kind, raw, err := dingtalkDecodeTarget(cfg.CorpID, msg.ChatID)
	if err != nil || kind != "group" || raw != callback.ConversationID {
		t.Fatalf("group target %s/%s: %v", kind, raw, err)
	}
}

func TestDingtalkNormalizeRejectionsAreSanitized(t *testing.T) {
	for _, test := range []struct {
		name, category string
		mutate         func(*dingtalkCallback)
		terminal       bool
	}{
		{"missing staff", "sender_identity_unsupported", func(c *dingtalkCallback) { c.SenderStaffID = "" }, false},
		{"external sender", "sender_identity_unsupported", func(c *dingtalkCallback) { c.SenderCorpID = "other-corp" }, false},
		{"wrong enterprise", "tenant_mismatch", func(c *dingtalkCallback) { c.ChatbotCorpID = "other-corp" }, true},
		{"wrong robot", "tenant_mismatch", func(c *dingtalkCallback) { c.RobotCode = "other-robot" }, true},
		{"missing msg id", "invalid_message_id", func(c *dingtalkCallback) { c.MsgID = "" }, false},
		{"missing type", "unsupported_conversation", func(c *dingtalkCallback) { c.ConversationType = "" }, false},
		{"unknown type", "unsupported_conversation", func(c *dingtalkCallback) { c.ConversationType = "3" }, false},
		{"rich text", "unsupported_message", func(c *dingtalkCallback) { c.MsgType = "richText" }, false},
		{"group without mention", "group_mention_required", func(c *dingtalkCallback) { c.ConversationType = "2" }, false},
		{"missing group id", "invalid_target", func(c *dingtalkCallback) { c.ConversationType = "2"; c.IsInAtList = true; c.ConversationID = "" }, false},
		{"oversized text", "message_too_large", func(c *dingtalkCallback) { c.Text.Content = strings.Repeat("x", 64*1024+1) }, false},
		{"empty text", "invalid_input", func(c *dingtalkCallback) { c.Text.Content = "" }, false},
		{"blank text", "invalid_input", func(c *dingtalkCallback) { c.Text.Content = " \t\n" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			callback := dingtalkIdentityTestCallback()
			callback.Text.Content = "sensitive callback body"
			test.mutate(&callback)
			msg, err := normalizeDingtalkCallback(dingtalkIdentityTestConfig(), dingtalkIdentityTestPayload(t, callback))
			assertDingtalkIdentityCategory(t, err, test.category, test.terminal)
			if msg.Text != "" {
				t.Fatalf("rejection retained body: %+v", msg)
			}
			if test.category == "unsupported_message" || test.category == "message_too_large" || test.category == "group_mention_required" || test.category == "invalid_input" {
				if msg.MessageID == "" || msg.ChatID == "" || msg.AccountID == "" || msg.SenderID == "" {
					t.Fatalf("rejection omitted authenticated business identity: %+v", msg)
				}
			} else if !reflect.DeepEqual(msg, im.IncomingMessage{}) {
				t.Fatalf("identity rejection retained message: %+v", msg)
			}
		})
	}
	callback := dingtalkIdentityTestCallback()
	callback.SenderStaffID = ""
	payload := strings.TrimSuffix(dingtalkIdentityTestPayload(t, callback), "}") + `,"senderId":"fallback","isAdmin":true}`
	_, err := normalizeDingtalkCallback(dingtalkIdentityTestConfig(), payload)
	assertDingtalkIdentityCategory(t, err, "sender_identity_unsupported", false)
	for _, payload := range []string{"{bad-json-with-sensitive-content", string([]byte{0xff}), "null", "[]"} {
		_, err := normalizeDingtalkCallback(dingtalkIdentityTestConfig(), payload)
		assertDingtalkIdentityCategory(t, err, "invalid_callback", false)
	}
}

func TestDingtalkIdentityProbeOnlyPrivateAndNeverChangesConfig(t *testing.T) {
	cfg := dingtalkIdentityTestConfig()
	before := *cfg
	callback := dingtalkIdentityTestCallback()
	if err := validateDingtalkIdentityProbe(cfg, &callback); err != nil {
		t.Fatal(err)
	}
	callback.ConversationType = "2"
	assertDingtalkIdentityCategory(t, validateDingtalkIdentityProbe(cfg, &callback), "identity_probe_requires_private", false)
	callback.ConversationType, callback.RobotCode = "1", "other-robot"
	assertDingtalkIdentityCategory(t, validateDingtalkIdentityProbe(cfg, &callback), "tenant_mismatch", true)
	if !reflect.DeepEqual(*cfg, before) {
		t.Fatal("probe changed configured identity")
	}
}

func TestDingtalkClientLockExcludesDifferentConfiguredIdentities(t *testing.T) {
	cfg := dingtalkIdentityTestConfig()
	resources, err := dingtalkLockResources(cfg)
	if err != nil || len(resources) != 2 || !sort.StringsAreSorted(resources) {
		t.Fatalf("resources %v: %v", resources, err)
	}
	first, err := acquireDingtalkAccountLocks(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	other := *cfg
	other.CorpID, other.RobotCode = "other-corp", "other-robot"
	if duplicate, err := acquireDingtalkAccountLocks(&other); err == nil {
		duplicate.Close()
		t.Fatal("same client acquired a second lock with a different tenant")
	}
	other.ClientID = "other-client"
	independent, err := acquireDingtalkAccountLocks(&other)
	if err != nil {
		t.Fatal(err)
	}
	independent.Close()
	account, _ := dingtalkAccountID(cfg)
	if duplicate, err := im.AcquireIMAccountLock("dingtalk", account); err == nil {
		duplicate.Close()
		t.Fatal("public account lock was not acquired")
	}
}

func TestDingtalkLiteralGroupCallback(t *testing.T) {
	// Independent wire fixture following the official Stream protocol example:
	// https://open-dingtalk.github.io/developerpedia/docs/learn/stream/protocol/
	// Group mentions are represented by isInAtList/atUsers; text has no bot prefix.
	const payload = `{
		"conversationId":"test-group", "conversationType":"2",
		"chatbotCorpId":"test-corp", "chatbotUserId":"test-bot-user",
		"senderCorpId":"test-corp", "senderStaffId":"test-staff",
		"senderId":"test-user", "robotCode":"test-robot", "msgId":"test-message",
		"isInAtList":true, "atUsers":[{"dingtalkId":"test-bot-user"}],
		"msgtype":"text", "text":{"content":" /bind /project/path"}
	}`
	msg, err := normalizeDingtalkCallback(dingtalkIdentityTestConfig(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if msg.ChatKind != "group" || msg.SenderID != "test-staff" || parseIMCommand(msg.Text).Type != "bind" {
		t.Fatalf("group command did not normalize: %+v", msg)
	}
	// Literal mentions in the user's task must not be stripped speculatively.
	msg, err = normalizeDingtalkCallback(dingtalkIdentityTestConfig(), strings.Replace(payload, "/bind /project/path", "@someone review this", 1))
	if err != nil || msg.Text != "@someone review this" {
		t.Fatal(msg.Text, err)
	}
}
