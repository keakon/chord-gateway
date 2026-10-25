package wecom

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

func wecomProtocolFrame(t *testing.T, mutate func(map[string]any)) []byte {
	t.Helper()
	body := map[string]any{"aibotid": "example-bot", "msgid": "example-message", "from": map[string]any{"userid": "example-owner"}, "chattype": "single", "msgtype": "text", "text": map[string]any{"content": "hello"}}
	if mutate != nil {
		mutate(body)
	}
	raw, err := json.Marshal(map[string]any{"cmd": "aibot_msg_callback", "headers": map[string]any{"req_id": "delivery-one"}, "body": body})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestWecomProtocolIdentityAndTargets(t *testing.T) {
	for _, kind := range []string{"private", "group"} {
		t.Run(kind, func(t *testing.T) {
			content := "@Example Bot hello @Example Bot /approve request"
			raw := wecomProtocolFrame(t, func(b map[string]any) {
				b["text"] = map[string]any{"content": content}
				if kind == "group" {
					b["chattype"], b["chatid"] = "group", "group:with/slash"
				}
			})
			received := time.Unix(1800000001, 123)
			got, err := NormalizeWecomCallback(raw, "example-bot", received)
			if err != nil {
				t.Fatal(err)
			}
			msg := got.Message
			if msg.IMType != "wecom" || msg.AccountID != "example-bot" || msg.SenderID != "example-owner" || msg.MessageID != "message:example-message" || msg.ChatKind != kind || msg.Text != content || msg.InternalAction != nil || got.DeliveryID != "delivery-one" || !got.SourceTime.Equal(received) || !got.ReceivedAt.Equal(received) {
				t.Fatalf("unexpected normalization: %+v", got)
			}
			target := "example-owner"
			if kind == "group" {
				target = "group:with/slash"
			}
			encoded, err := EncodeWecomTarget(kind, target)
			if err != nil || encoded != msg.ChatID {
				t.Fatalf("target encoding: %s %v", encoded, err)
			}
			decoded, err := DecodeWecomTarget(encoded)
			wantType := 1
			if kind == "group" {
				wantType = 2
			}
			if err != nil || decoded.ChatID != target || decoded.ChatType != wantType {
				t.Fatalf("target decoding: %+v %v", decoded, err)
			}
		})
	}
	private, _ := EncodeWecomTarget("private", "same")
	group, _ := EncodeWecomTarget("group", "same")
	if private == group {
		t.Fatal("private and group target scopes collide")
	}
	foreign, _ := im.EncodeScopedID("target-v1:", "slack-thread", "example")
	for _, target := range []string{"raw-chat", "", foreign, private + "="} {
		if _, err := DecodeWecomTarget(target); err == nil {
			t.Fatalf("accepted target %q", target)
		}
	}
}

func TestWecomProtocolClassifiesCallbacks(t *testing.T) {
	// Events and non-text messages are deliberately outside the text command
	// contract; they are ignored without being reported as lost input.
	unsupported := map[string]func(map[string]any){
		"assistant":      func(b map[string]any) { b["compat_info"] = map[string]any{"source": "ai_assistant"} },
		"channel":        func(b map[string]any) { b["compat_info"] = map[string]any{"source": "channel"} },
		"unknown_source": func(b map[string]any) { b["compat_info"] = map[string]any{"source": "future"} },
		"empty_compat":   func(b map[string]any) { b["compat_info"] = map[string]any{} },
		"null_compat":    func(b map[string]any) { b["compat_info"] = nil },
		"voice": func(b map[string]any) {
			b["msgtype"] = "voice"
			b["voice"] = map[string]any{"content": "/approve example"}
		},
		"mixed": func(b map[string]any) { b["msgtype"] = "mixed" },
	}
	for name, mutate := range unsupported {
		t.Run("unsupported/"+name, func(t *testing.T) {
			if _, err := NormalizeWecomCallback(wecomProtocolFrame(t, mutate), "example-bot", time.Now()); !errors.Is(err, ErrWecomUnsupportedCallback) {
				t.Fatalf("unsupported callback error: %v", err)
			}
		})
	}
	// A callback that claims to be user text but fails validation must be
	// visible to the caller instead of vanishing without evidence.
	invalid := map[string]func(map[string]any){
		"missing_bot":     func(b map[string]any) { delete(b, "aibotid") },
		"missing_message": func(b map[string]any) { delete(b, "msgid") },
		"missing_sender":  func(b map[string]any) { delete(b, "from") },
		"empty_sender":    func(b map[string]any) { b["from"] = map[string]any{"userid": " "} },
		"number_sender":   func(b map[string]any) { b["from"] = map[string]any{"userid": 123} },
		"missing_kind":    func(b map[string]any) { delete(b, "chattype") },
		"unknown_kind":    func(b map[string]any) { b["chattype"] = "private" },
		"missing_group":   func(b map[string]any) { b["chattype"] = "group" },
		"nested_group_alias": func(b map[string]any) {
			b["chattype"] = "group"
			b["group"] = map[string]any{"chatid": "example-group"}
		},
		"missing_text":     func(b map[string]any) { delete(b, "text") },
		"missing_content":  func(b map[string]any) { b["text"] = map[string]any{} },
		"null_content":     func(b map[string]any) { b["text"] = map[string]any{"content": nil} },
		"empty_content":    func(b map[string]any) { b["text"] = map[string]any{"content": ""} },
		"blank_content":    func(b map[string]any) { b["text"] = map[string]any{"content": " \t\n"} },
		"large_text":       func(b map[string]any) { b["text"] = map[string]any{"content": strings.Repeat("x", 64<<10+1)} },
		"large_message_id": func(b map[string]any) { b["msgid"] = strings.Repeat("x", im.MaxScopedIDBytes) },
	}
	for name, mutate := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			if _, err := NormalizeWecomCallback(wecomProtocolFrame(t, mutate), "example-bot", time.Now()); !errors.Is(err, ErrWecomInvalidCallback) {
				t.Fatalf("invalid callback error: %v", err)
			}
		})
	}
	for _, kind := range []string{"text", "voice"} {
		raw := wecomProtocolFrame(t, func(b map[string]any) { b["aibotid"] = "other-bot"; b["msgtype"] = kind })
		if _, err := NormalizeWecomCallback(raw, "example-bot", time.Now()); !errors.Is(err, ErrWecomAccountMismatch) {
			t.Fatalf("account mismatch was not fatal: %v", err)
		}
	}
	event := []byte(`{"cmd":"aibot_event_callback","body":{"event":{"eventtype":"enter_chat"}}}`)
	if _, err := NormalizeWecomCallback(event, "example-bot", time.Now()); !errors.Is(err, ErrWecomUnsupportedCallback) {
		t.Fatalf("event callback classification: %v", err)
	}
	for _, raw := range [][]byte{[]byte(`{"cmd":"aibot_msg_callback"} {}`), []byte(strings.Repeat(" ", 1<<20+1)), {0xff}} {
		if _, err := NormalizeWecomCallback(raw, "example-bot", time.Now()); !errors.Is(err, ErrWecomInvalidCallback) {
			t.Fatalf("invalid frame classification: %v", err)
		}
	}
}

