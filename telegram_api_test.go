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

	"github.com/keakon/chord-gateway/internal/im"
)

func testTelegramAPI(t *testing.T, handler http.HandlerFunc) *telegramHTTPAPI {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	api := newTelegramAPI("123:secret-token", nil)
	// Swapping in the test server's client must keep the redirect policy that
	// newTelegramAPI applies, so redirect-replay tests stay meaningful.
	api.baseURL, api.client = server.URL, im.MessageHTTPClient(server.Client())
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
			if string(payload["limit"]) != strconv.Itoa(telegramPollLimit) {
				t.Errorf("limit = %s", payload["limit"])
			}
			if string(payload["timeout"]) != strconv.Itoa(telegramLongPollSeconds) {
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
	if !errors.Is(err, im.ErrDeliveryOutcomeUnknown) || calls.Load() != 1 {
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
		{`{"update_id":9007199254740993,"callback_query":{"id":"a","from":{"id":9007199254740993}}}`, &b},
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

// getMe, getWebhookInfo and getUpdates describe the poll loop itself, so a
// non-429 ok:false from them is a method-level refusal that never self-heals.
// A stale proxy answering 200 with an error body is the realistic shape.
func TestTelegramIngressMethodRejectionIsTerminal(t *testing.T) {
	for _, method := range []string{"getMe", "getWebhookInfo", "getUpdates"} {
		t.Run(method, func(t *testing.T) {
			api := testTelegramAPI(t, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, `{"ok":false,"error_code":400,"description":"bad request"}`)
			})
			var err error
			switch method {
			case "getMe":
				_, err = api.GetMe(t.Context())
			case "getWebhookInfo":
				_, err = api.GetWebhookInfo(t.Context())
			case "getUpdates":
				_, err = api.GetUpdates(t.Context(), nil, telegramLongPollSeconds)
			}
			var apiErr *telegramAPIError
			if !errors.As(err, &apiErr) || apiErr.Category != "method_rejected" {
				t.Fatalf("%s rejection = %v", method, err)
			}
			var b telegramBackoff
			if _, retry := b.failed(apiErr); retry {
				t.Fatal("method rejection retried forever")
			}
		})
	}
	// A 429 stays retryable, and a send keeps its delivery-level category.
	api := testTelegramAPI(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":false,"error_code":429,"parameters":{"retry_after":3}}`)
	})
	_, err := api.GetUpdates(t.Context(), nil, telegramLongPollSeconds)
	var apiErr *telegramAPIError
	if !errors.As(err, &apiErr) || apiErr.Category != "rate_limited" {
		t.Fatalf("429 = %v", err)
	}
	if _, err := api.SendMessage(t.Context(), 1, "test"); !errors.As(err, &apiErr) || apiErr.Category != "rate_limited" {
		t.Fatalf("send 429 = %v", err)
	}
	api = testTelegramAPI(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":false,"error_code":400}`)
	})
	if _, err := api.SendMessage(t.Context(), 1, "test"); !errors.As(err, &apiErr) || apiErr.Category != "api_rejected" {
		t.Fatalf("send 400 = %v", err)
	}
	// A 429 without a retry delay still describes the poll loop. A read-only
	// method backs off; only a send keeps the unknown-outcome category.
	api = testTelegramAPI(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":false,"error_code":429}`)
	})
	if _, err := api.GetUpdates(t.Context(), nil, telegramLongPollSeconds); !errors.As(err, &apiErr) || apiErr.Category != "rate_limited" {
		t.Fatalf("bare 429 getUpdates = %v", err)
	}
	var backoff telegramBackoff
	if _, retry := backoff.failed(apiErr); !retry {
		t.Fatal("bare 429 stopped the poller")
	}
	if _, err := api.SendMessage(t.Context(), 1, "test"); !errors.As(err, &apiErr) || apiErr.Category != "api_rejected" {
		t.Fatalf("bare 429 send = %v", err)
	}
}

