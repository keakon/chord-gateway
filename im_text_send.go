package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	IMTextSendTimeout    = 5 * time.Minute
	IMControlSendTimeout = 15 * time.Second
	imTextMaxBytes       = 64 << 10
	imTextMaxSegments    = 32
)

// Shared by all platforms, rather than allowing eight senders per account.
var imTextSendSlots = make(chan struct{}, 8)

// IMRateLimitError denotes an explicit platform rejection that accepted no
// message. Never wrap a timeout or an unknown HTTP outcome in this type.
type IMRateLimitError struct{ RetryAfter time.Duration }

func (e *IMRateLimitError) Error() string {
	return "platform explicitly rejected message due to rate limiting"
}

// IMTextSendError reports only segments explicitly accepted by the platform.
// The failed segment may have an unknown outcome; callers must not resend the
// whole output. Cause must be sanitized by the adapter before logging.
type IMTextSendError struct {
	AcceptedSegments int
	Category         string
	Cause            error
}

// imTextFailureClassifier allows a transport to retain a definite rejection or
// unknown outcome in partial-send diagnostics without exposing platform data.
type imTextFailureClassifier interface {
	IMTextSendFailureCategory() string
}

func (e *IMTextSendError) Error() string {
	return fmt.Sprintf("IM text send stopped after %d accepted segments (%s)", e.AcceptedSegments, e.Category)
}
func (e *IMTextSendError) Unwrap() error { return e.Cause }

// IMTextSendOptions keeps rendering and platform character accounting in the
// adapter. Measure counts the rendered value; it must be nonnegative and
// monotonic as a source prefix grows. Render must be deterministic. SendSegment
// must honor ctx and return nil only after explicit platform acceptance.
type IMTextSendOptions struct {
	Platform    string
	Budget      int
	Render      func(string) string
	Measure     func(string) int
	SendSegment func(context.Context, string, string) error
	// FailureCategory classifies transport evidence before counting a failure.
	// It must inspect local state only and must not send, retry, or wait.
	FailureCategory  func(context.Context, string, error) string
	TruncationNotice string
}
type imTextTarget struct {
	slot chan struct{}
	refs int
}
type IMTextSender struct {
	metrics imTextMetrics
	options IMTextSendOptions
	mu      sync.Mutex
	targets map[string]*imTextTarget
}

func NewIMTextSender(options IMTextSendOptions) (*IMTextSender, error) {
	if options.Budget <= 0 || options.SendSegment == nil {
		return nil, errors.New("text sender requires a positive segment budget and transport")
	}
	if options.Render == nil {
		options.Render = func(s string) string { return s }
	}
	if options.Measure == nil {
		options.Measure = utf8.RuneCountInString
	}
	if options.TruncationNotice == "" {
		options.TruncationNotice = "[Output truncated. View the full result in the original Chord session.]"
	}
	if !utf8.ValidString(options.TruncationNotice) || options.TruncationNotice == "" || len(options.TruncationNotice)+1 > imTextMaxBytes {
		return nil, errors.New("invalid truncation notice")
	}
	if n := options.Measure(options.Render(options.TruncationNotice)); n < 0 || n > options.Budget {
		return nil, errors.New("truncation notice exceeds segment budget")
	}
	return &IMTextSender{options: options, targets: make(map[string]*imTextTarget)}, nil
}