func TestWecomProtocolRejectionReasons(t *testing.T) {
	received := time.Unix(1800000002, 0)
	skewed := wecomProtocolFrame(t, func(b map[string]any) { b["create_time"] = int64(1700000000) })
	_, err := NormalizeWecomCallback(skewed, "example-bot", received)
	if !errors.Is(err, ErrWecomInvalidCallback) || !errors.Is(err, ErrWecomSourceTimeOutOfRange) {
		t.Fatalf("out-of-range source time classification: %v", err)
	}
	if reason := wecomCallbackRejectionReason(err); reason != "source_time_out_of_range" {
		t.Fatalf("out-of-range source time reason: %q", reason)
	}
	malformed := wecomProtocolFrame(t, func(b map[string]any) { delete(b, "text") })
	_, err = NormalizeWecomCallback(malformed, "example-bot", received)
	if reason := wecomCallbackRejectionReason(err); reason != "invalid_callback" {
		t.Fatalf("invalid callback reason: %q (%v)", reason, err)
	}
	blank := wecomProtocolFrame(t, func(b map[string]any) { b["text"] = map[string]any{"content": " \t\n"} })
	_, err = NormalizeWecomCallback(blank, "example-bot", received)
	if reason := wecomCallbackRejectionReason(err); reason != "invalid_input" {
		t.Fatalf("blank text reason: %q (%v)", reason, err)
	}
}

func TestWecomProtocolSourceTime(t *testing.T) {
	received := time.Unix(1800000002, 345)
	for _, tc := range []struct {
		name    string
		value   any
		missing bool
		valid   bool
		want    time.Time
	}{
		{"seconds", int64(1800000000), false, true, time.Unix(1800000000, 0)},
		{"milliseconds", int64(1800000000999), false, true, time.Unix(1800000000, 0)},
		{"missing", nil, true, true, received},
		{"null", nil, false, false, time.Time{}},
		{"quoted", "1800000000", false, false, time.Time{}},
		{"fraction", 1800000000.5, false, false, time.Time{}},
		{"negative", -1, false, false, time.Time{}},
		{"eleven_digits", int64(18000000000), false, false, time.Time{}},
		{"twelve_digits", int64(180000000000), false, false, time.Time{}},
		{"fourteen_digits", int64(18000000000000), false, false, time.Time{}},
		// Out-of-range platform timestamps are rejected like every other
		// adapter and the shared store (-24h/+5m window) so a skewed
		// create_time is visible instead of silently rewritten.
		{"near_past", int64(1799996402), false, true, time.Unix(1799996402, 0)},
		{"near_future", int64(1800000242), false, true, time.Unix(1800000242, 0)},
		{"rejected_past", int64(1700000000), false, false, time.Time{}},
		{"rejected_future", int64(1800003602), false, false, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := wecomProtocolFrame(t, func(b map[string]any) {
				if tc.missing {
					delete(b, "create_time")
				} else {
					b["create_time"] = tc.value
				}
			})
			got, err := NormalizeWecomCallback(raw, "example-bot", received)
			if (err == nil) != tc.valid || tc.valid && !got.SourceTime.Equal(tc.want) {
				t.Fatalf("source time: %s, %v", got.SourceTime, err)
			}
		})
	}
}
