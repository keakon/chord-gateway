package main

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/keakon/golog/log"
)

const queueMetricsLogInterval = 5 * time.Minute

type queueMetrics struct {
	enqueued     atomic.Uint64
	processed    atomic.Uint64
	full         atomic.Uint64
	closed       atomic.Uint64
	syncFallback atomic.Uint64
}

type queueMetricsSnapshot struct {
	Enqueued     uint64
	Processed    uint64
	Full         uint64
	Closed       uint64
	SyncFallback uint64
	Depth        int
	Capacity     int
}

type queueMetricsProvider interface {
	metricsSnapshot() queueMetricsSnapshot
}

func (m *queueMetrics) snapshot(depth, capacity int) queueMetricsSnapshot {
	if m == nil {
		return queueMetricsSnapshot{Depth: depth, Capacity: capacity}
	}
	return queueMetricsSnapshot{
		Enqueued:     m.enqueued.Load(),
		Processed:    m.processed.Load(),
		Full:         m.full.Load(),
		Closed:       m.closed.Load(),
		SyncFallback: m.syncFallback.Load(),
		Depth:        depth,
		Capacity:     capacity,
	}
}

func (s *queueMetricsSnapshot) add(other queueMetricsSnapshot) {
	s.Enqueued += other.Enqueued
	s.Processed += other.Processed
	s.Full += other.Full
	s.Closed += other.Closed
	s.SyncFallback += other.SyncFallback
	s.Depth += other.Depth
	s.Capacity += other.Capacity
}

func (s queueMetricsSnapshot) active() bool {
	return s.Enqueued != 0 || s.Processed != 0 || s.Full != 0 || s.Closed != 0 || s.SyncFallback != 0 || s.Depth != 0
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
	for {
		select {
		case <-ticker.C:
			logQueueMetrics(router, adapter, logf)
		case <-ctx.Done():
			logQueueMetrics(router, adapter, logf)
			return
		}
	}
}

func logQueueMetrics(router *NotificationRouter, adapter IMAdapter, logf func(string, ...any)) {
	if router != nil && router.outbound != nil {
		snapshot := router.outbound.metricsSnapshot()
		if snapshot.active() {
			logQueueMetricsSnapshot(logf, "outbound", snapshot)
		}
	}
	var inbound queueMetricsSnapshot
	collectFeishuQueueMetrics(adapter, &inbound)
	if inbound.active() {
		logQueueMetricsSnapshot(logf, "feishu_inbound", inbound)
	}
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

func logQueueMetricsSnapshot(logf func(string, ...any), queue string, snapshot queueMetricsSnapshot) {
	logf("performance queue=%v enqueued=%v processed=%v depth=%v capacity=%v full=%v closed=%v sync_fallback=%v",
		queue,
		snapshot.Enqueued,
		snapshot.Processed,
		snapshot.Depth,
		snapshot.Capacity,
		snapshot.Full,
		snapshot.Closed,
		snapshot.SyncFallback,
	)
}
