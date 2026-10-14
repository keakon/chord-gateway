package main

import (
	"context"
	"sync"
	"time"
)

// One adapter represents one workspace installation. The channel key deliberately
// omits thread_ts, and the method block covers every channel in that workspace.
type slackSendLimits struct {
	mu              sync.Mutex
	channels        map[string]time.Time
	accountNext     time.Time
	channelInterval time.Duration
	accountInterval time.Duration
}

func newSlackSendLimits() *slackSendLimits {
	return &slackSendLimits{
		channels: make(map[string]time.Time), channelInterval: time.Second,
		accountInterval: time.Second / 5,
	}
}

func (l *slackSendLimits) wait(ctx context.Context, channel string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.mu.Lock()
		now := time.Now()
		next := l.channels[channel]
		if l.accountNext.After(next) {
			next = l.accountNext
		}
		if !next.After(now) {
			// Remove expired buckets so historical bound channels do not leak.
			for key, available := range l.channels {
				if !available.After(now) {
					delete(l.channels, key)
				}
			}
			l.channels[channel] = now.Add(l.channelInterval)
			l.accountNext = now.Add(l.accountInterval)
			l.mu.Unlock()
			return nil
		}
		l.mu.Unlock()
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			// Recheck under the mutex: another thread may have observed a 429.
		}
	}
}

// Owned by the installation's HTTP client, so lookups and reconnects share
// Retry-After deadlines. Only explicit 429 responses extend a method's block;
// this layer never retries requests, especially ambiguous message POSTs.
type slackMethodLimits struct {
	mu      sync.Mutex
	blocked map[string]time.Time
}

func (l *slackMethodLimits) block(method string, delay time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	until := time.Now().Add(delay)
	if until.After(l.blocked[method]) {
		l.blocked[method] = until
	}
}

func (l *slackMethodLimits) wait(ctx context.Context, method string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.mu.Lock()
		until := l.blocked[method]
		if !until.After(time.Now()) {
			delete(l.blocked, method)
			l.mu.Unlock()
			return nil
		}
		l.mu.Unlock()
		timer := time.NewTimer(time.Until(until))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			// Another in-flight request may have extended the deadline.
		}
	}
}
