package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

type telegramSenderTestAPI struct {
	send func(context.Context, int64, string) (telegramMessage, error)
}

func (*telegramSenderTestAPI) GetMe(context.Context) (telegramUser, error) {
	return telegramUser{}, nil
}
func (*telegramSenderTestAPI) GetWebhookInfo(context.Context) (telegramWebhookInfo, error) {
	return telegramWebhookInfo{}, nil
}
func (*telegramSenderTestAPI) GetUpdates(context.Context, *int64, int) ([]telegramUpdate, error) {
	return nil, nil
}
func (a *telegramSenderTestAPI) SendMessage(ctx context.Context, chat int64, text string) (telegramMessage, error) {
	return a.send(ctx, chat, text)
}

func TestTelegramUTF16Segmentation(t *testing.T) {
	sender, err := newTelegramTextSender(&telegramSenderTestAPI{})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{strings.Repeat("a", 3500), strings.Repeat("😀", 1750), strings.Repeat("😀", 1751), strings.Repeat("a", 3400) + "\n" + strings.Repeat("😀", 100)} {
		parts, err := sender.segments(text)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(parts, "") != text {
			t.Fatal("segmentation lost text")
		}
		for _, part := range parts {
			if !utf8.ValidString(part) || imUTF16Units(part) > telegramTextBudget {
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
	sender, err := newTelegramTextSender(&telegramSenderTestAPI{send: func(context.Context, int64, string) (telegramMessage, error) {
		calls.Add(1)
		return telegramMessage{}, &telegramAPIError{Method: "sendMessage", Category: "unknown_send", Cause: errDeliveryOutcomeUnknown}
	}})
	if err != nil {
		t.Fatal(err)
	}
	err = sender.Send(t.Context(), nil, nil, "1", "test", false)
	if !errors.Is(err, errDeliveryOutcomeUnknown) || calls.Load() != 1 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
}
func TestTelegramSend429WaitIsCancellable(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	sender, err := newTelegramTextSender(&telegramSenderTestAPI{send: func(context.Context, int64, string) (telegramMessage, error) {
		calls.Add(1)
		close(entered)
		return telegramMessage{}, &telegramAPIError{Method: "sendMessage", Category: "rate_limited", RetryAfter: time.Hour}
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
		sender, err := NewIMTextSender(IMTextSendOptions{Platform: "telegram", Budget: 1, TruncationNotice: "!", SendSegment: func(context.Context, string, string) error {
			calls++
			if calls == 1 {
				return nil
			}
			return &telegramAPIError{Method: "sendMessage", Category: category}
		}})
		if err != nil {
			t.Fatal(err)
		}
		err = sender.Send(t.Context(), nil, nil, "1", "ab", false)
		want := category
		if category == "private description" {
			want = "delivery_failed"
		}
		var sendErr *IMTextSendError
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
	sender, err := newTelegramTextSender(&telegramSenderTestAPI{send: func(context.Context, int64, string) (telegramMessage, error) {
		cancel()
		return telegramMessage{}, &telegramAPIError{Method: "sendMessage", Category: "unknown_send", Cause: errDeliveryOutcomeUnknown}
	}})
	if err != nil {
		t.Fatal(err)
	}
	err = sender.Send(ctx, nil, nil, "1", "test", true)
	if !errors.Is(err, errDeliveryOutcomeUnknown) || !strings.Contains(err.Error(), "unknown_send") {
		t.Fatalf("in-flight unknown delivery was replaced by cancellation: %v", err)
	}
}
