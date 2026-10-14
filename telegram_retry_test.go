package main

import (
	"errors"
	"testing"
	"time"
)

func TestTelegramBackoffSurvivesBriefSuccessAndResetsAfterStableMinute(t *testing.T) {
	var b telegramBackoff
	now := time.Now()
	check := func(base time.Duration) {
		t.Helper()
		delay, retry := b.failed(errors.New("network failure"))
		if !retry || delay < base*8/10 || delay > base*12/10 {
			t.Fatalf("base=%v delay=%v retry=%v", base, delay, retry)
		}
	}
	for _, base := range []time.Duration{1, 2, 4, 8, 16, 30, 30} {
		check(base * time.Second)
		b.succeeded(now)
		b.succeeded(now.Add(59 * time.Second))
		now = now.Add(time.Minute)
	}
	b.succeeded(now.Add(time.Minute))
	check(time.Second)
	for _, category := range []string{"credentials", "poller_conflict", "invalid_identity", "invalid_request", "forbidden"} {
		if _, retry := b.failed(&telegramAPIError{Category: category}); retry {
			t.Fatal("terminal error retried", category)
		}
	}
	if delay, retry := b.failed(&telegramAPIError{Category: "rate_limited", RetryAfter: time.Hour}); !retry || delay != time.Hour {
		t.Fatal("retry_after was jittered or capped", delay, retry)
	}
}
