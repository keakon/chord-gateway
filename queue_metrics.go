package main

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/keakon/golog/log"
)

const (
	queueMetricsLogInterval = 5 * time.Minute

	// blockedWaitSlowThreshold marks an enqueue wait long enough to matter for
	// user-visible notifications; slow waits are counted separately so a
	// provider stall stays visible even when the total wait looks harmless.
	blockedWaitSlowThreshold = time.Second
)

type queueMetrics struct {
	enqueued  atomic.Uint64
	processed atomic.Uint64
	full      atomic.Uint64
	closed    atomic.Uint64
	blocked   atomic.Uint64

	blockedWaitNanos atomic.Uint64
	blockedWaitMax   atomic.Uint64
	blockedSlow      atomic.Uint64
}

// recordBlockedWait records how long a blocked enqueue waited for queue
// capacity. It must only be called for enqueues that actually blocked.
func (m *queueMetrics) recordBlockedWait(wait time.Duration) {
	if m == nil || wait <= 0 {
		return
	}
	nanos := uint64(wait)
	m.blockedWaitNanos.Add(nanos)
	if wait >= blockedWaitSlowThreshold {
		m.blockedSlow.Add(1)
	}
	for {
		previous := m.blockedWaitMax.Load()
		if nanos <= previous || m.blockedWaitMax.CompareAndSwap(previous, nanos) {
			return
		}
	}
}

type queueMetricsSnapshot struct {
	Enqueued  uint64
	Processed uint64
	Full      uint64
	Closed    uint64
	Blocked   uint64

	// BlockedWaitTotal and BlockedWaitMax aggregate how long blocked enqueues
	// waited for queue capacity; BlockedSlow counts waits at or above
	// blockedWaitSlowThreshold.
	BlockedWaitTotal time.Duration
	BlockedWaitMax   time.Duration
	BlockedSlow      uint64
	Depth            int
	Capacity         int

	// MaxDepth is the deepest single queue at sample time, which shows whether
	// one shard carries the backlog while the aggregate depth looks moderate.
	MaxDepth int
}

type queueMetricsProvider interface {
	metricsSnapshot() queueMetricsSnapshot
}

func (m *queueMetrics) snapshot(depth, maxDepth, capacity int) queueMetricsSnapshot {
	if m == nil {
		return queueMetricsSnapshot{Depth: depth, MaxDepth: maxDepth, Capacity: capacity}
	}
	return queueMetricsSnapshot{
		Enqueued:         m.enqueued.Load(),
		Processed:        m.processed.Load(),
		Full:             m.full.Load(),
		Closed:           m.closed.Load(),
		Blocked:          m.blocked.Load(),
		BlockedWaitTotal: time.Duration(m.blockedWaitNanos.Load()),
		BlockedWaitMax:   time.Duration(m.blockedWaitMax.Load()),
		BlockedSlow:      m.blockedSlow.Load(),
		Depth:            depth,
		Capacity:         capacity,
		MaxDepth:         maxDepth,
	}
}

func (s *queueMetricsSnapshot) add(other queueMetricsSnapshot) {
	s.Enqueued += other.Enqueued
	s.Processed += other.Processed
	s.Full += other.Full
	s.Closed += other.Closed
	s.Blocked += other.Blocked
	s.BlockedWaitTotal += other.BlockedWaitTotal
	if other.BlockedWaitMax > s.BlockedWaitMax {
		s.BlockedWaitMax = other.BlockedWaitMax
	}
	s.BlockedSlow += other.BlockedSlow
	s.Depth += other.Depth
	s.Capacity += other.Capacity
	if other.MaxDepth > s.MaxDepth {
		s.MaxDepth = other.MaxDepth
	}
}

func (s queueMetricsSnapshot) active() bool {
	return s.Enqueued != 0 || s.Processed != 0 || s.Full != 0 || s.Closed != 0 || s.Blocked != 0 ||
		s.BlockedWaitTotal != 0 || s.BlockedSlow != 0 || s.Depth != 0
}

func reportQueueMetrics(ctx context.Context, router *NotificationRouter, adapter IMAdapter, interval time.Duration, logf func(string, ...any)) {
	if interval <= 0 {
		interval = queueMetricsLogInterval
	}
	if logf == nil {
		logf = log.Infof
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var previousOutbound queueMetricsSnapshot
	var previousInbound queueMetricsSnapshot
	for {
		select {
		case <-ticker.C:
			logQueueMetrics(router, adapter, logf, &previousOutbound, &previousInbound)
		case <-ctx.Done():
			logQueueMetrics(router, adapter, logf, &previousOutbound, &previousInbound)
			return
		}
	}
}

func logQueueMetrics(router *NotificationRouter, adapter IMAdapter, logf func(string, ...any), previousOutbound, previousInbound *queueMetricsSnapshot) {
	if router != nil && router.outbound != nil {
		snapshot := router.outbound.metricsSnapshot()
		if snapshot.active() {
			logQueueMetricsSnapshot(logf, "outbound", snapshot, snapshot.delta(*previousOutbound))
		}
		*previousOutbound = snapshot
	}
	var inbound queueMetricsSnapshot
	collectFeishuQueueMetrics(adapter, &inbound)
	if inbound.active() {
		logQueueMetricsSnapshot(logf, "feishu_inbound", inbound, inbound.delta(*previousInbound))
	}
	*previousInbound = inbound
}

func collectFeishuQueueMetrics(adapter IMAdapter, total *queueMetricsSnapshot) {
	switch a := adapter.(type) {
	case interface {
		IMAdapter
		queueMetricsProvider
	}:
		if a.Type() == "feishu" {
			total.add(a.metricsSnapshot())
		}
	case *MultiAdapter:
		for _, child := range a.adapters {
			collectFeishuQueueMetrics(child, total)
		}
	}
}

func (s queueMetricsSnapshot) delta(previous queueMetricsSnapshot) queueMetricsSnapshot {
	return queueMetricsSnapshot{
		Enqueued:         s.Enqueued - previous.Enqueued,
		Processed:        s.Processed - previous.Processed,
		Full:             s.Full - previous.Full,
		Closed:           s.Closed - previous.Closed,
		Blocked:          s.Blocked - previous.Blocked,
		BlockedWaitTotal: s.BlockedWaitTotal - previous.BlockedWaitTotal,
		BlockedSlow:      s.BlockedSlow - previous.BlockedSlow,
		// BlockedWaitMax and MaxDepth are gauges: the metrics line prints the
		// current snapshot values instead of deltas.
	}
}

func logQueueMetricsSnapshot(logf func(string, ...any), queue string, snapshot, delta queueMetricsSnapshot) {
	logf("performance queue=%v enqueued=%v enqueued_delta=%v processed=%v processed_delta=%v depth=%v max_depth=%v capacity=%v full=%v full_delta=%v closed=%v closed_delta=%v blocked=%v blocked_delta=%v blocked_wait_total=%v blocked_wait_total_delta=%v blocked_wait_max=%v blocked_slow=%v blocked_slow_delta=%v",
		queue,
		snapshot.Enqueued,
		delta.Enqueued,
		snapshot.Processed,
		delta.Processed,
		snapshot.Depth,
		snapshot.MaxDepth,
		snapshot.Capacity,
		snapshot.Full,
		delta.Full,
		snapshot.Closed,
		delta.Closed,
		snapshot.Blocked,
		delta.Blocked,
		snapshot.BlockedWaitTotal,
		delta.BlockedWaitTotal,
		snapshot.BlockedWaitMax,
		snapshot.BlockedSlow,
		delta.BlockedSlow,
	)
}
