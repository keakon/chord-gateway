package main

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/keakon/golog/log"
)

const queueMetricsLogInterval = 5 * time.Minute

type queueMetrics struct {
	enqueued  atomic.Uint64
	processed atomic.Uint64
	full      atomic.Uint64
	closed    atomic.Uint64
	blocked   atomic.Uint64
}

type queueMetricsSnapshot struct {
	Enqueued  uint64
	Processed uint64
	Full      uint64
	Closed    uint64
	Blocked   uint64
	Depth     int
	Capacity  int
}

type queueMetricsProvider interface {
	metricsSnapshot() queueMetricsSnapshot
}

func (m *queueMetrics) snapshot(depth, capacity int) queueMetricsSnapshot {
	if m == nil {
		return queueMetricsSnapshot{Depth: depth, Capacity: capacity}
	}
	return queueMetricsSnapshot{
		Enqueued:  m.enqueued.Load(),
		Processed: m.processed.Load(),
		Full:      m.full.Load(),
		Closed:    m.closed.Load(),
		Blocked:   m.blocked.Load(),
		Depth:     depth,
		Capacity:  capacity,
	}
}

func (s *queueMetricsSnapshot) add(other queueMetricsSnapshot) {
	s.Enqueued += other.Enqueued
	s.Processed += other.Processed
	s.Full += other.Full
	s.Closed += other.Closed
	s.Blocked += other.Blocked
	s.Depth += other.Depth
	s.Capacity += other.Capacity
}

func (s queueMetricsSnapshot) active() bool {
	return s.Enqueued != 0 || s.Processed != 0 || s.Full != 0 || s.Closed != 0 || s.Blocked != 0 || s.Depth != 0
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
		Enqueued:  s.Enqueued - previous.Enqueued,
		Processed: s.Processed - previous.Processed,
		Full:      s.Full - previous.Full,
		Closed:    s.Closed - previous.Closed,
		Blocked:   s.Blocked - previous.Blocked,
	}
}

func logQueueMetricsSnapshot(logf func(string, ...any), queue string, snapshot, delta queueMetricsSnapshot) {
	logf("performance queue=%v enqueued=%v enqueued_delta=%v processed=%v processed_delta=%v depth=%v capacity=%v full=%v full_delta=%v closed=%v closed_delta=%v blocked=%v blocked_delta=%v",
		queue,
		snapshot.Enqueued,
		delta.Enqueued,
		snapshot.Processed,
		delta.Processed,
		snapshot.Depth,
		snapshot.Capacity,
		snapshot.Full,
		delta.Full,
		snapshot.Closed,
		delta.Closed,
		snapshot.Blocked,
		delta.Blocked,
	)
}
