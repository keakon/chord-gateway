package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/keakon/chord-gateway/internal/im"
)

type telegramSenderTestAPI struct {
	send func(context.Context, int64, string) (Message, error)
}

func (*telegramSenderTestAPI) GetMe(context.Context) (User, error) {
	return User{}, nil
}
func (*telegramSenderTestAPI) GetWebhookInfo(context.Context) (WebhookInfo, error) {
	return WebhookInfo{}, nil
}
func (*telegramSenderTestAPI) GetUpdates(context.Context, *int64, int) ([]Update, error) {
	return nil, nil
}
func (a *telegramSenderTestAPI) SendMessage(ctx context.Context, chat int64, text string) (Message, error) {
	return a.send(ctx, chat, text)
}

type telegramSendRoundTripFunc func(*http.Request) (*http.Response, error)

func (f telegramSendRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestTelegramUTF16Segmentation(t *testing.T) {
	sender, err := NewTelegramTextSender(&telegramSenderTestAPI{})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{strings.Repeat("a", 3500), strings.Repeat("😀", 1750), strings.Repeat("😀", 1751), strings.Repeat("a", 3400) + "\n" + strings.Repeat("😀", 100)} {
		parts, err := sender.Segments(text)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(parts, "") != text {
			t.Fatal("segmentation lost text")
		}
		for _, part := range parts {
			if !utf8.ValidString(part) || im.UTF16Units(part) > TextBudget {
				t.Fatal("invalid segment")
			}
		}
		if strings.Contains(text, "\n") && !strings.HasSuffix(parts[0], "\n") {
			t.Fatal("newline was not preferred")
		}
	}
}
func TestTelegramSendUnknownOutcomeNeverRetries(t *testing.T) {
	var calls atomic.Int32
	sender, err := NewTelegramTextSender(&telegramSenderTestAPI{send: func(context.Context, int64, string) (Message, error) {
		calls.Add(1)
		return Message{}, &APIError{Method: "sendMessage", Category: "unknown_send", Cause: im.ErrDeliveryOutcomeUnknown}
	}})
	if err != nil {
		t.Fatal(err)
	}
	err = sender.Send(t.Context(), nil, nil, "1", "test", false)
	if !errors.Is(err, im.ErrDeliveryOutcomeUnknown) || calls.Load() != 1 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
}

func TestTelegramPartialFailureStopsActualSend(t *testing.T) {
	for _, tc := range []struct {
		name, body, category string
		status               int
		unknown              bool
	}{
		{name: "forbidden", status: http.StatusForbidden, body: `{"ok":false,"error_code":403}`, category: "forbidden"},
		{name: "API rejection", status: http.StatusBadRequest, body: `{"ok":false,"error_code":400}`, category: "api_rejected"},
		{name: "chat migrated", status: http.StatusBadRequest, body: `{"ok":false,"error_code":400,"parameters":{"migrate_to_chat_id":-321}}`, category: "chat_migrated"},
		{name: "server error", status: http.StatusBadGateway, body: `{"ok":true,"result":{"message_id":1,"chat":{"id":1}}}`, category: "unknown_send", unknown: true},
		{name: "bare rate limit", status: http.StatusTooManyRequests, body: ``, category: "unknown_send", unknown: true},
		{name: "missing receipt", status: http.StatusOK, body: `{"ok":true,"result":{"chat":{"id":1}}}`, category: "unknown_send", unknown: true},
		{name: "malformed receipt", status: http.StatusOK, body: `invalid`, category: "unknown_send", unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var sent []string
				api := NewAPI("123:sample-token", nil)
				api.client.Transport = telegramSendRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					defer req.Body.Close()
					var body struct {
						ChatID int64  `json:"chat_id"`
						Text   string `json:"text"`
					}
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if body.ChatID != 1 || !strings.HasSuffix(req.URL.Path, "/sendMessage") {
						t.Fatalf("send changed target or method: chat=%d path=%s", body.ChatID, req.URL.Path)
					}
					sent = append(sent, body.Text)
					status, response := http.StatusOK, `{"ok":true,"result":{"message_id":1,"chat":{"id":1}}}`
					if len(sent) == 2 {
						status, response = tc.status, tc.body
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
				})
				sender, err := NewTelegramTextSender(api)
				if err != nil {
					t.Fatal(err)
				}
				a := NewTelegramAdapterForTest("account-1", "samplebot", nil, nil, nil)
				defer a.Disconnect()
				a.ready, a.sender = true, sender
				first, second, third := strings.Repeat("a", TextBudget), strings.Repeat("b", TextBudget), strings.Repeat("c", TextBudget)
				err = a.SendTextContext(t.Context(), "1", first+second+third)
				var partial *im.IMTextSendError
				var apiErr *APIError
				if !errors.As(err, &partial) || partial.AcceptedSegments != 1 || partial.Category != tc.category || !errors.As(err, &apiErr) || apiErr.Category != tc.category {
					t.Fatalf("partial send = %v, want category %s after one accepted segment", err, tc.category)
				}
				if errors.Is(err, im.ErrDeliveryOutcomeUnknown) != tc.unknown {
					t.Fatalf("unknown outcome evidence disagrees with category: %v", err)
				}
				if !reflect.DeepEqual(sent, []string{first, second}) {
					t.Fatalf("sent %d segments, want first and second only in original order", len(sent))
				}
				snapshot := a.TextSendTargetSnapshot("1")
				metrics := a.TextSendMetrics()
				if snapshot.LastAcceptedSegments != 1 || snapshot.LastFailureCategory != tc.category || metrics.Failures != 1 || metrics.PartialFailures != 1 || metrics.RateLimitWaits != 0 {
					t.Fatalf("partial diagnostics = %+v, metrics = %+v", snapshot, metrics)
				}
				if strings.Contains(err.Error(), "sample-token") || strings.Contains(apiErr.Error(), "sample-token") {
					t.Fatal("send error exposed credentials")
				}
			})
		})
	}
}

