package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/keakon/golog/log"
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
	for _, category := range []string{"credentials", "invalid_identity", "invalid_request", "forbidden", "method_rejected"} {
		if _, retry := b.failed(&telegramAPIError{Category: category}); retry {
			t.Fatal("terminal error retried", category)
		}
	}
	// A 409 right after a local restart is the stale server-side long poll and
	// retries briefly; a persistent conflict (a true second poller) stays fatal.
	var conflict telegramBackoff
	for range 30 {
		if _, retry := conflict.failed(&telegramAPIError{Category: "poller_conflict"}); !retry {
			t.Fatal("restart-overlap 409 must retry")
		}
	}
	if _, retry := conflict.failed(&telegramAPIError{Category: "poller_conflict"}); retry {
		t.Fatal("persistent 409 must stay fatal")
	}
	if delay, retry := b.failed(&telegramAPIError{Category: "rate_limited", RetryAfter: time.Hour}); !retry || delay != time.Hour {
		t.Fatal("retry_after was jittered or capped", delay, retry)
	}
}

// A permanently failing poller that never logs is indistinguishable from an idle
// one, so every backed-off attempt must be reported once per backoff step.
func TestTelegramRetryWarningReportsEachBackoffStep(t *testing.T) {
	var a TelegramAdapter
	var warnings []string
	restore := log.Warnf
	log.Warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	defer func() { log.Warnf = restore }()
	transport := &telegramAPIError{Method: "getUpdates", Status: 500, Category: "invalid_response"}
	a.warnRetry(transport, time.Second)
	// Within the bounded interval the same failure stays quiet, ...
	a.warnRetry(transport, 2*time.Second)
	if len(warnings) != 1 {
		t.Fatalf("bounded interval repeated the warning: %v", warnings)
	}
	// ... but the next step after the interval is reported again.
	a.retryWarn = a.retryWarn.Add(-2 * telegramRetryReportInterval)
	a.warnRetry(transport, 3*time.Second)
	if len(warnings) != 2 {
		t.Fatalf("bounded interval swallowed the next warning: %v", warnings)
	}
}

// The warning must name the method and the error category, and must never leak
// the token or the request URL.
func TestTelegramRetryWarningNamesMethodAndCategory(t *testing.T) {
	var a TelegramAdapter
	var warnings []string
	restore := log.Warnf
	log.Warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	defer func() { log.Warnf = restore }()
	report := func(err error, delay time.Duration) {
		a.retryWarn = time.Time{}
		a.warnRetry(err, delay)
	}
	report(&telegramAPIError{Method: "getUpdates", Status: 500, Category: "invalid_response"}, time.Second)
	report(&telegramAPIError{Method: "getMe", Status: 409, Category: "poller_conflict"}, 2*time.Second)
	report(errors.New("dial failure"), time.Second)
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"getUpdates", "invalid_response", "category=poller_conflict", "getMe", "category=transport", "retrying in"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("warning lacks %q: %q", want, joined)
		}
	}
	for _, warning := range warnings {
		if strings.Contains(warning, "/bot") {
			t.Fatalf("warning leaked the request path: %q", warning)
		}
	}
}
