package main

import (
	"errors"
	"time"
)

// One backoff spans startup and polling requests. A single successful probe
// must not reset a flapping connection's retry delay.
type telegramBackoff struct {
	next         time.Duration
	successSince time.Time
}

func (b *telegramBackoff) succeeded(now time.Time) {
	if b.successSince.IsZero() {
		b.successSince = now
	}
	if now.Sub(b.successSince) >= time.Minute {
		b.next = 0
	}
}

func (b *telegramBackoff) failed(err error) (time.Duration, bool) {
	b.successSince = time.Time{}
	var apiErr *telegramAPIError
	if errors.As(err, &apiErr) {
		switch apiErr.Category {
		case "credentials", "poller_conflict", "invalid_identity", "invalid_request", "forbidden":
			return 0, false
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