func TestTelegramSend429WaitIsCancellable(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	sender, err := NewTelegramTextSender(&telegramSenderTestAPI{send: func(context.Context, int64, string) (Message, error) {
		calls.Add(1)
		close(entered)
		return Message{}, &APIError{Method: "sendMessage", Category: "rate_limited", RetryAfter: time.Hour}
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- sender.Send(ctx, nil, nil, "1", "test", false) }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
			t.Fatalf("calls=%d err=%v", calls.Load(), err)
		}
	case <-time.After(time.Second):
		t.Fatal("rate wait did not cancel")
	}
}
func TestTelegramLimiterChatAccountAndGroupBudgets(t *testing.T) {
	limiter := &telegramSendLimiter{}
	if err := limiter.wait(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if err := limiter.wait(t.Context(), -1); err != nil {
		t.Fatal(err)
	}
	limiter.mu.Lock()
	privateRemaining := time.Until(limiter.nextChat[1])
	groupRemaining := time.Until(limiter.nextChat[-1])
	limiter.mu.Unlock()
	if privateRemaining < 900*time.Millisecond || groupRemaining < 2900*time.Millisecond {
		t.Fatalf("spacing private=%v group=%v", privateRemaining, groupRemaining)
	}
	for chat := int64(2); chat <= 19; chat++ {
		if err := limiter.wait(t.Context(), chat); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := limiter.wait(ctx, 20); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("21st account send not limited: %v", err)
	}
	ctx2, cancel2 := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel2()
	if err := limiter.wait(ctx2, -1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("group send not limited: %v", err)
	}
}
func TestTelegramLimiterGlobalRetryAfter(t *testing.T) {
	limiter := &telegramSendLimiter{}
	limiter.pause(time.Hour)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := limiter.wait(ctx, 999); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("account pause not respected: %v", err)
	}
}

func TestTelegramSendErrorRetainsSanitizedCategory(t *testing.T) {
	for _, category := range []string{"unknown_send", "chat_migrated", "forbidden", "private description"} {
		calls := 0
		sender, err := im.NewIMTextSender(im.IMTextSendOptions{Platform: "telegram", Budget: 1, TruncationNotice: "!", SendSegment: func(context.Context, string, string) error {
			calls++
			if calls == 1 {
				return nil
			}
			return &APIError{Method: "sendMessage", Category: category}
		}})
		if err != nil {
			t.Fatal(err)
		}
		err = sender.Send(t.Context(), nil, nil, "1", "ab", false)
		want := category
		if category == "private description" {
			want = "delivery_failed"
		}
		var sendErr *im.IMTextSendError
		if !errors.As(err, &sendErr) || sendErr.AcceptedSegments != 1 || sendErr.Category != want {
			t.Fatalf("%s: %v", category, err)
		}
		if snapshot := sender.TextSendTargetSnapshot("1"); snapshot.LastFailureCategory != want || snapshot.LastAcceptedSegments != 1 {
			t.Fatalf("%s: %+v", category, snapshot)
		}
	}
}

