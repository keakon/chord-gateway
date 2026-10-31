package im

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
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
		return ErrDeliveryOutcomeUnknown
	})
	err := s.Send(ctx, nil, nil, "chat", "hello", false)
	var sendErr *IMTextSendError
	if !errors.Is(err, ErrDeliveryOutcomeUnknown) || !errors.Is(err, context.Canceled) || !errors.As(err, &sendErr) || sendErr.Category != "unknown_send" || sendErr.AcceptedSegments != 0 || calls != 1 {
		t.Fatalf("unknown outcome was replaced or replayed: calls=%d err=%+v", calls, err)
	}
}

// An outbound message that fits one segment is budgeted by rendering it once;
// probing prefix lengths instead would multiply the adapter's render cost by
// the number of binary search steps.
func TestIMTextSegmentsSingleSegmentRendersOnce(t *testing.T) {
	renders := 0
	s, err := NewIMTextSender(IMTextSendOptions{
		Platform: "discord", Budget: 1900,
		Render:           func(source string) string { renders++; return source },
		Measure:          UTF16Units,
		TruncationNotice: "[cut]",
		SendSegment:      func(context.Context, string, string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"sample reply", strings.Repeat("界", 1300), strings.Repeat("😀", 900)} {
		renders = 0
		segments, err := s.Segments(text)
		if err != nil {
			t.Fatal(err)
		}
		if len(segments) != 1 || segments[0] != text || renders != 1 {
			t.Fatalf("single segment budgeting rendered %d times: %q", renders, segments)
		}
	}
}

// Budget probes slice at rune boundaries, so every adapter shape must still
// yield valid UTF-8 segments within budget that partition the message without
// losing or duplicating content, including when the notice is appended.
func TestIMTextSegmentsKeepBudgetAndContentInvariants(t *testing.T) {
	escapeAngle := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "<", "&lt;"), ">", "&gt;") }
	escapeMarkdown := func(s string) string {
		var out strings.Builder
		for _, r := range s {
			if strings.ContainsRune("\\`*_{}[]()#+-.!|>~", r) {
				out.WriteByte('\\')
			}
			out.WriteRune(r)
		}
		return out.String()
	}
	adapters := []struct {
		name    string
		budget  int
		render  func(string) string
		measure func(string) int
	}{
		{"plain", 12, func(s string) string { return s }, UTF16Units},
		{"escaped", 24, escapeAngle, func(s string) int { return len(s) }},
		{"markdown", 1900, escapeMarkdown, UTF16Units},
	}
	atoms := []string{"a", "界", "😀", "\n", "*", "<", ">", " ", "ab", "中文"}
	const notice = "[cut]"
	r := rand.New(rand.NewSource(11))
	for _, adapter := range adapters {
		s, err := NewIMTextSender(IMTextSendOptions{
			Platform: adapter.name, Budget: adapter.budget, Render: adapter.render, Measure: adapter.measure,
			TruncationNotice: notice, SendSegment: func(context.Context, string, string) error { return nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		for trial := range 150 {
			runes := 1 + r.Intn(400)
			if trial%25 == 24 {
				runes = MaxTextBytes + 1 // forces the byte cap and the truncation notice
			}
			var sb strings.Builder
			for range runes {
				sb.WriteString(atoms[r.Intn(len(atoms))])
			}
			text := sb.String()
			segments, err := s.Segments(text)
			if err != nil {
				t.Fatalf("%s: %v", adapter.name, err)
			}
			if len(segments) == 0 || len(segments) > MaxTextSegments {
				t.Fatalf("%s: segment count %d", adapter.name, len(segments))
			}
			var assembled strings.Builder
			for i, segment := range segments {
				if !utf8.ValidString(segment) {
					t.Fatalf("%s: split rune in segment %d of %q", adapter.name, i, text)
				}
				if n := adapter.measure(segment); n < 0 || n > adapter.budget {
					t.Fatalf("%s: segment %d uses %d of %d budget: %q", adapter.name, i, n, adapter.budget, segment)
				}
				assembled.WriteString(segment)
			}
			renderedNotice := adapter.render(notice)
			truncated := false
			for i, segment := range segments {
				if !strings.Contains(segment, renderedNotice) {
					continue
				}
				truncated = true
				if i != len(segments)-1 {
					t.Fatalf("%s: notice landed in segment %d of %d: %q", adapter.name, i, len(segments), segment)
				}
			}
			if truncated && !strings.HasSuffix(assembled.String(), renderedNotice) {
				t.Fatalf("%s: truncation notice is not at the end of the message", adapter.name)
			}
			if len(text) > MaxTextBytes && !truncated {
				t.Fatalf("%s: byte-capped text omitted the truncation notice", adapter.name)
			}
			source := strings.TrimSuffix(assembled.String(), renderedNotice)
			if truncated {
				// The sender inserts a newline between the last content and the
				// notice, so the message itself ends one byte earlier.
				source = strings.TrimSuffix(source, "\n")
			}
			if truncated && strings.Contains(source, renderedNotice) {
				t.Fatalf("%s: notice repeated inside the message", adapter.name)
			}
			renderedText := adapter.render(text)
			if truncated {
				if !strings.HasPrefix(renderedText, source) {
					t.Fatalf("%s: truncated segments are not an in-order prefix: %d of %d bytes", adapter.name, len(source), len(renderedText))
				}
			} else if source != renderedText {
				t.Fatalf("%s: complete segments changed content: got %d bytes, want %d", adapter.name, len(source), len(renderedText))
			}
		}
	}
}

// A message truncated by the 64 KiB byte cap must still merge the tail into
// the final segment when the tail fits alongside the truncation notice, rather
// than spending one extra message on it.
func TestIMTextSegmentsByteCapTruncationMergesNoticeTail(t *testing.T) {
	escapeMarkdown := func(s string) string {
		var out strings.Builder
		for _, r := range s {
			if strings.ContainsRune("\\`*_{}[]()#+-.!|>~", r) {
				out.WriteByte('\\')
			}
			out.WriteRune(r)
		}
		return out.String()
	}
	s, err := NewIMTextSender(IMTextSendOptions{
		Platform: "discord", Budget: 1900,
		Render:      escapeMarkdown,
		Measure:     UTF16Units,
		SendSegment: func(context.Context, string, string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	// A Discord-shaped body just past the byte cap: CJK text, one newline, and
	// a run of escapable characters across the reserved-tail boundary, where
	// escape expansion moves the notice-aware break off a plain segment split.
	var sb strings.Builder
	for sb.Len() < 63600 {
		sb.WriteString("中")
	}
	sb.WriteString("\n")
	for sb.Len() < 64925 {
		sb.WriteString("中")
	}
	for sb.Len() < 66000 {
		sb.WriteString("*")
	}
	segments, err := s.Segments(sb.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 12 {
		t.Fatalf("byte-cap truncation split into %d segments, want 12", len(segments))
	}
	renderedNotice := escapeMarkdown(s.options.TruncationNotice)
	if !strings.HasSuffix(segments[len(segments)-1], renderedNotice) {
		t.Fatalf("last segment does not end with the truncation notice: %q", segments[len(segments)-1])
	}
}
