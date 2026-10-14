// Derived from DisGo v0.19.6; see UPSTREAM.md.
package gateway

import (
	"context"
	"sync"
	"time"
)

const CommandsPerMinute = 120
const ReservedCommandSlots = 3

type RateLimiterCommandType int

const (
	NormalCommandType RateLimiterCommandType = iota
	InternalCommandType
)

type RateLimiter interface {
	Close(context.Context)
	Reset()
	Wait(context.Context, RateLimiterCommandType) error
	Unlock()
}

type rateLimiterImpl struct {
	mu           sync.Mutex
	gate         chan struct{}
	reset        time.Time
	remaining    int
	closed       bool
	closedSignal chan struct{}
	config       rateLimiterConfig
}

func NewRateLimiter(opts ...RateLimiterConfigOpt) RateLimiter {
	cfg := defaultRateLimiterConfig()
	cfg.apply(opts)
	return &rateLimiterImpl{config: cfg, gate: make(chan struct{}, 1), closedSignal: make(chan struct{})}
}
func (l *rateLimiterImpl) Close(context.Context) {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.closedSignal)
	}
	l.mu.Unlock()
}
func (l *rateLimiterImpl) Reset() {
	l.mu.Lock()
	if !l.closed {
		close(l.closedSignal)
	}
	l.closed = false
	l.closedSignal = make(chan struct{})
	l.reset = time.Time{}
	l.remaining = 0
	l.mu.Unlock()
}
func (l *rateLimiterImpl) Wait(ctx context.Context, kind RateLimiterCommandType) error {
	for {
		l.mu.Lock()
		signal := l.closedSignal
		l.mu.Unlock()
		select {
		case <-signal:
			return context.Canceled
		case <-ctx.Done():
			return ctx.Err()
		case l.gate <- struct{}{}:
		}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			l.Unlock()
			return context.Canceled
		}
		now := time.Now()
		if !now.Before(l.reset) {
			l.reset = now.Add(time.Minute)
			l.remaining = l.config.CommandsPerMinute
		}
		limited := l.remaining <= 0 || (l.remaining < l.config.ReservedCommandSlots && kind != InternalCommandType)
		delay := time.Until(l.reset)
		if !limited {
			l.remaining--
		}
		l.mu.Unlock()
		if !limited {
			return nil
		}
		l.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-signal:
			timer.Stop()
			return context.Canceled
		case <-timer.C:
		}

	}
}
func (l *rateLimiterImpl) Unlock() { <-l.gate }
