package main

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/rest"
)

const discordRateLimiterCapacity = 4096

var (
	errDiscordRateLimiterCapacity = errors.New("discord rate limiter capacity reached")
	errDiscordRateLimitHeaders    = errors.New("invalid discord rate limit headers")
)

type discordRateLimitKey struct{ method, route, major string }
type discordRateLimitBucket struct {
	busy       bool
	references int
	remaining  int
	reset      time.Time
}

// discordRateLimiter supplies only DisGo's rate-limit hook. The original REST
// client still owns requests and explicit 429 retries. No cleanup goroutine is
// needed: unused, expired buckets are reclaimed when the bounded map fills.
type discordRateLimiter struct {
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	closed  bool
	global  time.Time
	buckets map[discordRateLimitKey]*discordRateLimitBucket
	changed chan struct{}
}

var _ rest.RateLimiter = (*discordRateLimiter)(nil)

func newDiscordRateLimiter(ctx context.Context) *discordRateLimiter {
	ctx, cancel := context.WithCancel(ctx)
	return &discordRateLimiter{ctx: ctx, cancel: cancel, buckets: make(map[discordRateLimitKey]*discordRateLimitBucket), changed: make(chan struct{})}
}

func (*discordRateLimiter) MaxRetries() int { return 10 }

func discordRateLimitEndpointKey(endpoint *rest.CompiledEndpoint) (discordRateLimitKey, error) {
	if endpoint == nil || endpoint.Endpoint == nil || endpoint.Endpoint.Method == "" || endpoint.Endpoint.Route == "" {
		return discordRateLimitKey{}, errors.New("invalid discord rate limit endpoint")
	}
	return discordRateLimitKey{endpoint.Endpoint.Method, endpoint.Endpoint.Route, endpoint.MajorParams}, nil
}

// notifyLocked wakes both route-lock contenders and waits whose global reset
// may have changed. Callers always recompute the deadline under the same lock.
func (l *discordRateLimiter) notifyLocked() {
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *discordRateLimiter) stoppedLocked() error {
	if err := l.ctx.Err(); err != nil {
		return err
	}
	if l.closed {
		return context.Canceled
	}
	return nil
}

func (l *discordRateLimiter) Wait(ctx context.Context, endpoint *rest.CompiledEndpoint) error {
	key, err := discordRateLimitEndpointKey(endpoint)
	if err != nil {
		return err
	}
	l.mu.Lock()
	if err = l.stoppedLocked(); err == nil {
		err = ctx.Err()
	}
	if err != nil {
		l.mu.Unlock()
		return err
	}
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= discordRateLimiterCapacity {
			now := time.Now()
			for candidate, bucket := range l.buckets {
				if bucket.references == 0 && !bucket.busy && !bucket.reset.After(now) {
					delete(l.buckets, candidate)
				}
			}
		}
		if len(l.buckets) >= discordRateLimiterCapacity {
			l.mu.Unlock()
			return errDiscordRateLimiterCapacity
		}
		b = &discordRateLimitBucket{remaining: 1}
		l.buckets[key] = b
	}
	b.references++
	held := false
	for {
		if err = l.stoppedLocked(); err == nil {
			err = ctx.Err()
		}
		if err != nil {
			b.references--
			if held {
				b.busy = false
				l.notifyLocked()
			}
			l.mu.Unlock()
			return err
		}
		if !held && !b.busy {
			b.busy, held = true, true
		}
		var until time.Time
		if held {
			until = l.global
			if b.remaining == 0 && b.reset.After(until) {
				until = b.reset
			}
			if !until.After(time.Now()) {
				l.mu.Unlock()
				return nil // Unlock releases this reference and exclusive bucket.
			}
		}
		changed := l.changed
		l.mu.Unlock()
		var timer *time.Timer
		var elapsed <-chan time.Time
		if held {
			timer = time.NewTimer(time.Until(until))
			elapsed = timer.C
		}
		select {
		case <-ctx.Done():
		case <-l.ctx.Done():
		case <-changed:
		case <-elapsed:
		}
		if timer != nil {
			timer.Stop()
		}
		l.mu.Lock()
	}
}

// discordRateLimitSeconds accepts Discord's decimal seconds without truncating
// the fraction before scaling. Ceil avoids opening a bucket a nanosecond early.
func discordRateLimitSeconds(value string) (time.Duration, error) {
	seconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds >= float64(math.MaxInt64)/float64(time.Second) {
		return 0, errDiscordRateLimitHeaders
	}
	return time.Duration(math.Ceil(seconds * float64(time.Second))), nil
}

func (l *discordRateLimiter) Unlock(endpoint *rest.CompiledEndpoint, response *http.Response) error {
	key, err := discordRateLimitEndpointKey(endpoint)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil || !b.busy {
		return nil
	}
	defer func() {
		b.busy = false
		b.references--
		l.notifyLocked()
	}()
	if response == nil {
		return nil // Transport failure still releases every route waiter.
	}
	now := time.Now()
	header := response.Header
	if response.StatusCode == http.StatusTooManyRequests {
		var delay time.Duration
		for _, name := range []string{"Retry-After", "X-RateLimit-Reset-After"} {
			if value := header.Get(name); value != "" {
				candidate, parseErr := discordRateLimitSeconds(value)
				if parseErr != nil {
					return parseErr
				}
				if candidate > delay {
					delay = candidate
				}
			}
		}
		if delay <= 0 {
			return errDiscordRateLimitHeaders // Stop SDK retries without a safe delay.
		}
		reset := now.Add(delay)
		// Discord sends "false" when a limit is route scoped; only an explicit
		// true, or the global scope label, is a global reset.
		if strings.EqualFold(header.Get("X-RateLimit-Global"), "true") || strings.EqualFold(header.Get("X-RateLimit-Scope"), "global") {
			if reset.After(l.global) {
				l.global = reset
			}
		} else {
			b.remaining = 0
			if reset.After(b.reset) {
				b.reset = reset
			}
		}
		return nil
	}
	remaining := b.remaining
	if value := header.Get("X-RateLimit-Remaining"); value != "" {
		remaining, err = strconv.Atoi(value)
		if err != nil || remaining < 0 {
			return errDiscordRateLimitHeaders
		}
	}
	reset := b.reset
	if value := header.Get("X-RateLimit-Reset-After"); value != "" {
		delay, parseErr := discordRateLimitSeconds(value)
		if parseErr != nil {
			return parseErr
		}
		reset = now.Add(delay)
	} else if value := header.Get("X-RateLimit-Reset"); value != "" {
		seconds, parseErr := strconv.ParseFloat(value, 64)
		if parseErr != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds >= float64(math.MaxInt64) {
			return errDiscordRateLimitHeaders
		}
		whole, fraction := math.Modf(seconds)
		reset = time.Unix(int64(whole), int64(math.Ceil(fraction*float64(time.Second))))
	}
	b.remaining, b.reset = remaining, reset
	return nil
}

func (l *discordRateLimiter) Close(_ context.Context) {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		l.notifyLocked()
	}
	l.mu.Unlock()
	l.cancel()
}

func (l *discordRateLimiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.global = time.Time{}
	for key, b := range l.buckets {
		if b.references == 0 && !b.busy {
			delete(l.buckets, key)
		} else {
			b.remaining, b.reset = 1, time.Time{}
		}
	}
	l.notifyLocked()
}
