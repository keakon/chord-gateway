package main

import (
	"errors"
	"time"

	"github.com/keakon/golog/log"
)

// One backoff spans startup and polling requests. A single successful probe
// must not reset a flapping connection's retry delay.
type telegramBackoff struct {
	next         time.Duration
	successSince time.Time
	// conflictRetries bounds restarts that overlap a stale server-side long
	// poll. A second poller elsewhere stays fatal; a local restart overlapping
	// the previous 25s long poll retries briefly instead of exiting.
	conflictRetries int
}

func (b *telegramBackoff) succeeded(now time.Time) {
	if b.successSince.IsZero() {
		b.successSince = now
	}
	if now.Sub(b.successSince) >= time.Minute {
		b.next = 0
	}
	b.conflictRetries = 0
}

func (b *telegramBackoff) failed(err error) (time.Duration, bool) {
	b.successSince = time.Time{}
	var apiErr *telegramAPIError
	if errors.As(err, &apiErr) {
		switch apiErr.Category {
		case "credentials", "invalid_identity", "invalid_request", "forbidden", "method_rejected":
			return 0, false
		case "poller_conflict":
			// A stale server-side long poll survives a local restart for up to
			// its 25s timeout. Retry briefly so a restart does not exit; a true
			// second poller keeps conflicting past the window and stays fatal.
			if b.conflictRetries >= 30 {
				return 0, false
			}
			b.conflictRetries++
			return jitterBackoff(2 * time.Second), true
		case "rate_limited":
			if apiErr.RetryAfter > 0 {
				return apiErr.RetryAfter, true
			}
		}
	}
	if b.next == 0 {
		b.next = time.Second
	}
	delay := jitterBackoff(b.next)
	b.next = min(b.next*2, 30*time.Second)
	return delay, true
}

// telegramRetryReportInterval bounds how often a failing request repeats its
// warning. Time-based, not count-based: a long-polling loop and a tight retry
// loop must warn at the same cadence.
const telegramRetryReportInterval = time.Minute

// warnRetry keeps a failing poller visible: warn on the first failure of a run,
// then at a bounded interval, naming the method, the error category and the
// delay. Without it a permanently failing request looks exactly like an idle
// poller. The token and the request URL never appear.
func (a *TelegramAdapter) warnRetry(err error, delay time.Duration) {
	now := time.Now()
	if !a.retryWarn.IsZero() && now.Sub(a.retryWarn) < telegramRetryReportInterval {
		return
	}
	a.retryWarn = now
	method, category := "request", "transport"
	var apiErr *telegramAPIError
	if errors.As(err, &apiErr) {
		method, category = apiErr.Method, apiErr.Category
	}
	log.Warnf("telegram: %v request failed category=%v; retrying in %v", method, category, delay)
}
