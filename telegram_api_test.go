package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testTelegramAPI(t *testing.T, handler http.HandlerFunc) *telegramHTTPAPI {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	api := newTelegramAPI("123:secret-token")
	api.baseURL, api.client = server.URL, server.Client()
	return api
}
func TestTelegramTypedAPIRequests(t *testing.T) {
	api := testTelegramAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("invalid request method/headers")
		}
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			io.WriteString(w, `{"ok":true,"result":{"id":9007199254740993,"is_bot":true,"username":"testbot","new_field":1}}`)
		case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
			io.WriteString(w, `{"ok":true,"result":{"url":""}}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			if string(payload["allowed_updates"]) != `["message"]` {
				t.Errorf("allowed_updates = %s", payload["allowed_updates"])
			}
			if string(payload["timeout"]) != "25" {
				t.Errorf("timeout = %s", payload["timeout"])
			}
			if _, exists := payload["offset"]; exists {
				t.Error("idle polling included offset")
			}
			io.WriteString(w, `{"ok":true,"result":[{"update_id":10,"message":{"message_id":3,"chat":{"id":-1009007199254740993,"type":"supergroup"},"from":{"id":9007199254740993},"text":"hello"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			if len(payload) != 3 || string(payload["chat_id"]) != "-1009007199254740993" || string(payload["link_preview_options"]) != `{"is_disabled":true}` {
				t.Errorf("send payload = %v", payload)
			}
			io.WriteString(w, `{"ok":true,"result":{"message_id":123,"chat":{"id":-1009007199254740993}}}`)
		default:
			t.Errorf("unexpected method")
			w.WriteHeader(404)
		}
	})
	user, err := api.GetMe(t.Context())
	if err != nil || user.ID != 9007199254740993 {
		t.Fatalf("identity = %+v, %v", user, err)
	}
	if _, err := api.GetWebhookInfo(t.Context()); err != nil {
		t.Fatal(err)
	}
	updates, err := api.GetUpdates(t.Context(), nil, 25)
	if err != nil || len(updates) != 1 || updates[0].Message.Chat.ID != -1009007199254740993 {
		t.Fatalf("updates = %+v, %v", updates, err)
	}
	if _, err := api.SendMessage(t.Context(), -1009007199254740993, "plain ``` @text"); err != nil {
		t.Fatal(err)
	}
}
func TestTelegramAPIErrorClassificationAndRedaction(t *testing.T) {
	cases := []struct {
		name, body, category string
		status               int
	}{
		{"credentials", `{"ok":false,"error_code":401,"description":"secret-token"}`, "credentials", 401},
		{"credentials_malformed", `secret-token`, "credentials", 401},
		{"poller", `{"ok":false,"error_code":409,"description":"secret-token"}`, "poller_conflict", 409},
		{"poller_malformed", `secret-token`, "poller_conflict", 409},
		{"rate", `{"ok":false,"error_code":429,"parameters":{"retry_after":2}}`, "rate_limited", 429},
		{"forbidden", `{"ok":false,"error_code":403}`, "forbidden", 403},
		{"migrated", `{"ok":false,"error_code":400,"parameters":{"migrate_to_chat_id":-321}}`, "chat_migrated", 400},
		{"missing_receipt", `{"ok":true,"result":{"chat":{"id":1}}}`, "unknown_send", 200},
		{"wrong_target", `{"ok":true,"result":{"message_id":2,"chat":{"id":2}}}`, "unknown_send", 200},
		{"http_status", `{"ok":true,"result":{"message_id":2,"chat":{"id":1}}}`, "unknown_send", 500},
		{"bare_429", `secret-token`, "unknown_send", 429},
		{"null", `{"ok":true,"result":null}`, "unknown_send", 200},
		{"no_ok", `{"result":{"message_id":2,"chat":{"id":1}}}`, "unknown_send", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := testTelegramAPI(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) })
			_, err := api.SendMessage(t.Context(), 1, "test")
			var apiErr *telegramAPIError
			if !errors.As(err, &apiErr) || apiErr.Category != tc.category {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), api.baseURL) {
				t.Fatalf("unsanitized error: %v", err)
			}
			if tc.name == "rate" && apiErr.RetryAfter != 2*time.Second {
				t.Errorf("retry_after = %v", apiErr.RetryAfter)
			}
		})
	}
}
func TestTelegramAPIBodyLimitAndRequiredIdentity(t *testing.T) {
	for _, body := range []string{strings.Repeat("x", telegramResponseMaxBytes+1), `{"ok":true,"result":{"id":1,"is_bot":false,"username":"x"}}`, `{"ok":true,"result":{"is_bot":true,"username":"x"}}`} {
		api := testTelegramAPI(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
		if _, err := api.GetMe(t.Context()); err == nil {
			t.Fatal("accepted invalid identity or oversized body")
		}
	}
	api := testTelegramAPI(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true,"result":[{"message":{}}]}`)
	})
	if _, err := api.GetUpdates(t.Context(), nil, 0); err == nil {
		t.Fatal("accepted missing update identity")
	}
}
func TestTelegramAPICancellationDoesNotReplaySend(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	api := testTelegramAPI(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := api.SendMessage(ctx, 1, "test"); done <- err }()
	<-entered
	cancel()
	err := <-done
	if !errors.Is(err, errDeliveryOutcomeUnknown) || calls.Load() != 1 {
		t.Fatalf("calls = %d, err = %v", calls.Load(), err)
	}
	// A cancelled request never reaches the server.
	cancelled, stop := context.WithCancel(t.Context())
	stop()
	_, err = api.SendMessage(cancelled, 1, "test")
	if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("pre-cancelled send = %v", err)
	}
}

func TestTelegramWebhookURLPresence(t *testing.T) {
	for _, result := range []string{`{}`, `{"url":null}`} {
		api := testTelegramAPI(t, func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"ok":true,"result":`+result+`}`)
		})
		if _, err := api.GetWebhookInfo(t.Context()); err == nil {
			t.Fatal("missing webhook URL was treated as empty")
		}
	}
}

