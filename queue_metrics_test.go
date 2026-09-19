package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type metricsTestAdapter struct {
	typeName string
	snapshot queueMetricsSnapshot
}

func (a *metricsTestAdapter) Connect() error                        { return nil }
func (a *metricsTestAdapter) SendText(string, string) error         { return nil }
func (a *metricsTestAdapter) Disconnect()                           {}
func (a *metricsTestAdapter) Type() string                          { return a.typeName }
func (a *metricsTestAdapter) SupportsLoginRenewal() bool            { return false }
func (a *metricsTestAdapter) StartLogin() (string, error)           { return "", ErrLoginNotSupported }
func (a *metricsTestAdapter) metricsSnapshot() queueMetricsSnapshot { return a.snapshot }

func TestQueueMetricsSnapshot(t *testing.T) {
	var metrics queueMetrics
	metrics.enqueued.Add(3)
	metrics.processed.Add(2)
	metrics.full.Add(1)
	metrics.closed.Add(4)
	metrics.blocked.Add(1)
	metrics.recordBlockedWait(0)
	metrics.recordBlockedWait(10 * time.Millisecond)
	metrics.recordBlockedWait(blockedWaitSlowThreshold + time.Millisecond)
	got := metrics.snapshot(5, 7, 256)
	want := queueMetricsSnapshot{
		Enqueued:         3,
		Processed:        2,
		Full:             1,
		Closed:           4,
		Blocked:          1,
		BlockedWaitTotal: blockedWaitSlowThreshold + 11*time.Millisecond,
		BlockedWaitMax:   blockedWaitSlowThreshold + time.Millisecond,
		BlockedSlow:      1,
		Depth:            5,
		MaxDepth:         7,
		Capacity:         256,
	}
	if got != want {
		t.Fatalf("snapshot = %#v, want %#v", got, want)
	}
}

func TestQueueMetricsSnapshotDelta(t *testing.T) {
	previous := queueMetricsSnapshot{Enqueued: 10, Processed: 8, Full: 1, Closed: 2, Blocked: 1, BlockedWaitTotal: 2 * time.Second, BlockedWaitMax: 3 * time.Second, BlockedSlow: 1, Depth: 4, MaxDepth: 9}
	current := queueMetricsSnapshot{Enqueued: 15, Processed: 12, Full: 3, Closed: 2, Blocked: 2, BlockedWaitTotal: 5*time.Second + 500*time.Millisecond, BlockedWaitMax: 6 * time.Second, BlockedSlow: 3, Depth: 6, MaxDepth: 12}
	want := queueMetricsSnapshot{
		Enqueued:         5,
		Processed:        4,
		Full:             2,
		Blocked:          1,
		BlockedWaitTotal: 3*time.Second + 500*time.Millisecond,
		BlockedSlow:      2,
	}
	if got := current.delta(previous); got != want {
		t.Fatalf("delta = %#v, want %#v", got, want)
	}
}

func TestQueueMetricsActiveTracksBlockedWaits(t *testing.T) {
	if (queueMetricsSnapshot{}).active() {
		t.Fatal("zero snapshot must be inactive")
	}
	if !(queueMetricsSnapshot{BlockedSlow: 1}).active() {
		t.Fatal("slow wait must keep the queue reported as active")
	}
}

func TestCollectFeishuQueueMetricsAggregatesMultiAdapter(t *testing.T) {
	first := &metricsTestAdapter{typeName: "feishu", snapshot: queueMetricsSnapshot{Enqueued: 2, Processed: 1, Depth: 1, MaxDepth: 3, Capacity: 8, BlockedWaitTotal: 10 * time.Millisecond, BlockedWaitMax: 5 * time.Millisecond, BlockedSlow: 1}}
	second := &metricsTestAdapter{typeName: "feishu", snapshot: queueMetricsSnapshot{Enqueued: 3, Processed: 3, Full: 1, MaxDepth: 5, Capacity: 8, BlockedWaitTotal: 20 * time.Millisecond, BlockedWaitMax: 25 * time.Millisecond}}
	wechat := &metricsTestAdapter{typeName: "wechat", snapshot: queueMetricsSnapshot{Enqueued: 100, Processed: 100, Capacity: 8, MaxDepth: 99}}
	multi := &MultiAdapter{adapters: []IMAdapter{first, second, wechat}}
	var got queueMetricsSnapshot
	collectFeishuQueueMetrics(multi, &got)
	want := queueMetricsSnapshot{
		Enqueued:         5,
		Processed:        4,
		Full:             1,
		BlockedWaitTotal: 30 * time.Millisecond,
		BlockedWaitMax:   25 * time.Millisecond,
		BlockedSlow:      1,
		Depth:            1,
		MaxDepth:         5,
		Capacity:         16,
	}
	if got != want {
		t.Fatalf("aggregate = %#v, want %#v", got, want)
	}
}

func TestReportQueueMetricsLogsFinalSnapshotOnCancel(t *testing.T) {
	adapter := &metricsTestAdapter{typeName: "feishu", snapshot: queueMetricsSnapshot{Enqueued: 2, Processed: 2, Capacity: 8}}
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var logs []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		reportQueueMetrics(ctx, nil, adapter, time.Hour, func(format string, args ...any) {
			mu.Lock()
			logs = append(logs, fmt.Sprintf(format, args...))
			mu.Unlock()
		})
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("metrics reporter did not stop")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 1 || !strings.Contains(logs[0], "queue=feishu_inbound") || !strings.Contains(logs[0], "enqueued=2") {
		t.Fatalf("final logs = %v", logs)
	}
}

func BenchmarkQueueMetricsHotPath(b *testing.B) {
	var metrics queueMetrics
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		metrics.enqueued.Add(1)
	}
}
