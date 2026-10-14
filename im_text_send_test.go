package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func testIMTextSender(t *testing.T, budget int, send func(context.Context, string, string) error) *IMTextSender {
	t.Helper()
	s, err := NewIMTextSender(IMTextSendOptions{Budget: budget, SendSegment: send, TruncationNotice: "[cut]"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestIMTextSendRenderingUnicodeAndNewlines(t *testing.T) {
	var got []string
	s, err := NewIMTextSender(IMTextSendOptions{Budget: 12, Render: func(s string) string { return "<" + strings.ReplaceAll(s, "&", "&amp;") + ">" }, Measure: utf8.RuneCountInString, TruncationNotice: "[cut]", SendSegment: func(_ context.Context, _, text string) error { got = append(got, text); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Send(context.Background(), nil, nil, "chat", "A😀\n&&界界界界界界", false); err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 || got[0] != "<A😀\n>" {
		t.Fatalf("newline not preferred: %q", got)
	}
	var recovered strings.Builder
	for _, part := range got {
		if !utf8.ValidString(part) || utf8.RuneCountInString(part) > 12 {
			t.Fatalf("invalid rendered chunk: %q", part)
		}
		recovered.WriteString(strings.ReplaceAll(part[1:len(part)-1], "&amp;", "&"))
	}
	if recovered.String() != "A😀\n&&界界界界界界" {
		t.Fatalf("body changed: %q", recovered.String())
	}
}
func TestIMTextSendTruncationReservesNoticeAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		budget     int
	}{{"segments", strings.Repeat("😀", 1000), 10}, {"bytes", strings.Repeat("界", 30000), 65536}} {
		t.Run(tc.name, func(t *testing.T) {
			var parts []string
			s := testIMTextSender(t, tc.budget, func(_ context.Context, _, text string) error { parts = append(parts, text); return nil })
			if err := s.Send(context.Background(), nil, nil, "chat", tc.text, false); err != nil {
				t.Fatal(err)
			}
			if len(parts) > 32 || len(parts) == 0 {
				t.Fatalf("segment count=%d", len(parts))
			}
			joined := strings.Join(parts, "")
			if len(joined) > 65536 || !utf8.ValidString(joined) || !strings.HasSuffix(joined, "[cut]") {
				t.Fatalf("invalid truncation bytes=%d parts=%d", len(joined), len(parts))
			}
			for _, part := range parts {
				if utf8.RuneCountInString(part) > tc.budget {
					t.Fatalf("budget exceeded: %d", utf8.RuneCountInString(part))
				}
			}
		})
	}
}
func TestIMTextSendPartialFailureNeverReplays(t *testing.T) {
	failure := errors.New("unknown HTTP outcome")
	var calls []string
	s := testIMTextSender(t, 5, func(_ context.Context, _, text string) error {
		calls = append(calls, text)
		if len(calls) == 2 {
			return failure
		}
		return nil
	})
	err := s.Send(context.Background(), nil, nil, "chat", "aaaaabbbbbccccc", false)
	var partial *IMTextSendError
	if !errors.As(err, &partial) || partial.AcceptedSegments != 1 || partial.Category != "delivery_failed" || !errors.Is(err, failure) {
		t.Fatalf("bad partial failure: %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"aaaaa", "bbbbb"}) {
		t.Fatalf("replayed or continued: %v", calls)
	}
}
func TestIMTextSendExplicitRateLimitOnlyRetriesRejectedSegment(t *testing.T) {
	var calls []string
	s := testIMTextSender(t, 5, func(_ context.Context, _, text string) error {
		calls = append(calls, text)
		if len(calls) == 2 {
			return &IMRateLimitError{RetryAfter: time.Millisecond}
		}
		return nil
	})
	if err := s.Send(context.Background(), nil, nil, "chat", "aaaaabbbbbccccc", false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"aaaaa", "bbbbb", "bbbbb", "ccccc"}) {
		t.Fatal(calls)
	}
}
func TestIMTextSendContextsAndRateWaitCancellation(t *testing.T) {
	for _, cancelSource := range []string{"caller", "lifecycle", "permit"} {
		t.Run(cancelSource, func(t *testing.T) {
			caller, cancelCaller := context.WithCancel(context.Background())
			defer cancelCaller()
			life, cancelLife := context.WithCancel(context.Background())
			defer cancelLife()
			permit, cancelPermit := context.WithCancel(context.Background())
			defer cancelPermit()
			called := make(chan struct{})
			calls := 0
			s := testIMTextSender(t, 10, func(context.Context, string, string) error {
				calls++
				close(called)
				return &IMRateLimitError{RetryAfter: time.Hour}
			})
			done := make(chan error, 1)
			go func() { done <- s.Send(caller, life, permit, "chat", "hello", false) }()
			<-called
			switch cancelSource {
			case "caller":
				cancelCaller()
			case "lifecycle":
				cancelLife()
			case "permit":
				cancelPermit()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("rate-limit wait ignored cancellation")
			}
			if calls != 1 {
				t.Fatalf("cancelled output retried: %d", calls)
			}
		})
	}
}
func TestIMTextSendDeadlineIsOneTotalBudget(t *testing.T) {
	for _, tc := range []struct {
		name               string
		control            bool
		parentBudget, want time.Duration
	}{{"long", false, time.Hour, 5 * time.Minute}, {"control", true, time.Hour, 15 * time.Second}, {"caller", false, time.Second, time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			parent, cancel := context.WithTimeout(context.Background(), tc.parentBudget)
			defer cancel()
			var first time.Time
			s := testIMTextSender(t, 5, func(ctx context.Context, _, _ string) error {
				deadline, ok := ctx.Deadline()
				if !ok {
					t.Fatal("missing deadline")
				}
				if first.IsZero() {
					first = deadline
				} else if !deadline.Equal(first) {
					t.Fatal("deadline reset per segment")
				}
				return nil
			})
			if err := s.Send(parent, nil, nil, "chat", "aaaaabbbbb", tc.control); err != nil {
				t.Fatal(err)
			}
			if delta := first.Sub(start) - tc.want; delta < -100*time.Millisecond || delta > 100*time.Millisecond {
				t.Fatalf("unexpected deadline delta %s", delta)
			}
		})
	}
}
func TestIMTextSendSameTargetDoesNotInterleave(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s := testIMTextSender(t, 5, func(_ context.Context, _, text string) error {
		once.Do(func() { close(entered); <-release })
		mu.Lock()
		calls = append(calls, text)
		mu.Unlock()
		return nil
	})
	done := make(chan error, 2)
	go func() { done <- s.Send(context.Background(), nil, nil, "chat", "aaaaaaaaaa", false) }()
	<-entered
	go func() { done <- s.Send(context.Background(), nil, nil, "chat", "bbbbbbbbbb", false) }()
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(calls, []string{"aaaaa", "aaaaa", "bbbbb", "bbbbb"}) {
		t.Fatalf("interleaved: %v", calls)
	}
	if len(s.targets) != 0 {
		t.Fatal("target queue leaked after completion")
	}
}
func TestIMTextSendGlobalConcurrencyAcrossSenders(t *testing.T) {
	var active, peak atomic.Int32
	entered := make(chan struct{}, 16)
	release := make(chan struct{})
	send := func(context.Context, string, string) error {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		entered <- struct{}{}
		<-release
		return nil
	}
	a := testIMTextSender(t, 10, send)
	b := testIMTextSender(t, 10, send)
	done := make(chan error, 16)
	for i := range 16 {
		go func() {
			sender := a
			if i%2 != 0 {
				sender = b
			}
			done <- sender.Send(context.Background(), nil, nil, fmt.Sprint(i), "body", false)
		}()
	}
	for range 8 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("global capacity not reached")
		}
	}
	close(release)
	for range 16 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if peak.Load() != 8 {
		t.Fatalf("global concurrency=%d", peak.Load())
	}
}
func TestIMTextSendPreCancelledDoesNotSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := testIMTextSender(t, 5, func(context.Context, string, string) error { t.Fatal("cancelled transport called"); return nil })
	if err := s.Send(context.Background(), ctx, nil, "chat", "hello", false); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestIMTextSendQueuedCancellationAndTargetCleanup(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	s := testIMTextSender(t, 10, func(context.Context, string, string) error { calls.Add(1); close(entered); <-release; return nil })
	first := make(chan error, 1)
	go func() { first <- s.Send(context.Background(), nil, nil, "chat", "first", false) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Send(ctx, nil, nil, "chat", "second", false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || len(s.targets) != 0 {
		t.Fatal("cancelled target send ran or leaked")
	}
}
func TestIMTextSendInvalidInputDoesNotReachTransport(t *testing.T) {
	send := func(context.Context, string, string) error { t.Fatal("invalid input sent"); return nil }
	if _, err := NewIMTextSender(IMTextSendOptions{Budget: 0, SendSegment: send}); err == nil {
		t.Fatal("zero budget accepted")
	}
	if _, err := NewIMTextSender(IMTextSendOptions{Budget: 5}); err == nil {
		t.Fatal("missing transport accepted")
	}
	if _, err := NewIMTextSender(IMTextSendOptions{Budget: 5, SendSegment: send, TruncationNotice: "too long"}); err == nil {
		t.Fatal("oversized notice accepted")
	}
	s := testIMTextSender(t, 10, send)
	for _, tc := range []struct{ target, text string }{{"", "hello"}, {"chat", string([]byte{0xff})}} {
		if err := s.Send(context.Background(), nil, nil, tc.target, tc.text, false); err == nil {
			t.Fatalf("invalid input accepted: %+v", tc)
		}
	}
	if err := s.Send(context.Background(), nil, nil, "chat", "", false); err != nil {
		t.Fatal(err)
	}
}

func TestIMTextSendCancellationPreservesUnknownDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	s := testIMTextSender(t, 10, func(context.Context, string, string) error {
		calls++
		cancel()
		return errDeliveryOutcomeUnknown
	})
	err := s.Send(ctx, nil, nil, "chat", "hello", false)
	var sendErr *IMTextSendError
	if !errors.Is(err, errDeliveryOutcomeUnknown) || !errors.Is(err, context.Canceled) || !errors.As(err, &sendErr) || sendErr.Category != "unknown_send" || sendErr.AcceptedSegments != 0 || calls != 1 {
		t.Fatalf("unknown outcome was replaced or replayed: calls=%d err=%+v", calls, err)
	}
}