// A long poll outlives the ordinary request timeout, and the read must not
// outlive the poll: a stalled connection would otherwise hold the poll loop
// open with no bound at all.
func TestTelegramLongPollReadIsBoundedByRequestTimeout(t *testing.T) {
	previous := telegramPollTimeout
	telegramPollTimeout = 150 * time.Millisecond
	t.Cleanup(func() { telegramPollTimeout = previous })
	api := testTelegramAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"result":[`)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	})
	done := make(chan error, 1)
	go func() { _, err := api.GetUpdates(t.Context(), nil, telegramLongPollSeconds); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled long poll body was accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled long poll body was never aborted")
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
	// A 404 on a non-identity method is a method-level refusal, not a
	// transient rejection, so it must terminate instead of retrying forever.
	if !errors.As(err, &apiErr) || apiErr.Category != "method_rejected" {
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

// Telegram copies a media caption into `text`, so presence detection decides
// admission: a document with "summarize this" is a media update, not a task.
func TestTelegramMediaCaptionIsNotATextTask(t *testing.T) {
	adapter := &TelegramAdapter{accountID: "99", username: "ThisBot", now: realTelegramClock}
	payloads := []struct{ name, field, value string }{
		{"photo", "photo", `[{"file_id":"file"}]`},
		{"document", "document", `{"file_id":"file","file_name":"notes.txt"}`},
		{"video", "video", `{"file_id":"file","duration":3}`},
		{"audio", "audio", `{"file_id":"file","duration":3}`},
		{"voice", "voice", `{"file_id":"file","duration":3}`},
		{"animation", "animation", `{"file_id":"file"}`},
		{"sticker", "sticker", `{"file_id":"file","emoji":"x"}`},
		{"video_note", "video_note", `{"file_id":"file","duration":3}`},
		{"contact", "contact", `{"phone_number":"+100","first_name":"A"}`},
		{"location", "location", `{"latitude":1,"longitude":2}`},
		{"venue", "venue", `{"location":{"latitude":1,"longitude":2},"title":"t"}`},
		{"poll", "poll", `{"id":"1","question":"q","options":[],"total_voter_count":0}`},
		{"dice", "dice", `{"emoji":"x","value":1}`},
		{"game", "game", `{"title":"t","description":"d"}`},
		{"invoice", "invoice", `{"title":"t","description":"d","currency":"USD","total_amount":1}`},
		{"successful_payment", "successful_payment", `{"currency":"USD","total_amount":1,"invoice_payload":"p"}`},
		{"story", "story", `{"id":1}`},
		{"passport_data", "passport_data", `[{"type":"phone_number"}]`},
		{"web_app_data", "web_app_data", `{"button_text":"b","data":"d"}`},
		// Presence decides, even when the value carries no payload.
		{"null_document", "document", `null`},
	}
	for _, tc := range payloads {
		t.Run(tc.name, func(t *testing.T) {
			var update telegramUpdate
			data := `{"update_id":1,"message":{"message_id":2,"chat":{"id":7,"type":"private"},"from":{"id":7},"text":"summarize this","date":` + strconv.FormatInt(time.Now().Unix(), 10) + `,"` + tc.field + `":` + tc.value + `}}`
			if err := json.Unmarshal([]byte(data), &update); err != nil {
				t.Fatal(err)
			}
			if !update.Message.hasMedia {
				t.Fatalf("media field %s was not detected", tc.field)
			}
			_, reason := adapter.normalizeUpdate(update)
			if reason != "unsupported_content" {
				t.Fatalf("reason=%q want=%q", reason, "unsupported_content")
			}
		})
	}
	// An ordinary text message with the same caption still becomes a task.
	var plain telegramUpdate
	if err := json.Unmarshal([]byte(`{"update_id":1,"message":{"message_id":2,"chat":{"id":7,"type":"private"},"from":{"id":7},"text":"summarize this","date":`+strconv.FormatInt(time.Now().Unix(), 10)+`}}`), &plain); err != nil {
		t.Fatal(err)
	}
	e, reason := adapter.normalizeUpdate(plain)
	if reason != "" || e.Message.Text != "summarize this" {
		t.Fatalf("plain text rejected: reason=%q message=%+v", reason, e.Message)
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
	if !errors.Is(err, im.ErrDeliveryOutcomeUnknown) || calls.Load() != 1 {
		t.Fatalf("redirect replay calls = %d, err = %v", calls.Load(), err)
	}
}