func TestTelegramUnsupportedUpdateFingerprint(t *testing.T) {
	var a, b, conflict telegramUpdate
	for _, sample := range []struct {
		value string
		out   *telegramUpdate
	}{
		{`{"update_id":9007199254740993,"callback_query":{"id":"a","from":{"id":9007199254740993}}}`, &a},
		{`{"callback_query":{"from":{"id":9007199254740993},"id":"a"},"update_id":9007199254740993}`, &b},
		{`{"update_id":9007199254740993,"callback_query":{"id":"b","from":{"id":9007199254740993}}}`, &conflict},
	} {
		if err := json.Unmarshal([]byte(sample.value), sample.out); err != nil {
			t.Fatal(err)
		}
	}
	if a.UpdateID != 9007199254740993 || a.receivedDigest == "" || a.receivedDigest != b.receivedDigest || a.receivedDigest == conflict.receivedDigest {
		t.Fatal("unsupported update fingerprint lost identity or payload differences")
	}
}

func TestTelegramGetMe404StopsInvalidCredentials(t *testing.T) {
	for _, body := range []string{`{"ok":false,"error_code":404}`, `Not Found`} {
		api := testTelegramAPI(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, body)
		})
		_, err := api.GetMe(t.Context())
		var apiErr *telegramAPIError
		if !errors.As(err, &apiErr) || apiErr.Category != "credentials" {
			t.Fatalf("getMe invalid token: %v", err)
		}
	}
	api := testTelegramAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"ok":false,"error_code":404}`)
	})
	_, err := api.GetWebhookInfo(t.Context())
	var apiErr *telegramAPIError
	if !errors.As(err, &apiErr) || apiErr.Category != "api_rejected" {
		t.Fatalf("non-identity method 404: %v", err)
	}
}

func TestTelegramMessageThreadIDPresence(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		present     bool
		value       int64
	}{
		{"absent", "", false, 0},
		{"zero", `,"message_thread_id":0`, true, 0},
		{"positive", `,"message_thread_id":123`, true, 123},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var update telegramUpdate
			data := `{"update_id":1,"message":{"message_id":2,"chat":{"id":7,"type":"private"},"from":{"id":7},"text":"hello","date":` + strconv.FormatInt(time.Now().Unix(), 10) + tc.field + `}}`
			if err := json.Unmarshal([]byte(data), &update); err != nil {
				t.Fatal(err)
			}
			if update.Message == nil || update.Message.hasMessageThreadID != tc.present || update.Message.MessageThreadID != tc.value {
				t.Fatalf("thread field presence/value mismatch: %+v", update.Message)
			}
			adapter := &TelegramAdapter{accountID: "99", username: "ThisBot", now: realTelegramClock}
			_, reason := adapter.normalizeUpdate(update)
			wantReason := ""
			if tc.present {
				wantReason = "unsupported_topic"
			}
			if reason != wantReason {
				t.Fatalf("topic admission reason = %q, want %q", reason, wantReason)
			}
		})
	}
}
func TestTelegramAPIBlocksRedirectReplay(t *testing.T) {
	var calls atomic.Int32
	api := testTelegramAPI(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", "/redirect")
		w.WriteHeader(307)
	})
	_, err := api.SendMessage(t.Context(), 1, "test")
	if !errors.Is(err, errDeliveryOutcomeUnknown) || calls.Load() != 1 {
		t.Fatalf("redirect replay calls = %d, err = %v", calls.Load(), err)
	}
}