// Send holds a target's ordering slot across the complete output, including
// rate-limit waits. All waits share one absolute deadline. lifecycle should
// represent adapter shutdown, not temporary HTTP-platform ingress disconnects;
// permit represents the current authorized outbound binding.
func (s *IMTextSender) Send(ctx, lifecycle, permit context.Context, target, text string, control bool) error {
	if ctx == nil {
		return errors.New("text send requires a context")
	}
	timeout := IMTextSendTimeout
	if control {
		timeout = IMControlSendTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, other := range []context.Context{lifecycle, permit} {
		if other != nil {
			if other.Err() != nil {
				cancel()
			}
			stop := context.AfterFunc(other, cancel)
			defer stop()
		}
	}
	fail := func(accepted int, category string, err error) error {
		if s.options.FailureCategory != nil {
			category = imMetricReason(s.options.FailureCategory(ctx, category, err))
		}
		s.observeSendFailure(target, accepted, category)
		return &IMTextSendError{accepted, category, err}
	}
	if target == "" {
		return fail(0, "invalid_target", errors.New("empty target"))
	}
	if err := ctx.Err(); err != nil {
		return fail(0, "cancelled", err)
	}
	release, err := s.acquireTarget(ctx, target)
	if err != nil {
		return fail(0, "cancelled", err)
	}
	defer release()
	segments, err := s.segments(text)
	if err != nil {
		return fail(0, "invalid_text", err)
	}
	for i, segment := range segments {
		for {
			if err = ctx.Err(); err != nil {
				return fail(i, "cancelled", err)
			}
			select {
			case imTextSendSlots <- struct{}{}:
			case <-ctx.Done():
				return fail(i, "cancelled", ctx.Err())
			}
			if err = ctx.Err(); err == nil {
				err = s.options.SendSegment(ctx, target, segment)
			}
			<-imTextSendSlots
			if err == nil {
				break
			}
			// A cancellation after the transport wrote a segment still leaves
			// its delivery unknown. Retain that outcome rather than implying
			// the segment was merely cancelled before sending.
			if errors.Is(err, errDeliveryOutcomeUnknown) {
				return fail(i, "unknown_send", errors.Join(ctx.Err(), err))
			}
			var classified imTextFailureClassifier
			if errors.As(err, &classified) {
				switch category := classified.IMTextSendFailureCategory(); category {
				case "unknown_send", "platform_rejected", "backpressure", "connection_lost", "connection_unavailable", "auth_failed", "duplicate_connection", "identity_mismatch", "invalid_message", "local_error", "credentials", "poller_conflict", "chat_migrated", "forbidden", "api_rejected", "invalid_target", "invalid_request":
					return fail(i, category, err)
				}
			}
			if ctx.Err() != nil {
				// Cancellation ends this call but cannot erase a transport's
				// evidence that an already-sent segment has an unknown outcome.
				return fail(i, "cancelled", errors.Join(ctx.Err(), err))
			}
			var limited *IMRateLimitError
			if !errors.As(err, &limited) {
				return fail(i, "delivery_failed", err)
			}
			if limited.RetryAfter <= 0 {
				return fail(i, "invalid_rate_limit", err)
			}
			waitStart := time.Now()
			timer := time.NewTimer(limited.RetryAfter)
			select {
			case <-timer.C:
				s.observeRateWait(time.Since(waitStart))
			case <-ctx.Done():
				timer.Stop()
				s.observeRateWait(time.Since(waitStart))
				return fail(i, "cancelled", ctx.Err())
			}
		}
	}
	return nil
}
func (s *IMTextSender) acquireTarget(ctx context.Context, target string) (func(), error) {
	s.mu.Lock()
	entry := s.targets[target]
	if entry == nil {
		entry = &imTextTarget{slot: make(chan struct{}, 1)}
		s.targets[target] = entry
	}
	entry.refs++
	s.mu.Unlock()
	unref := func() {
		s.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(s.targets, target)
		}
		s.mu.Unlock()
	}
	select {
	case entry.slot <- struct{}{}:
		return func() { <-entry.slot; unref() }, nil
	case <-ctx.Done():
		unref()
		return nil, ctx.Err()
	}
}

func (s *IMTextSender) fits(source string) bool {
	n := s.options.Measure(s.options.Render(source))
	return n >= 0 && n <= s.options.Budget
}

// prefix returns a Unicode-safe prefix. Binary search bounds rendering work to
// logarithmic probes per segment; adapters supply monotonic budget accounting.
func (s *IMTextSender) prefix(source, suffix string) string {
	runes := []rune(source)
	low, high := 0, len(runes)
	for low < high {
		mid := (low + high + 1) / 2
		if s.fits(string(runes[:mid]) + suffix) {
			low = mid
		} else {
			high = mid - 1
		}
	}
	prefix := string(runes[:low])
	if end := strings.LastIndexByte(prefix, '\n'); low < len(runes) && end >= 0 {
		prefix = prefix[:end+1]
	}
	return prefix
}
func trimIMTextBytes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[:limit]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}
func (s *IMTextSender) segments(text string) ([]string, error) {
	if !utf8.ValidString(text) {
		return nil, errors.New("text is not valid UTF-8")
	}
	if text == "" {
		return nil, nil
	}
	raw := trimIMTextBytes(text, imTextMaxBytes)
	var source []string
	remaining := raw
	for len(source) < imTextMaxSegments && remaining != "" {
		part := s.prefix(remaining, "")
		if part == "" {
			return nil, errors.New("one character exceeds segment budget")
		}
		source = append(source, part)
		remaining = remaining[len(part):]
	}
	truncated := remaining != "" || len(raw) != len(text)
	if truncated {
		notice := s.options.TruncationNotice
		// Reserve both the bytes and the final segment before sending anything.
		remaining = trimIMTextBytes(text, imTextMaxBytes-len(notice)-1)
		source = nil
		for len(source) < imTextMaxSegments-1 && remaining != "" {
			if s.fits(remaining + "\n" + notice) {
				break
			}
			part := s.prefix(remaining, "")
			if part == "" {
				return nil, errors.New("one character exceeds segment budget")
			}
			source = append(source, part)
			remaining = remaining[len(part):]
		}
		part := s.prefix(remaining, "\n"+notice)
		if part == "" {
			source = append(source, notice)
		} else {
			source = append(source, part+"\n"+notice)
		}
	}
	rendered := make([]string, len(source))
	for i, part := range source {
		rendered[i] = s.options.Render(part)
	}
	return rendered, nil
}