func TestTelegramCancelledInFlightSendRemainsUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sender, err := NewTelegramTextSender(&telegramSenderTestAPI{send: func(context.Context, int64, string) (Message, error) {
		cancel()
		return Message{}, &APIError{Method: "sendMessage", Category: "unknown_send", Cause: im.ErrDeliveryOutcomeUnknown}
	}})
	if err != nil {
		t.Fatal(err)
	}
	err = sender.Send(ctx, nil, nil, "1", "test", true)
	if !errors.Is(err, im.ErrDeliveryOutcomeUnknown) || !strings.Contains(err.Error(), "unknown_send") {
		t.Fatalf("in-flight unknown delivery was replaced by cancellation: %v", err)
	}
}

func TestTelegramSendTextContextControlBudget(t *testing.T) {
	for _, tc := range []struct {
		name               string
		permit             bool
		parentBudget, want time.Duration
	}{
		{name: "control budget", permit: true, parentBudget: time.Hour, want: 15 * time.Second},
		{name: "proactive budget", parentBudget: time.Hour, want: 5 * time.Minute},
		{name: "short caller control", permit: true, parentBudget: 5 * time.Second, want: 15 * time.Second},
		{name: "short caller proactive", parentBudget: 5 * time.Second, want: 5 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var deadlines []time.Time
				// SendMessage adds its own per-request timeout, so inspect the
				// complete-output budget before entering the HTTP API.
				sender, err := NewTelegramTextSender(&telegramSenderTestAPI{send: func(ctx context.Context, _ int64, _ string) (Message, error) {
					deadline, _ := ctx.Deadline()
					deadlines = append(deadlines, deadline)
					return Message{}, nil
				}})
				if err != nil {
					t.Fatal(err)
				}
				a := NewTelegramAdapterForTest("account-1", "samplebot", nil, nil, nil)
				defer a.Disconnect()
				a.ready, a.sender = true, sender
				ctx, cancel := context.WithTimeout(t.Context(), tc.parentBudget)
				defer cancel()
				if tc.permit {
					ctx = context.WithValue(ctx, im.OutboundReplyContextKey{}, im.ReplyPermit{SenderID: "user-1", ExpiresAt: time.Now().Add(time.Hour)})
				}
				parentDeadline, _ := ctx.Deadline()
				start := time.Now()
				if err := a.SendTextContext(ctx, "123456789", strings.Repeat("a", 3*TextBudget)); err != nil {
					t.Fatalf("send failed: %v", err)
				}
				if len(deadlines) != 3 {
					t.Fatalf("request deadlines = %v, want three segments", deadlines)
				}
				want := start.Add(tc.want)
				if parentDeadline.Before(want) {
					want = parentDeadline
				}
				for i, deadline := range deadlines {
					if !deadline.Equal(want) {
						t.Fatalf("segment %d deadline = %s, want %s", i+1, deadline, want)
					}
				}
			})
		})
	}
}

func TestTelegramLimiterPauseNeverMovesDeadlineBackward(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := &telegramSendLimiter{}
		limiter.pause(80 * time.Millisecond)
		blockedUntil := limiter.blockedUntil
		limiter.pause(20 * time.Millisecond)
		if !limiter.blockedUntil.Equal(blockedUntil) {
			t.Fatalf("overlapping shorter pause moved the deadline from %v to %v", blockedUntil, limiter.blockedUntil)
		}
		time.Sleep(70 * time.Millisecond)
		limiter.pause(20 * time.Millisecond)
		if want := blockedUntil.Add(10 * time.Millisecond); !limiter.blockedUntil.Equal(want) {
			t.Fatalf("later pause deadline = %v, want %v", limiter.blockedUntil, want)
		}
	})
}
